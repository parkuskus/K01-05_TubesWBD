package database

import (
	"context"
	"database/sql"

	"if3110.local/db-platform/internal/contracts"
)

type SchemaSnapshot struct {
	Schema      string
	Tables      []SchemaTable
	Unsupported []SchemaObject
}

type SchemaTable struct {
	Name, Kind  string
	Columns     []Column
	Constraints []SchemaConstraint
	Indexes     []Definition
}

type SchemaConstraint struct {
	Name, Definition, ReferenceSchema, ReferenceTable string
}

type SchemaObject struct{ Schema, Name, Kind, Table string }

func (v *Viewer) Snapshot(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, schema string) (SchemaSnapshot, error) {
	if !visibleSchema(schema) {
		return SchemaSnapshot{}, ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	db, err := v.pool(ctx, actor, id, contracts.ActionViewSchema)
	if err != nil {
		return SchemaSnapshot{}, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return SchemaSnapshot{}, viewerError(err)
	}
	defer tx.Rollback()
	snapshot, err := readSchemaSnapshot(ctx, tx, schema)
	if err != nil {
		return SchemaSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return SchemaSnapshot{}, viewerError(err)
	}
	return snapshot, nil
}

func (v *Viewer) Diff(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, referenceSchema, targetSchema string) (SchemaDiff, error) {
	if !visibleSchema(referenceSchema) || !visibleSchema(targetSchema) || referenceSchema == targetSchema {
		return SchemaDiff{}, ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	db, err := v.pool(ctx, actor, id, contracts.ActionViewSchema)
	if err != nil {
		return SchemaDiff{}, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return SchemaDiff{}, viewerError(err)
	}
	defer tx.Rollback()
	reference, err := readSchemaSnapshot(ctx, tx, referenceSchema)
	if err != nil {
		return SchemaDiff{}, err
	}
	target, err := readSchemaSnapshot(ctx, tx, targetSchema)
	if err != nil {
		return SchemaDiff{}, err
	}
	diff := CompareSchemas(reference, target)
	if err := tx.Commit(); err != nil {
		return SchemaDiff{}, viewerError(err)
	}
	return diff, nil
}

func readSchemaSnapshot(ctx context.Context, tx *sql.Tx, schema string) (SchemaSnapshot, error) {
	if !visibleSchema(schema) {
		return SchemaSnapshot{}, ErrInvalidInput
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_namespace
		WHERE nspname=$1 AND pg_catalog.has_schema_privilege(oid,'USAGE'))`, schema).Scan(&exists); err != nil {
		return SchemaSnapshot{}, viewerError(err)
	}
	if !exists {
		return SchemaSnapshot{}, ErrNotFound
	}
	var denied bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=$1 AND c.relkind IN ('r','p','v','m','f') AND NOT pg_catalog.has_table_privilege(c.oid,'SELECT')
		) OR EXISTS (
			SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname=$1 AND c.relkind='S'
			AND CASE WHEN c.relkind='S' THEN NOT (
				pg_catalog.has_sequence_privilege(c.oid,'SELECT')
				OR pg_catalog.has_sequence_privilege(c.oid,'USAGE')
				OR pg_catalog.has_sequence_privilege(c.oid,'UPDATE')) ELSE false END
		) OR EXISTS (
			SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
			WHERE n.nspname=$1 AND NOT pg_catalog.has_function_privilege(p.oid,'EXECUTE')
		) OR EXISTS (
			SELECT 1 FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
			WHERE n.nspname=$1 AND t.typrelid=0 AND t.typtype IN ('d','e','r','c')
				AND NOT pg_catalog.has_type_privilege(t.oid,'USAGE')
		)`, schema).Scan(&denied); err != nil {
		return SchemaSnapshot{}, viewerError(err)
	}
	if denied {
		return SchemaSnapshot{}, contracts.ErrForbidden
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.oid,c.relname,c.relkind
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relkind IN ('r','p','v','m','S','f')
		ORDER BY c.relname`, schema)
	if err != nil {
		return SchemaSnapshot{}, viewerError(err)
	}
	type relationInfo struct {
		oid  uint32
		name string
		kind string
	}
	var relations []relationInfo
	for rows.Next() {
		var relation relationInfo
		if err := rows.Scan(&relation.oid, &relation.name, &relation.kind); err != nil {
			_ = rows.Close()
			return SchemaSnapshot{}, viewerError(err)
		}
		relations = append(relations, relation)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return SchemaSnapshot{}, viewerError(err)
	}
	if err := rows.Close(); err != nil {
		return SchemaSnapshot{}, viewerError(err)
	}
	snapshot := SchemaSnapshot{Schema: schema, Tables: make([]SchemaTable, 0), Unsupported: make([]SchemaObject, 0)}
	for _, relation := range relations {
		if relation.kind != "r" && relation.kind != "p" {
			kind := map[string]string{"v": "view", "m": "materialized view", "S": "sequence", "f": "foreign table"}[relation.kind]
			snapshot.Unsupported = append(snapshot.Unsupported, SchemaObject{Schema: schema, Name: relation.name, Kind: kind})
			continue
		}
		table := SchemaTable{Name: relation.name, Kind: "Table"}
		if relation.kind == "p" {
			table.Kind = "Partitioned table"
		}
		if table.Columns, err = columns(ctx, tx, relation.oid); err != nil {
			return SchemaSnapshot{}, err
		}
		constraintRows, queryErr := tx.QueryContext(ctx, `SELECT con.conname,pg_catalog.pg_get_constraintdef(con.oid,true),
			COALESCE(refns.nspname,''),COALESCE(ref.relname,'')
			FROM pg_catalog.pg_constraint con
			LEFT JOIN pg_catalog.pg_class ref ON ref.oid=con.confrelid
			LEFT JOIN pg_catalog.pg_namespace refns ON refns.oid=ref.relnamespace
			WHERE con.conrelid=$1 AND con.contype IN ('p','f','u','c') ORDER BY con.conname`, relation.oid)
		if queryErr != nil {
			return SchemaSnapshot{}, viewerError(queryErr)
		}
		for constraintRows.Next() {
			var constraint SchemaConstraint
			if err := constraintRows.Scan(&constraint.Name, &constraint.Definition, &constraint.ReferenceSchema, &constraint.ReferenceTable); err != nil {
				_ = constraintRows.Close()
				return SchemaSnapshot{}, viewerError(err)
			}
			table.Constraints = append(table.Constraints, constraint)
		}
		if err := constraintRows.Err(); err != nil {
			_ = constraintRows.Close()
			return SchemaSnapshot{}, viewerError(err)
		}
		if err := constraintRows.Close(); err != nil {
			return SchemaSnapshot{}, viewerError(err)
		}
		if table.Indexes, err = definitions(ctx, tx, `SELECT c.relname,pg_catalog.pg_get_indexdef(i.indexrelid)
			FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indexrelid
			WHERE i.indrelid=$1 AND NOT EXISTS (
				SELECT 1 FROM pg_catalog.pg_constraint con WHERE con.conindid=i.indexrelid)
			ORDER BY c.relname`, relation.oid); err != nil {
			return SchemaSnapshot{}, err
		}
		snapshot.Tables = append(snapshot.Tables, table)
	}
	unsupportedRows, err := tx.QueryContext(ctx, `
		SELECT p.proname::text,''::text,'function'
		FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname=$1
		UNION ALL
		SELECT t.tgname::text,c.relname::text,'trigger'
		FROM pg_catalog.pg_trigger t
		JOIN pg_catalog.pg_class c ON c.oid=t.tgrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND NOT t.tgisinternal
		UNION ALL
		SELECT p.polname::text,c.relname::text,'policy'
		FROM pg_catalog.pg_policy p
		JOIN pg_catalog.pg_class c ON c.oid=p.polrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1
		UNION ALL
		SELECT t.typname::text,''::text,'user-defined type'
		FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
		WHERE n.nspname=$1 AND t.typrelid=0 AND t.typtype IN ('d','e','r','c')
		UNION ALL
		SELECT con.conname::text,c.relname::text,'exclusion constraint'
		FROM pg_catalog.pg_constraint con
		JOIN pg_catalog.pg_class c ON c.oid=con.conrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND con.contype='x'
		UNION ALL
		SELECT a.attname::text,c.relname::text,'generated/identity column'
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND a.attnum>0 AND NOT a.attisdropped AND (a.attidentity<>'' OR a.attgenerated<>'')
		UNION ALL
		SELECT a.attname::text,c.relname::text,'sequence-backed column'
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND a.attnum>0 AND NOT a.attisdropped
			AND pg_catalog.pg_get_serial_sequence(pg_catalog.format('%I.%I',n.nspname,c.relname),a.attname) IS NOT NULL
		UNION ALL
		SELECT a.attname::text,c.relname::text,'user-defined column type'
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
		JOIN pg_catalog.pg_namespace relns ON relns.oid=c.relnamespace
		JOIN pg_catalog.pg_type typ ON typ.oid=a.atttypid
		JOIN pg_catalog.pg_namespace typen ON typen.oid=typ.typnamespace
		WHERE relns.nspname=$1 AND a.attnum>0 AND NOT a.attisdropped
			AND typen.nspname NOT IN ('pg_catalog','information_schema') AND typ.typrelid=0
		UNION ALL
		SELECT c.relname::text,c.relname::text,'partition topology'
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relkind IN ('r','p') AND EXISTS (
			SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhrelid=c.oid OR i.inhparent=c.oid)
		ORDER BY 3,1,2`, schema)
	if err != nil {
		return SchemaSnapshot{}, viewerError(err)
	}
	for unsupportedRows.Next() {
		var object SchemaObject
		object.Schema = schema
		if err := unsupportedRows.Scan(&object.Name, &object.Table, &object.Kind); err != nil {
			_ = unsupportedRows.Close()
			return SchemaSnapshot{}, viewerError(err)
		}
		snapshot.Unsupported = append(snapshot.Unsupported, object)
	}
	if err := unsupportedRows.Err(); err != nil {
		_ = unsupportedRows.Close()
		return SchemaSnapshot{}, viewerError(err)
	}
	if err := unsupportedRows.Close(); err != nil {
		return SchemaSnapshot{}, viewerError(err)
	}
	for i := range snapshot.Tables {
		snapshot.Tables[i].Columns = supportedColumns(snapshot.Tables[i].Name, snapshot.Tables[i].Columns, snapshot.Unsupported)
	}
	return snapshot, nil
}

func supportedColumns(table string, columns []Column, unsupported []SchemaObject) []Column {
	result := make([]Column, 0, len(columns))
	for _, column := range columns {
		unsupportedColumn := false
		for _, object := range unsupported {
			if object.Table == table && object.Name == column.Name && (object.Kind == "generated/identity column" || object.Kind == "user-defined column type" || object.Kind == "sequence-backed column") {
				unsupportedColumn = true
				break
			}
		}
		if !unsupportedColumn {
			result = append(result, column)
		}
	}
	return result
}
