package database

import (
	"context"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/contracts"
)

func TestSchemaSnapshotRejectsAccessBeforeProvider(t *testing.T) {
	provider := &countingProvider{}
	viewer, err := NewViewer(&grantAuthorizer{}, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.Snapshot(context.Background(), "stranger", "1", "public"); err != contracts.ErrForbidden {
		t.Fatalf("Snapshot() error = %v, want forbidden", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", provider.calls)
	}
	if _, err := viewer.Snapshot(context.Background(), "viewer", "1", "pg_catalog"); err != ErrInvalidInput {
		t.Fatalf("internal schema error = %v, want invalid input", err)
	}
	if provider.calls != 0 {
		t.Fatalf("invalid schema reached provider %d times", provider.calls)
	}
}

func TestSchemaDiffRejectsAccessAndInvalidSchemaBeforeProvider(t *testing.T) {
	provider := &countingProvider{}
	viewer, err := NewViewer(&grantAuthorizer{}, provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.Diff(context.Background(), "stranger", "1", "desired", "live"); err != contracts.ErrForbidden {
		t.Fatalf("Diff() error = %v, want forbidden", err)
	}
	if _, err := viewer.Diff(context.Background(), "viewer", "1", "same", "same"); err != ErrInvalidInput {
		t.Fatalf("same-schema Diff() error = %v, want invalid input", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", provider.calls)
	}
}

func TestSupportedColumnsExcludeGeneratedAndUserDefinedColumns(t *testing.T) {
	columns := []Column{{Name: "plain", Type: "text"}, {Name: "identity", Type: "bigint"}, {Name: "mood", Type: "mood"}, {Name: "serial_id", Type: "bigint"}}
	unsupported := []SchemaObject{
		{Table: "items", Name: "identity", Kind: "generated/identity column"},
		{Table: "items", Name: "mood", Kind: "user-defined column type"},
		{Table: "items", Name: "serial_id", Kind: "sequence-backed column"},
	}
	got := supportedColumns("items", columns, unsupported)
	if len(got) != 1 || got[0].Name != "plain" {
		t.Fatalf("supported columns = %#v, want only plain", got)
	}
}

func TestCompareSchemasProducesStableRiskLabeledSQL(t *testing.T) {
	reference := SchemaSnapshot{Schema: "desired", Tables: []SchemaTable{
		{Name: `odd"table`, Kind: "Table", Columns: []Column{{Name: "id", Type: "integer", Nullable: false}}},
		{Name: "existing", Kind: "Table", Columns: []Column{{Name: "id", Type: "bigint", Nullable: false}, {Name: "name", Type: "text", Nullable: true}}},
	}}
	target := SchemaSnapshot{Schema: "live", Tables: []SchemaTable{
		{Name: "existing", Kind: "Table", Columns: []Column{{Name: "id", Type: "bigint", Nullable: false}}},
		{Name: "obsolete", Kind: "Table", Columns: []Column{{Name: "id", Type: "integer", Nullable: false}}},
	}}

	diff := CompareSchemas(reference, target)
	if len(diff.Changes) != 3 {
		t.Fatalf("change count = %d, want 3: %#v", len(diff.Changes), diff.Changes)
	}
	if diff.Changes[0].Kind != "table" || diff.Changes[0].Action != "add" || !strings.Contains(diff.Changes[0].SQL, `CREATE TABLE "live"."odd""table"`) {
		t.Fatalf("first change = %#v, want safely quoted table creation", diff.Changes[0])
	}
	if diff.Changes[1].Kind != "column" || diff.Changes[1].Action != "add" || diff.Changes[1].Risk != RiskReview {
		t.Fatalf("second change = %#v, want additive column", diff.Changes[1])
	}
	if diff.Changes[2].Kind != "table" || diff.Changes[2].Action != "drop" || diff.Changes[2].Risk != RiskDestructive {
		t.Fatalf("third change = %#v, want destructive table removal", diff.Changes[2])
	}
	if !strings.Contains(diff.SQL, `ALTER TABLE "live".`) || strings.Contains(diff.SQL, "CASCADE") {
		t.Fatalf("combined SQL = %q, want safe targeted statements without CASCADE", diff.SQL)
	}
	if again := CompareSchemas(reference, target); again.SQL != diff.SQL {
		t.Fatal("same snapshots produced unstable SQL")
	}
}

func TestCompareSchemasReportsUnsupportedObjectsWithoutSQL(t *testing.T) {
	diff := CompareSchemas(
		SchemaSnapshot{Schema: "desired", Unsupported: []SchemaObject{{Name: "report", Kind: "view"}}},
		SchemaSnapshot{Schema: "live"},
	)
	if len(diff.Unsupported) != 1 || diff.Unsupported[0].Name != "report" || diff.SQL != "" {
		t.Fatalf("diff = %#v, want unsupported view and no SQL", diff)
	}
}

func TestCompareSchemasDoesNotTreatIndexSchemaQualificationAsAnIndexChange(t *testing.T) {
	reference := SchemaSnapshot{Schema: "desired", Tables: []SchemaTable{{Name: "items", Kind: "Table", Indexes: []Definition{{
		Name: "items_name_idx", SQL: `CREATE INDEX items_name_idx ON desired.items USING btree (name)`,
	}}}}}
	target := SchemaSnapshot{Schema: "live", Tables: []SchemaTable{{Name: "items", Kind: "Table", Indexes: []Definition{{
		Name: "items_name_idx", SQL: `CREATE INDEX items_name_idx ON live.items USING btree (name)`,
	}}}}}
	if diff := CompareSchemas(reference, target); len(diff.Changes) != 0 || diff.SQL != "" {
		t.Fatalf("same index in different schemas produced diff: %#v", diff)
	}
}

func TestCompareSchemasDoesNotTreatSelfReferenceSchemaAsAConstraintChange(t *testing.T) {
	reference := SchemaSnapshot{Schema: "desired", Tables: []SchemaTable{{Name: "child", Kind: "Table", Constraints: []SchemaConstraint{{
		Name: "child_parent_fk", Definition: `FOREIGN KEY (parent_id) REFERENCES desired.parent(id)`, ReferenceSchema: "desired", ReferenceTable: "parent",
	}}}}}
	target := SchemaSnapshot{Schema: "live", Tables: []SchemaTable{{Name: "child", Kind: "Table", Constraints: []SchemaConstraint{{
		Name: "child_parent_fk", Definition: `FOREIGN KEY (parent_id) REFERENCES live.parent(id)`, ReferenceSchema: "live", ReferenceTable: "parent",
	}}}}}
	if diff := CompareSchemas(reference, target); len(diff.Changes) != 0 {
		t.Fatalf("equivalent self-schema foreign keys produced diff: %#v", diff.Changes)
	}
}

func TestCompareSchemasRebasesForeignKeyToTargetSchema(t *testing.T) {
	reference := SchemaSnapshot{Schema: "desired", Tables: []SchemaTable{
		{Name: "parent", Kind: "Table", Columns: []Column{{Name: "id", Type: "bigint", Nullable: false}}},
		{Name: "child", Kind: "Table", Columns: []Column{{Name: "parent_id", Type: "bigint", Nullable: true}}, Constraints: []SchemaConstraint{{
			Name: "child_parent_fk", Definition: `FOREIGN KEY (parent_id) REFERENCES desired.parent(id)`, ReferenceSchema: "desired", ReferenceTable: "parent",
		}}},
	}}
	target := SchemaSnapshot{Schema: "live", Tables: []SchemaTable{
		{Name: "parent", Kind: "Table", Columns: []Column{{Name: "id", Type: "bigint", Nullable: false}}},
		{Name: "child", Kind: "Table", Columns: []Column{{Name: "parent_id", Type: "bigint", Nullable: true}}},
	}}
	diff := CompareSchemas(reference, target)
	if len(diff.Changes) != 1 || !strings.Contains(diff.SQL, `REFERENCES "live"."parent"(id)`) {
		t.Fatalf("foreign key DDL = %#v, want reference rebased to live schema", diff)
	}
}

func TestCompareSchemasGeneratesSafeConstraintAndIndexChanges(t *testing.T) {
	reference := SchemaSnapshot{Schema: "desired", Tables: []SchemaTable{{
		Name: "items", Kind: "Table",
		Columns:     []Column{{Name: "id", Type: "bigint", Nullable: false, Default: "1"}, {Name: "name", Type: "text", Nullable: false, Default: "'new'::text"}},
		Constraints: []SchemaConstraint{{Name: "items_check", Definition: "CHECK (id > 0)"}},
		Indexes:     []Definition{{Name: `idx"name`, SQL: `CREATE INDEX "idx""name" ON desired.items USING btree (name)`}},
	}}}
	target := SchemaSnapshot{Schema: "live", Tables: []SchemaTable{{
		Name: "items", Kind: "Table",
		Columns:     []Column{{Name: "id", Type: "integer", Nullable: true}},
		Constraints: []SchemaConstraint{{Name: "items_check", Definition: "CHECK (id >= 0)"}},
		Indexes:     []Definition{{Name: `idx"name`, SQL: `CREATE INDEX "idx""name" ON live.items USING btree (id)`}},
	}}}
	diff := CompareSchemas(reference, target)
	var typeChange, notNull, defaultChange, constraintDrop, constraintAdd, indexDrop, indexAdd bool
	for _, change := range diff.Changes {
		switch {
		case change.Kind == "column" && change.Name == "id" && strings.Contains(change.SQL, " TYPE bigint"):
			typeChange = change.Risk == RiskDestructive
		case change.Kind == "column" && change.Name == "id" && strings.Contains(change.SQL, "SET NOT NULL"):
			notNull = change.Risk == RiskDestructive
		case change.Kind == "column" && change.Name == "id" && strings.Contains(change.SQL, "SET DEFAULT 1"):
			defaultChange = change.Risk == RiskReview
		case change.Kind == "column" && change.Name == "name" && strings.Contains(change.SQL, "ADD COLUMN"):
			if !strings.Contains(change.SQL, `"name" text DEFAULT 'new'::text NOT NULL`) {
				t.Fatalf("column SQL = %q", change.SQL)
			}
		case change.Kind == "constraint" && change.Action == "drop":
			constraintDrop = strings.Contains(change.SQL, "DROP CONSTRAINT")
		case change.Kind == "constraint" && change.Action == "add":
			constraintAdd = strings.Contains(change.SQL, "ADD CONSTRAINT")
		case change.Kind == "index" && change.Action == "drop":
			indexDrop = change.Risk == RiskDestructive && strings.Contains(change.SQL, `DROP INDEX "live"."idx""name"`)
		case change.Kind == "index" && change.Action == "add":
			indexAdd = strings.Contains(change.SQL, `ON "live"."items" USING btree (name)`)
		}
	}
	if !typeChange || !notNull || !defaultChange || !constraintDrop || !constraintAdd || !indexDrop || !indexAdd {
		t.Fatalf("diff missed changes: type=%v not-null=%v default=%v constraint-drop=%v constraint-add=%v index-drop=%v index-add=%v; %#v",
			typeChange, notNull, defaultChange, constraintDrop, constraintAdd, indexDrop, indexAdd, diff.Changes)
	}
}

func TestCompareSchemasDropsDependenciesBeforeDroppingColumn(t *testing.T) {
	reference := SchemaSnapshot{Schema: "desired", Tables: []SchemaTable{{Name: "items", Kind: "Table"}}}
	target := SchemaSnapshot{Schema: "live", Tables: []SchemaTable{{
		Name: "items", Kind: "Table", Columns: []Column{{Name: "obsolete", Type: "text", Nullable: true}},
		Constraints: []SchemaConstraint{{Name: "items_check", Definition: "CHECK (obsolete <> '')"}},
		Indexes:     []Definition{{Name: "items_obsolete_idx", SQL: "CREATE INDEX items_obsolete_idx ON live.items USING btree (obsolete)"}},
	}}}
	diff := CompareSchemas(reference, target)
	indexDrop := strings.Index(diff.SQL, `DROP INDEX "live"."items_obsolete_idx"`)
	constraintDrop := strings.Index(diff.SQL, `DROP CONSTRAINT "items_check"`)
	columnDrop := strings.Index(diff.SQL, `DROP COLUMN "obsolete"`)
	if indexDrop < 0 || constraintDrop < 0 || columnDrop < 0 || indexDrop > columnDrop || constraintDrop > columnDrop {
		t.Fatalf("dependency DDL order is unsafe: %s", diff.SQL)
	}
}

func TestCompareSchemasDropsDefaultBeforeChangingColumnType(t *testing.T) {
	diff := CompareSchemas(
		SchemaSnapshot{Schema: "desired", Tables: []SchemaTable{{Name: "items", Kind: "Table", Columns: []Column{{Name: "id", Type: "bigint", Nullable: false, Default: "2"}}}}},
		SchemaSnapshot{Schema: "live", Tables: []SchemaTable{{Name: "items", Kind: "Table", Columns: []Column{{Name: "id", Type: "integer", Nullable: false, Default: "1"}}}}},
	)
	dropDefault := strings.Index(diff.SQL, "DROP DEFAULT")
	typeChange := strings.Index(diff.SQL, " TYPE bigint")
	setDefault := strings.Index(diff.SQL, "SET DEFAULT 2")
	if dropDefault < 0 || typeChange < 0 || setDefault < 0 || dropDefault > typeChange || typeChange > setDefault {
		t.Fatalf("type/default DDL order is unsafe: %s", diff.SQL)
	}
}

func TestCompareSchemasReportsUnsupportedColumnReordering(t *testing.T) {
	diff := CompareSchemas(
		SchemaSnapshot{Schema: "desired", Tables: []SchemaTable{{Name: "items", Kind: "Table", Columns: []Column{{Name: "first", Type: "text"}, {Name: "second", Type: "text"}}}}},
		SchemaSnapshot{Schema: "live", Tables: []SchemaTable{{Name: "items", Kind: "Table", Columns: []Column{{Name: "second", Type: "text"}, {Name: "first", Type: "text"}}}}},
	)
	if len(diff.Changes) != 0 || len(diff.Unsupported) != 1 || diff.Unsupported[0].Kind != "column order" {
		t.Fatalf("reordered columns should be explicitly unsupported: %#v", diff)
	}
}

func TestCompareSchemasQuotesUntrustedObjectIdentifiers(t *testing.T) {
	column := `x"; DROP TABLE users;--`
	diff := CompareSchemas(
		SchemaSnapshot{Schema: "desired", Tables: []SchemaTable{{Name: "items", Kind: "Table", Columns: []Column{{Name: column, Type: "text", Nullable: true}}}}},
		SchemaSnapshot{Schema: "live", Tables: []SchemaTable{{Name: "items", Kind: "Table"}}},
	)
	if len(diff.Changes) != 1 || !strings.Contains(diff.SQL, `ADD COLUMN "x""; DROP TABLE users;--" text`) {
		t.Fatalf("identifier was not safely quoted: %q", diff.SQL)
	}
}

func TestCompareSchemasDoesNotGeneratePartialDDLForUnsupportedTables(t *testing.T) {
	reference := SchemaSnapshot{Schema: "desired", Tables: []SchemaTable{{Name: "items", Kind: "Table", Columns: []Column{{Name: "id", Type: "bigint"}}}}, Unsupported: []SchemaObject{{
		Schema: "desired", Name: "items_touch", Table: "items", Kind: "trigger",
	}}}
	if diff := CompareSchemas(reference, SchemaSnapshot{Schema: "live"}); len(diff.Changes) != 0 || diff.SQL != "" {
		t.Fatalf("unsupported table received partial DDL: %#v", diff)
	}
}

func TestCompareSchemasRemovesForeignKeysBeforeDroppingReferencedTables(t *testing.T) {
	target := SchemaSnapshot{Schema: "live", Tables: []SchemaTable{
		{Name: "a_parent", Kind: "Table", Columns: []Column{{Name: "id", Type: "bigint"}}},
		{Name: "z_child", Kind: "Table", Columns: []Column{{Name: "parent_id", Type: "bigint"}}, Constraints: []SchemaConstraint{{
			Name: "z_child_parent_fk", Definition: "FOREIGN KEY (parent_id) REFERENCES live.a_parent(id)", ReferenceSchema: "live", ReferenceTable: "a_parent",
		}}},
	}}
	diff := CompareSchemas(SchemaSnapshot{Schema: "desired"}, target)
	constraintDrop := strings.Index(diff.SQL, `DROP CONSTRAINT "z_child_parent_fk"`)
	firstTableDrop := strings.Index(diff.SQL, `DROP TABLE "live"."a_parent"`)
	if constraintDrop < 0 || firstTableDrop < 0 || constraintDrop > firstTableDrop {
		t.Fatalf("dependent table drop order is unsafe: %s", diff.SQL)
	}
}

func TestCompareSchemasRebuildsForeignKeysAroundReferencedTypeChanges(t *testing.T) {
	reference := SchemaSnapshot{Schema: "desired", Tables: []SchemaTable{
		{Name: "parent", Kind: "Table", Columns: []Column{{Name: "id", Type: "bigint"}}},
		{Name: "child", Kind: "Table", Columns: []Column{{Name: "parent_id", Type: "bigint"}}, Constraints: []SchemaConstraint{{
			Name: "child_parent_fk", Definition: "FOREIGN KEY (parent_id) REFERENCES desired.parent(id)", ReferenceSchema: "desired", ReferenceTable: "parent",
		}}},
	}}
	target := SchemaSnapshot{Schema: "live", Tables: []SchemaTable{
		{Name: "parent", Kind: "Table", Columns: []Column{{Name: "id", Type: "integer"}}},
		{Name: "child", Kind: "Table", Columns: []Column{{Name: "parent_id", Type: "integer"}}, Constraints: []SchemaConstraint{{
			Name: "child_parent_fk", Definition: "FOREIGN KEY (parent_id) REFERENCES live.parent(id)", ReferenceSchema: "live", ReferenceTable: "parent",
		}}},
	}}
	diff := CompareSchemas(reference, target)
	drop := strings.Index(diff.SQL, `DROP CONSTRAINT "child_parent_fk"`)
	parentType := strings.Index(diff.SQL, `ALTER COLUMN "id" TYPE bigint`)
	childType := strings.Index(diff.SQL, `ALTER COLUMN "parent_id" TYPE bigint`)
	add := strings.Index(diff.SQL, `ADD CONSTRAINT "child_parent_fk"`)
	if drop < 0 || parentType < 0 || childType < 0 || add < 0 || drop > parentType || drop > childType || add < parentType || add < childType {
		t.Fatalf("foreign key/type DDL order is unsafe: %s", diff.SQL)
	}
}
