package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"if3110.local/db-platform/internal/contracts"
)

func TestPostgresSchemaSnapshotIncludesTablesAndMarksUnsupportedObjects(t *testing.T) {
	target, cfg, _ := integrationDatabase(t, "TARGET_TEST_DATABASE_URL")
	platform, _, _ := integrationDatabase(t, "PLATFORM_TEST_DATABASE_URL")
	manager := integrationManager(t, platform)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "diff_desired_" + hex.EncodeToString(suffix[:])
	targetSchema := schema + "_target"
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	quotedTargetSchema := pgx.Identifier{targetSchema}.Sanitize()
	if _, err := target.ExecContext(ctx, "CREATE SCHEMA "+quotedSchema+"; CREATE SCHEMA "+quotedTargetSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := target.ExecContext(context.Background(), "DROP SCHEMA "+quotedSchema+", "+quotedTargetSchema+" CASCADE"); err != nil {
			t.Error("could not clean up schema diff fixture")
		}
	})
	parent := pgx.Identifier{schema, "parent"}.Sanitize()
	items := pgx.Identifier{schema, "items"}.Sanitize()
	view := pgx.Identifier{schema, "item_names"}.Sanitize()
	function := pgx.Identifier{schema, "touch"}.Sanitize()
	_, err := target.ExecContext(ctx, "CREATE TABLE "+parent+` (id bigint PRIMARY KEY);
		CREATE TABLE `+items+` (id bigint PRIMARY KEY, name text NOT NULL DEFAULT 'new', parent_id bigint REFERENCES `+parent+`(id),
			CONSTRAINT items_excl EXCLUDE USING btree (id WITH =));
		CREATE INDEX items_name_idx ON `+items+`(name);
		CREATE VIEW `+view+` AS SELECT name FROM `+items+`;
		CREATE FUNCTION `+function+` () RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
		CREATE TRIGGER items_touch BEFORE INSERT ON `+items+` FOR EACH ROW EXECUTE FUNCTION `+function+`();
		ALTER TABLE `+items+` ENABLE ROW LEVEL SECURITY;
		CREATE POLICY items_policy ON `+items+` USING (true);
		CREATE TYPE `+pgx.Identifier{schema, "mood"}.Sanitize()+` AS ENUM ('new','old');
		CREATE TABLE `+pgx.Identifier{schema, "generated"}.Sanitize()+` (id bigint GENERATED ALWAYS AS IDENTITY, total bigint GENERATED ALWAYS AS (id + 1) STORED);
		CREATE TABLE `+pgx.Identifier{schema, "serial_items"}.Sanitize()+` (id bigserial PRIMARY KEY);
		CREATE TABLE `+pgx.Identifier{schema, "partitioned"}.Sanitize()+` (id integer) PARTITION BY RANGE (id);
		CREATE TABLE `+pgx.Identifier{schema, "partitioned_child"}.Sanitize()+` PARTITION OF `+pgx.Identifier{schema, "partitioned"}.Sanitize()+` FOR VALUES FROM (0) TO (10)`)
	if err != nil {
		t.Fatal(err)
	}
	input := validInput()
	input.Host, input.Port, input.Database, input.Username = cfg.Host, int(cfg.Port), cfg.Database, cfg.User
	input.Password = &cfg.Password
	if cfg.TLSConfig != nil {
		input.SSLMode = "verify-full"
		if cfg.TLSConfig.InsecureSkipVerify {
			input.SSLMode = "require"
		}
	}
	connection, err := manager.Create(ctx, "admin", input)
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &grantAuthorizer{grants: map[contracts.UserID]map[databaseID]contracts.AccessLevel{
		"viewer": {connection.ID: contracts.AccessViewOnly},
	}}
	viewer, err := NewViewer(authorizer, manager)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := viewer.Snapshot(ctx, "viewer", connection.ID, schema)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Schema != schema || len(snapshot.Tables) != 6 {
		t.Fatalf("snapshot identity/tables = %q/%d, want %s/6", snapshot.Schema, len(snapshot.Tables), schema)
	}
	var item *SchemaTable
	for i := range snapshot.Tables {
		if snapshot.Tables[i].Name == "items" {
			item = &snapshot.Tables[i]
		}
	}
	if item == nil || len(item.Columns) != 3 || len(item.Constraints) != 2 || len(item.Indexes) != 1 {
		t.Fatalf("items snapshot = %#v, want 3 columns, 2 constraints and 1 index", item)
	}
	if item.Columns[1].Name != "name" || item.Columns[1].Nullable || item.Columns[1].Default != "'new'::text" {
		t.Fatalf("name column = %#v", item.Columns[1])
	}
	unsupportedKinds := make(map[string]bool)
	for _, object := range snapshot.Unsupported {
		unsupportedKinds[object.Kind] = true
	}
	for _, kind := range []string{"view", "function", "trigger", "policy", "user-defined type", "partition topology", "exclusion constraint", "generated/identity column", "sequence-backed column", "sequence"} {
		if !unsupportedKinds[kind] {
			t.Fatalf("unsupported objects = %#v, missing %s", snapshot.Unsupported, kind)
		}
	}
	viewFound, generatedColumnsFiltered := false, false
	for _, object := range snapshot.Unsupported {
		viewFound = viewFound || object.Kind == "view" && object.Name == "item_names"
	}
	for _, table := range snapshot.Tables {
		if table.Name == "generated" {
			generatedColumnsFiltered = len(table.Columns) == 0
		}
	}
	if !viewFound || !generatedColumnsFiltered {
		t.Fatalf("unsupported objects/tables = %#v/%#v, view=%v generated-columns-filtered=%v", snapshot.Unsupported, snapshot.Tables, viewFound, generatedColumnsFiltered)
	}
	diff, err := viewer.Diff(ctx, "viewer", connection.ID, schema, targetSchema)
	if err != nil {
		t.Fatal(err)
	}
	if diff.ReferenceSchema != schema || diff.TargetSchema != targetSchema || diff.SQL == "" {
		t.Fatalf("schema diff identity/SQL = %q/%q/%q", diff.ReferenceSchema, diff.TargetSchema, diff.SQL)
	}

	if _, err := viewer.Snapshot(ctx, "viewer", connection.ID, "pg_catalog"); err != ErrInvalidInput {
		t.Fatalf("internal schema error = %v, want invalid input", err)
	}
	if _, err := viewer.Snapshot(ctx, "viewer", connection.ID, "nonexistent_schema"); err != ErrNotFound {
		t.Fatalf("unknown schema error = %v, want not found", err)
	}
}

func TestPostgresSchemaSnapshotDeniesRelationsWithoutSelectPrivilege(t *testing.T) {
	target, cfg, _ := integrationDatabase(t, "TARGET_TEST_DATABASE_URL")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	role := "diff_reader_" + hex.EncodeToString(suffix[:])
	quotedRole := pgx.Identifier{role}.Sanitize()
	if _, err := target.ExecContext(ctx, "CREATE ROLE "+quotedRole+" NOLOGIN"); err != nil {
		t.Skip("test PostgreSQL role lacks CREATEROLE privilege")
	}
	t.Cleanup(func() {
		if _, err := target.ExecContext(context.Background(), "REVOKE "+quotedRole+" FROM "+pgx.Identifier{cfg.User}.Sanitize()); err != nil {
			t.Error("could not revoke temporary test role")
		}
		if _, err := target.ExecContext(context.Background(), "DROP ROLE "+quotedRole); err != nil {
			t.Error("could not clean up temporary test role")
		}
	})
	if _, err := target.ExecContext(ctx, "GRANT "+quotedRole+" TO "+pgx.Identifier{cfg.User}.Sanitize()); err != nil {
		t.Skip("test PostgreSQL role cannot grant temporary role membership")
	}
	schema := "diff_private_" + hex.EncodeToString(suffix[:])
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	secretTable := pgx.Identifier{schema, "secret"}.Sanitize()
	if _, err := target.ExecContext(ctx, "CREATE SCHEMA "+quotedSchema+"; CREATE TABLE "+secretTable+" (id integer); GRANT USAGE ON SCHEMA "+quotedSchema+" TO "+quotedRole); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := target.ExecContext(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Error("could not clean up restricted-privilege schema")
		}
	})
	limitedConfig := cfg.Copy()
	limitedConfig.RuntimeParams["role"] = role
	limitedDB := stdlib.OpenDB(*limitedConfig)
	t.Cleanup(func() { _ = limitedDB.Close() })
	viewer, err := NewViewer(&grantAuthorizer{grants: map[contracts.UserID]map[databaseID]contracts.AccessLevel{
		"viewer": {"1": contracts.AccessViewOnly},
	}}, &countingProvider{db: limitedDB})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.Snapshot(ctx, "viewer", "1", schema); err != contracts.ErrForbidden {
		t.Fatalf("Snapshot() without table SELECT = %v, want forbidden", err)
	}
}

func TestPostgresSchemaDiffSQLTransformsTargetWithoutLeavingSourceReferences(t *testing.T) {
	target, cfg, _ := integrationDatabase(t, "TARGET_TEST_DATABASE_URL")
	platform, _, _ := integrationDatabase(t, "PLATFORM_TEST_DATABASE_URL")
	manager := integrationManager(t, platform)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	base := "diff_apply_" + hex.EncodeToString(suffix[:])
	sourceSchema, targetSchema := base+"_source", base+"_target"
	quotedSource, quotedTarget := pgx.Identifier{sourceSchema}.Sanitize(), pgx.Identifier{targetSchema}.Sanitize()
	if _, err := target.ExecContext(ctx, "CREATE SCHEMA "+quotedSource+"; CREATE SCHEMA "+quotedTarget); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := target.ExecContext(context.Background(), "DROP SCHEMA "+quotedSource+", "+quotedTarget+" CASCADE"); err != nil {
			t.Error("could not clean up schema diff execution fixture")
		}
	})
	sourceParent := pgx.Identifier{sourceSchema, "parent"}.Sanitize()
	sourceItems := pgx.Identifier{sourceSchema, "items"}.Sanitize()
	targetParent := pgx.Identifier{targetSchema, "parent"}.Sanitize()
	targetItems := pgx.Identifier{targetSchema, "items"}.Sanitize()
	_, err := target.ExecContext(ctx, "CREATE TABLE "+sourceParent+` (id bigint PRIMARY KEY);
		CREATE TABLE `+sourceItems+` (id bigint PRIMARY KEY, name text NOT NULL DEFAULT 'x', parent_id bigint REFERENCES `+sourceParent+`(id));
		CREATE INDEX items_name_idx ON `+sourceItems+`(name);
		CREATE TABLE `+targetParent+` (id integer PRIMARY KEY);
		CREATE TABLE `+targetItems+` (id integer PRIMARY KEY, parent_id integer REFERENCES `+targetParent+`(id))`)
	if err != nil {
		t.Fatal(err)
	}
	input := validInput()
	input.Host, input.Port, input.Database, input.Username = cfg.Host, int(cfg.Port), cfg.Database, cfg.User
	input.Password = &cfg.Password
	if cfg.TLSConfig != nil {
		input.SSLMode = "verify-full"
		if cfg.TLSConfig.InsecureSkipVerify {
			input.SSLMode = "require"
		}
	}
	connection, err := manager.Create(ctx, "admin", input)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := NewViewer(&grantAuthorizer{grants: map[contracts.UserID]map[databaseID]contracts.AccessLevel{
		"editor": {connection.ID: contracts.AccessViewEdit},
	}}, manager)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := viewer.Diff(ctx, "editor", connection.ID, sourceSchema, targetSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Unsupported) != 0 || diff.SQL == "" || strings.Contains(diff.SQL, sourceSchema) {
		t.Fatalf("unexpected incomplete or source-bound diff: %#v", diff)
	}
	if _, err := target.ExecContext(ctx, diff.SQL); err != nil {
		t.Fatalf("generated SQL failed on disposable target schema: %v\n%s", err, diff.SQL)
	}
	var columnCount, constraintCount int
	if err := target.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname='items' AND a.attnum>0 AND NOT a.attisdropped`, targetSchema).Scan(&columnCount); err != nil {
		t.Fatal(err)
	}
	if err := target.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_constraint con
		JOIN pg_catalog.pg_class c ON c.oid=con.conrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relname='items' AND con.contype IN ('p','f','u','c')`, targetSchema).Scan(&constraintCount); err != nil {
		t.Fatal(err)
	}
	if columnCount != 3 || constraintCount != 2 {
		t.Fatalf("generated target has %d columns and %d supported constraints, want 3 and 2; SQL: %s", columnCount, constraintCount, diff.SQL)
	}
}
