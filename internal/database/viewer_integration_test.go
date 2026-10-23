package database

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"if3110.local/db-platform/internal/auth"
	"if3110.local/db-platform/internal/contracts"
)

func TestPostgresViewerObjectsDataAndAuthorization(t *testing.T) {
	target, cfg, schema := integrationDatabase(t, "TARGET_TEST_DATABASE_URL")
	platform, _, _ := integrationDatabase(t, "PLATFORM_TEST_DATABASE_URL")
	m := integrationManager(t, platform)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := `odd";table`
	quoted := pgx.Identifier{name}.Sanitize()
	_, err := target.ExecContext(ctx, `CREATE TABLE parent (id integer PRIMARY KEY); INSERT INTO parent VALUES (1);
		CREATE TABLE `+quoted+` (
		id integer PRIMARY KEY CHECK(id>0), label text UNIQUE NOT NULL,
		"text""field" text, raw bytea, long_text text DEFAULT 'default value',
		parent_id integer REFERENCES parent(id));
		CREATE INDEX label_lower_idx ON `+quoted+` (lower(label));
		CREATE VIEW sample_view AS SELECT id,label FROM `+quoted+`;
		CREATE MATERIALIZED VIEW sample_materialized AS SELECT 1 AS n;
		CREATE TABLE partitioned (id integer) PARTITION BY RANGE(id);
		CREATE TABLE partition_child PARTITION OF partitioned FOR VALUES FROM (0) TO (10);
		INSERT INTO partitioned VALUES(1);
		CREATE TABLE zero_columns (); INSERT INTO zero_columns DEFAULT VALUES;`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = target.ExecContext(ctx, `INSERT INTO `+quoted+` SELECT n, CASE WHEN n=1 THEN '<script>alert(1)</script>' ELSE 'row-'||n END,
		CASE WHEN n=1 THEN NULL ELSE 'NULL' END, decode('00ff','hex'),repeat('界',2000),1 FROM generate_series(1,55) n`)
	if err != nil {
		t.Fatal(err)
	}
	in := validInput()
	in.Host = cfg.Host
	in.Port = int(cfg.Port)
	in.Database = cfg.Database
	in.Username = cfg.User
	in.Password = &cfg.Password
	if cfg.TLSConfig != nil {
		in.SSLMode = "verify-full"
		if cfg.TLSConfig.InsecureSkipVerify {
			in.SSLMode = "require"
		}
	}
	c, err := m.Create(ctx, "admin", in)
	if err != nil {
		t.Fatal(err)
	}
	a := &grantAuthorizer{grants: map[contracts.UserID]map[databaseID]contracts.AccessLevel{"viewer": {c.ID: contracts.AccessViewOnly}, "editor": {c.ID: contracts.AccessViewEdit}}}
	m.authorizer = a
	v, err := NewViewer(a, m)
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := v.Schemas(ctx, "viewer", c.ID)
	if err != nil || !slices.Contains(schemas, schema) {
		t.Fatalf("schema list: %v", err)
	}
	for _, s := range schemas {
		if !visibleSchema(s) {
			t.Fatal("internal schema visible")
		}
	}
	tables, err := v.Tables(ctx, "viewer", c.ID, schema)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, table := range tables {
		if table.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatal("quoted table missing")
	}
	detail, err := v.Details(ctx, "viewer", c.ID, schema, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Columns) != 6 || detail.Columns[0].Nullable || detail.Columns[1].Type != "text" || detail.Columns[2].Name != `text"field` || !detail.Columns[2].Nullable || !strings.Contains(detail.Columns[4].Default, "default value") {
		t.Fatal("column metadata incomplete")
	}
	definitions := ""
	for _, d := range detail.Constraints {
		definitions += d.SQL
	}
	for _, required := range []string{"PRIMARY KEY", "FOREIGN KEY", "UNIQUE", "CHECK"} {
		if !strings.Contains(definitions, required) {
			t.Fatalf("constraint missing: %s", required)
		}
	}
	indexFound := false
	for _, idx := range detail.Indexes {
		if idx.Name == "label_lower_idx" {
			indexFound = true
		}
	}
	if !indexFound {
		t.Fatal("index missing")
	}
	first, err := v.Data(ctx, "viewer", c.ID, schema, name, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rows) != 50 || !first.HasNext || first.Rows[0][0].Text != "1" || first.Rows[49][0].Text != "50" {
		t.Fatal("first page/order incorrect")
	}
	if !first.Rows[0][2].Null || first.Rows[1][2].Null || first.Rows[1][2].Text != "NULL" {
		t.Fatal("NULL not distinguished from text")
	}
	if first.Rows[0][3].Text != "\\x00ff" || !first.Rows[0][4].Truncated || len([]rune(first.Rows[0][4].Text)) != maxCellRunes {
		t.Fatal("binary/long values incorrect")
	}
	second, err := v.Data(ctx, "editor", c.ID, schema, name, 2)
	if err != nil || len(second.Rows) != 5 || second.HasNext || second.Rows[0][0].Text != "51" {
		t.Fatalf("second page: %v", err)
	}
	empty, err := v.Data(ctx, "viewer", c.ID, schema, name, 3)
	if err != nil || len(empty.Rows) != 0 || empty.HasNext {
		t.Fatal("empty page incorrect")
	}
	for _, table := range []string{"sample_view", "sample_materialized", "partitioned", "zero_columns"} {
		if page, err := v.Data(ctx, "viewer", c.ID, schema, table, 1); err != nil || len(page.Rows) == 0 {
			t.Fatalf("read %s: %v", table, err)
		}
	}
	for _, invalidPage := range []int{0, -1, 1000001} {
		if _, err := v.Data(ctx, "viewer", c.ID, schema, name, invalidPage); err != ErrInvalidInput {
			t.Fatal("invalid page accepted")
		}
	}
	if _, err := v.Details(ctx, "viewer", c.ID, schema, `missing";DROP TABLE parent;--`); err != ErrNotFound {
		t.Fatal("unknown identifier was not handled safely")
	}
	if _, err := v.Tables(ctx, "viewer", c.ID, "pg_catalog"); err != ErrInvalidInput {
		t.Fatal("internal schema accepted")
	}
	for _, user := range []contracts.UserID{"admin", "stranger"} {
		if _, err := v.Data(ctx, user, c.ID, schema, name, 1); err != contracts.ErrForbidden {
			t.Fatal("missing grant accepted")
		}
	}
	// Revoked PostgreSQL privileges also deny data, despite the platform grant.
	if _, err := target.ExecContext(ctx, "REVOKE SELECT ON "+quoted+" FROM "+pgx.Identifier{cfg.User}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Data(ctx, "viewer", c.ID, schema, name, 1); err != contracts.ErrForbidden {
		t.Fatalf("target denial: %v", err)
	}
	if _, err := target.ExecContext(ctx, "GRANT SELECT ON "+quoted+" TO "+pgx.Identifier{cfg.User}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	// A view with a writing function must not turn a viewer GET into a mutation.
	_, err = target.ExecContext(ctx, `CREATE FUNCTION write_from_view() RETURNS integer LANGUAGE plpgsql AS $$ BEGIN INSERT INTO `+pgx.Identifier{schema, "parent"}.Sanitize()+` VALUES(2); RETURN 2; END $$;
		CREATE VIEW dangerous_view AS SELECT write_from_view() AS result`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Data(ctx, "viewer", c.ID, schema, "dangerous_view", 1); err == nil {
		t.Fatal("viewer allowed writes")
	}
	var count int
	if err := target.QueryRowContext(ctx, "SELECT count(*) FROM parent").Scan(&count); err != nil || count != 1 {
		t.Fatal("viewer changed target")
	}
	// Real viewer queries rendered through the actual module routes and Templ.
	h, _, _ := testHandlers(t)
	h.manager = m
	h.viewer = v
	protected, err := auth.CSRFProtection(make([]byte, 32), false, h.routes())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ path, contains string }{
		{"/databases/", c.Name},
		{viewerURL(c.ID, "schemas", "", "", 0), schema},
		{viewerURL(c.ID, "tables", schema, "", 0), "sample_view"},
		{viewerURL(c.ID, "table", schema, name, 0), "label_lower_idx"},
		{viewerURL(c.ID, "data", schema, name, 1), "&lt;script&gt;"},
	} {
		w := requestPage(protected, test.path, "viewer")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), test.contains) {
			t.Fatalf("HTTP viewer %s: %d", test.path, w.Code)
		}
		if strings.Contains(w.Body.String(), "<script>") {
			t.Fatal("stored XSS in viewer")
		}
	}
	// Revocation must take effect on the next request, even with a cached pool.
	delete(a.grants["viewer"], c.ID)
	if w := requestPage(protected, viewerURL(c.ID, "data", schema, name, 1), "viewer"); w.Code != 403 {
		t.Fatal("revoked grant still usable")
	}
}
