package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"if3110.local/db-platform/internal/contracts"
)

const PageSize = 50
const maxCellRunes = 1024

// DatabaseChoice deliberately omits connection credentials and server addresses.
type DatabaseChoice struct {
	ID    contracts.DatabaseID
	Name  string
	Level contracts.AccessLevel
}
type Table struct{ Name, Kind string }
type Column struct {
	Name, Type, Default string
	Nullable            bool
	typeOID             uint32
}
type Definition struct{ Name, SQL string }
type TableDetails struct {
	Schema, Name         string
	Columns              []Column
	Constraints, Indexes []Definition
}
type Cell struct {
	Text            string
	Null, Truncated bool
}
type DataPage struct {
	Columns []string
	Rows    [][]Cell
	Page    int
	HasNext bool
}

// Accessible lists only databases with a schema-view grant; admin status adds no grant.
func (m *Manager) Accessible(ctx context.Context, actor contracts.UserID) ([]DatabaseChoice, error) {
	if actor == "" {
		return nil, contracts.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	items, err := m.store.list(ctx)
	m.mu.Unlock()
	if err != nil {
		return nil, storageError(err)
	}
	result := make([]DatabaseChoice, 0)
	for _, c := range items {
		err := m.authorizer.RequireDatabaseAccess(ctx, actor, c.ID, contracts.ActionViewSchema)
		if errors.Is(err, contracts.ErrForbidden) {
			continue
		}
		if err != nil {
			return nil, storageError(err)
		}
		level, err := m.displayAccess(ctx, actor, c.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, DatabaseChoice{ID: c.ID, Name: c.Name, Level: level})
	}
	return result, nil
}

// displayAccess is only called after a successful read-access check. It asks the
// shared authorizer for the editing capability; it never calculates group grants.
func (m *Manager) displayAccess(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID) (contracts.AccessLevel, error) {
	err := m.authorizer.RequireDatabaseAccess(ctx, actor, id, contracts.ActionCreateMigration)
	if errors.Is(err, contracts.ErrForbidden) {
		return contracts.AccessViewOnly, nil
	}
	if err != nil {
		return contracts.AccessNone, storageError(err)
	}
	return contracts.AccessViewEdit, nil
}

// describe exposes only the authorized target's display name and capabilities.
// Unlike Get (admin-only), it is suitable for a member's viewer breadcrumbs.
func (m *Manager) describe(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, action contracts.DatabaseAction) (DatabaseChoice, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if actor == "" {
		return DatabaseChoice{}, contracts.ErrForbidden
	}
	if !validID(id) {
		return DatabaseChoice{}, ErrInvalidInput
	}
	if err := m.authorizer.RequireDatabaseAccess(ctx, actor, id, action); err != nil {
		if errors.Is(err, contracts.ErrForbidden) {
			return DatabaseChoice{}, contracts.ErrForbidden
		}
		return DatabaseChoice{}, storageError(err)
	}
	level, err := m.displayAccess(ctx, actor, id)
	if err != nil {
		return DatabaseChoice{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return DatabaseChoice{}, ErrClosed
	}
	r, err := m.store.get(ctx, id)
	if err != nil {
		return DatabaseChoice{}, storageError(err)
	}
	return DatabaseChoice{ID: id, Name: r.Name, Level: level}, nil
}

type Viewer struct {
	authorizer contracts.Authorizer
	provider   contracts.DatabaseProvider
}

func NewViewer(authorizer contracts.Authorizer, provider contracts.DatabaseProvider) (*Viewer, error) {
	if authorizer == nil || provider == nil {
		return nil, ErrInvalidConfig
	}
	return &Viewer{authorizer: authorizer, provider: provider}, nil
}

func (v *Viewer) pool(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, action contracts.DatabaseAction) (*sql.DB, error) {
	if actor == "" {
		return nil, contracts.ErrForbidden
	}
	if !validID(id) {
		return nil, ErrInvalidInput
	}
	if err := v.authorizer.RequireDatabaseAccess(ctx, actor, id, action); err != nil {
		if errors.Is(err, contracts.ErrForbidden) {
			return nil, contracts.ErrForbidden
		}
		return nil, storageError(err)
	}
	db, err := v.provider.Pool(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, connectionError(ctx)
	}
	return db, nil
}

func validObject(s string) bool {
	return s != "" && len(s) <= 63 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func visibleSchema(s string) bool {
	return validObject(s) && !strings.HasPrefix(s, "pg_") && s != "information_schema" && s != "if3110_platform"
}

func (v *Viewer) Schemas(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	db, err := v.pool(ctx, actor, id, contracts.ActionViewSchema)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT nspname FROM pg_catalog.pg_namespace
		WHERE left(nspname,3) <> 'pg_' AND nspname NOT IN ('information_schema','if3110_platform')
		AND pg_catalog.has_schema_privilege(oid,'USAGE') ORDER BY nspname`)
	if err != nil {
		return nil, viewerError(err)
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, viewerError(err)
		}
		result = append(result, name)
	}
	return result, viewerError(rows.Err())
}

func (v *Viewer) Tables(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, schema string) ([]Table, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	db, err := v.pool(ctx, actor, id, contracts.ActionViewSchema)
	if err != nil {
		return nil, err
	}
	if !visibleSchema(schema) {
		return nil, ErrInvalidInput
	}
	var exists bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname=$1 AND pg_catalog.has_schema_privilege(oid,'USAGE'))`, schema).Scan(&exists)
	if err != nil {
		return nil, viewerError(err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := db.QueryContext(ctx, `SELECT c.relname, CASE c.relkind WHEN 'v' THEN 'View' WHEN 'm' THEN 'Materialized view' WHEN 'p' THEN 'Partitioned table' ELSE 'Table' END
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relkind IN ('r','p','v','m') ORDER BY c.relname`, schema)
	if err != nil {
		return nil, viewerError(err)
	}
	defer rows.Close()
	result := make([]Table, 0)
	for rows.Next() {
		var table Table
		if err := rows.Scan(&table.Name, &table.Kind); err != nil {
			return nil, viewerError(err)
		}
		result = append(result, table)
	}
	return result, viewerError(rows.Err())
}

func relation(ctx context.Context, tx *sql.Tx, schema, table string) (uint32, string, error) {
	if !visibleSchema(schema) || !validObject(table) {
		return 0, "", ErrInvalidInput
	}
	var oid uint32
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT c.oid,c.relkind FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname=$2 AND c.relkind IN ('r','p','v','m') AND pg_catalog.has_schema_privilege(n.oid,'USAGE')`, schema, table).Scan(&oid, &kind)
	return oid, kind, viewerError(err)
}

func columns(ctx context.Context, tx *sql.Tx, oid uint32) ([]Column, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.attname, pg_catalog.format_type(a.atttypid,a.atttypmod), NOT a.attnotnull,
		COALESCE(pg_catalog.pg_get_expr(d.adbin,d.adrelid),''),a.atttypid
		FROM pg_catalog.pg_attribute a LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
		WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, oid)
	if err != nil {
		return nil, viewerError(err)
	}
	defer rows.Close()
	result := make([]Column, 0)
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &c.Default, &c.typeOID); err != nil {
			return nil, viewerError(err)
		}
		result = append(result, c)
	}
	return result, viewerError(rows.Err())
}

func definitions(ctx context.Context, tx *sql.Tx, query string, oid uint32) ([]Definition, error) {
	rows, err := tx.QueryContext(ctx, query, oid)
	if err != nil {
		return nil, viewerError(err)
	}
	defer rows.Close()
	result := make([]Definition, 0)
	for rows.Next() {
		var d Definition
		if err := rows.Scan(&d.Name, &d.SQL); err != nil {
			return nil, viewerError(err)
		}
		result = append(result, d)
	}
	return result, viewerError(rows.Err())
}

func (v *Viewer) Details(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, schema, table string) (TableDetails, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	db, err := v.pool(ctx, actor, id, contracts.ActionViewSchema)
	if err != nil {
		return TableDetails{}, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TableDetails{}, viewerError(err)
	}
	defer tx.Rollback()
	oid, _, err := relation(ctx, tx, schema, table)
	if err != nil {
		return TableDetails{}, err
	}
	d := TableDetails{Schema: schema, Name: table}
	if d.Columns, err = columns(ctx, tx, oid); err != nil {
		return TableDetails{}, err
	}
	if d.Constraints, err = definitions(ctx, tx, `SELECT conname,pg_catalog.pg_get_constraintdef(oid,true) FROM pg_catalog.pg_constraint WHERE conrelid=$1 ORDER BY conname`, oid); err != nil {
		return TableDetails{}, err
	}
	if d.Indexes, err = definitions(ctx, tx, `SELECT c.relname,pg_catalog.pg_get_indexdef(i.indexrelid) FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid=i.indexrelid WHERE i.indrelid=$1 ORDER BY c.relname`, oid); err != nil {
		return TableDetails{}, err
	}
	if err := tx.Commit(); err != nil {
		return TableDetails{}, viewerError(err)
	}
	return d, nil
}

func rowOrder(ctx context.Context, tx *sql.Tx, oid uint32, kind string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.attname FROM pg_catalog.pg_index i
		CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum,position)
		JOIN pg_catalog.pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum
		WHERE i.indrelid=$1 AND i.indisprimary AND k.position<=i.indnkeyatts ORDER BY k.position`, oid)
	if err != nil {
		return "", viewerError(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", viewerError(err)
		}
		names = append(names, pgx.Identifier{name}.Sanitize())
	}
	if err := rows.Err(); err != nil {
		return "", viewerError(err)
	}
	if len(names) > 0 {
		return " ORDER BY " + strings.Join(names, ","), nil
	}
	if kind == "r" || kind == "p" || kind == "m" {
		return " ORDER BY tableoid,ctid", nil
	}
	return "", nil // Views have no intrinsic stable row order.
}

func (v *Viewer) Data(ctx context.Context, actor contracts.UserID, id contracts.DatabaseID, schema, table string, page int) (DataPage, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	db, err := v.pool(ctx, actor, id, contracts.ActionViewData)
	if err != nil {
		return DataPage{}, err
	}
	if page < 1 || page > 1000000 {
		return DataPage{}, ErrInvalidInput
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DataPage{}, viewerError(err)
	}
	defer tx.Rollback()
	oid, kind, err := relation(ctx, tx, schema, table)
	if err != nil {
		return DataPage{}, err
	}
	cols, err := columns(ctx, tx, oid)
	if err != nil {
		return DataPage{}, err
	}
	order, err := rowOrder(ctx, tx, oid, kind)
	if err != nil {
		return DataPage{}, err
	}
	result := DataPage{Page: page, Columns: make([]string, 0, len(cols)), Rows: make([][]Cell, 0)}
	var projections []string
	for _, c := range cols {
		result.Columns = append(result.Columns, c.Name)
		expr := pgx.Identifier{c.Name}.Sanitize() + "::text"
		if c.typeOID == 17 {
			expr = "pg_catalog.encode(" + pgx.Identifier{c.Name}.Sanitize() + ",'hex')"
		}
		projections = append(projections, "pg_catalog.left("+expr+","+strconv.Itoa(maxCellRunes+1)+")")
	}
	// PostgreSQL permits zero-column tables; keep row counting valid there too.
	projection := strings.Join(projections, ",")
	if projection == "" {
		projection = "NULL::text"
	}
	query := "SELECT " + projection + " FROM " + pgx.Identifier{schema, table}.Sanitize() + order + " LIMIT $1 OFFSET $2"
	rows, err := tx.QueryContext(ctx, query, PageSize+1, (page-1)*PageSize)
	if err != nil {
		return DataPage{}, viewerError(err)
	}
	defer rows.Close()
	for rows.Next() {
		values := make([]sql.NullString, len(projections))
		if len(values) == 0 {
			values = make([]sql.NullString, 1)
		}
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return DataPage{}, viewerError(err)
		}
		row := make([]Cell, len(cols))
		for i := range cols {
			row[i].Null = !values[i].Valid
			if values[i].Valid {
				runes := []rune(values[i].String)
				if len(runes) > maxCellRunes {
					row[i].Truncated = true
					runes = runes[:maxCellRunes]
				}
				row[i].Text = string(runes)
				if cols[i].typeOID == 17 {
					row[i].Text = "\\x" + row[i].Text
				}
			}
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return DataPage{}, viewerError(err)
	}
	if err := rows.Close(); err != nil {
		return DataPage{}, viewerError(err)
	}
	if len(result.Rows) > PageSize {
		result.HasNext = page < 1000000
		result.Rows = result.Rows[:PageSize]
	}
	if err := tx.Commit(); err != nil {
		return DataPage{}, viewerError(err)
	}
	return result, nil
}

func viewerError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42501":
			return contracts.ErrForbidden
		case "42P01", "3F000":
			return ErrNotFound
		case "57014":
			return context.DeadlineExceeded
		}
	}
	return storageError(err)
}

func (c Cell) String() string {
	if c.Null {
		return "NULL"
	}
	if c.Truncated {
		return c.Text + "…"
	}
	return c.Text
}
