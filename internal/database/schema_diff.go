package database

import (
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

type ChangeRisk string

const (
	RiskReview      ChangeRisk = "review"
	RiskDestructive ChangeRisk = "destructive"
)

type SchemaChange struct {
	Kind, Action, Table, Name string
	Before, After             string
	Risk                      ChangeRisk
	SQL                       string
}

type SchemaDiff struct {
	ReferenceSchema, TargetSchema string
	Changes                       []SchemaChange
	Unsupported                   []SchemaObject
	SQL                           string
}

func CompareSchemas(reference, target SchemaSnapshot) SchemaDiff {
	diff := SchemaDiff{
		ReferenceSchema: reference.Schema,
		TargetSchema:    target.Schema,
		Changes:         make([]SchemaChange, 0),
		Unsupported:     append(append([]SchemaObject(nil), reference.Unsupported...), target.Unsupported...),
	}
	referenceTables := schemaTablesByName(reference.Tables)
	targetTables := schemaTablesByName(target.Tables)
	typeChangedTables := changedColumnTypeTables(referenceTables, targetTables)
	var additions, alterations, removals []SchemaChange

	for _, name := range sortedKeys(referenceTables) {
		if unsupportedTable(reference.Unsupported, name) || unsupportedTable(target.Unsupported, name) {
			continue
		}
		ref := referenceTables[name]
		targetTable, exists := targetTables[name]
		if !exists {
			columns := make([]string, 0, len(ref.Columns))
			for _, column := range ref.Columns {
				columns = append(columns, columnDefinition(column))
			}
			additions = append(additions, SchemaChange{Kind: "table", Action: "add", Name: name,
				After: ref.Kind, SQL: "CREATE TABLE " + qualifiedTable(target.Schema, name) + " (" + strings.Join(columns, ", ") + ")"})
			targetTable = SchemaTable{}
		}
		if exists && ref.Kind != targetTable.Kind {
			alterations = append(alterations, SchemaChange{Kind: "table", Action: "change", Name: name, Before: targetTable.Kind, After: ref.Kind, Risk: RiskDestructive})
			diff.Unsupported = append(diff.Unsupported, SchemaObject{Schema: reference.Schema, Name: name, Kind: "relation kind change"})
			continue
		}
		if exists {
			if columnsReordered(ref.Columns, targetTable.Columns) {
				diff.Unsupported = append(diff.Unsupported, SchemaObject{Schema: reference.Schema, Name: name, Kind: "column order"})
			}
			alterations = append(alterations, compareColumns(reference.Schema, target.Schema, name, ref.Columns, targetTable.Columns)...)
		}
		alterations = append(alterations, compareConstraints(reference.Schema, target.Schema, name, ref.Constraints, targetTable.Constraints, typeChangedTables)...)
		alterations = append(alterations, compareIndexes(reference.Schema, target.Schema, name, ref.Indexes, targetTable.Indexes)...)
	}

	for _, name := range sortedKeys(targetTables) {
		if _, exists := referenceTables[name]; exists || unsupportedTable(reference.Unsupported, name) || unsupportedTable(target.Unsupported, name) {
			continue
		}
		for _, constraint := range targetTables[name].Constraints {
			if constraint.ReferenceTable == "" || constraint.ReferenceSchema != target.Schema {
				continue
			}
			if _, remains := referenceTables[constraint.ReferenceTable]; remains {
				continue
			}
			if _, dropping := targetTables[constraint.ReferenceTable]; !dropping {
				continue
			}
			alterations = append(alterations, SchemaChange{Kind: "constraint", Action: "drop", Table: name, Name: constraint.Name,
				Before: constraint.Definition, Risk: RiskDestructive,
				SQL: "ALTER TABLE " + qualifiedTable(target.Schema, name) + " DROP CONSTRAINT " + quoteIdentifier(constraint.Name)})
		}
		removals = append(removals, SchemaChange{Kind: "table", Action: "drop", Name: name, Before: targetTables[name].Kind,
			Risk: RiskDestructive, SQL: "DROP TABLE " + qualifiedTable(target.Schema, name)})
	}
	diff.Changes = append(diff.Changes, additions...)
	diff.Changes = append(diff.Changes, alterations...)
	diff.Changes = append(diff.Changes, removals...)
	sort.SliceStable(diff.Changes, func(i, j int) bool {
		return changeOrder(diff.Changes[i]) < changeOrder(diff.Changes[j])
	})
	statements := make([]string, 0, len(diff.Changes))
	for _, change := range diff.Changes {
		if change.SQL != "" {
			statements = append(statements, change.SQL)
		}
	}
	diff.SQL = strings.Join(statements, ";\n")
	if diff.SQL != "" {
		diff.SQL += ";"
	}
	return diff
}

func unsupportedTable(objects []SchemaObject, table string) bool {
	for _, object := range objects {
		if object.Table == table {
			return true
		}
	}
	return false
}

func columnsReordered(reference, target []Column) bool {
	if len(reference) != len(target) {
		return false
	}
	targetNames := make(map[string]bool, len(target))
	for _, column := range target {
		targetNames[column.Name] = true
	}
	for i := range reference {
		if !targetNames[reference[i].Name] {
			return false
		}
		if reference[i].Name != target[i].Name {
			return true
		}
	}
	return false
}

func changeOrder(change SchemaChange) int {
	switch {
	case change.Kind == "table" && change.Action == "add":
		return 0
	case change.Action == "drop" && (change.Kind == "constraint" || change.Kind == "index"):
		return 1
	case change.Kind == "column" && change.Action == "drop":
		return 2
	case change.Kind == "column" && strings.Contains(change.SQL, " DROP DEFAULT"):
		return 2
	case change.Kind == "column":
		return 3
	case change.Kind == "constraint" && change.Action == "add":
		return 4
	case change.Kind == "index" && change.Action == "add":
		return 5
	case change.Kind == "table" && change.Action == "drop":
		return 6
	default:
		return 7
	}
}

func compareColumns(referenceSchema, targetSchema, table string, reference, target []Column) []SchemaChange {
	refByName, targetByName := columnsByName(reference), columnsByName(target)
	var changes []SchemaChange
	for _, name := range sortedKeys(refByName) {
		ref := refByName[name]
		old, exists := targetByName[name]
		if !exists {
			changes = append(changes, SchemaChange{Kind: "column", Action: "add", Table: table, Name: name, After: columnDescription(ref), Risk: RiskReview,
				SQL: "ALTER TABLE " + qualifiedTable(targetSchema, table) + " ADD COLUMN " + columnDefinition(ref)})
			continue
		}
		typeChanged := old.Type != ref.Type
		if typeChanged && old.Default != "" {
			changes = append(changes, SchemaChange{Kind: "column", Action: "change", Table: table, Name: name, Before: old.Default, Risk: RiskReview,
				SQL: "ALTER TABLE " + qualifiedTable(targetSchema, table) + " ALTER COLUMN " + quoteIdentifier(name) + " DROP DEFAULT"})
		}
		if typeChanged {
			changes = append(changes, SchemaChange{Kind: "column", Action: "change", Table: table, Name: name, Before: old.Type, After: ref.Type, Risk: RiskDestructive,
				SQL: "ALTER TABLE " + qualifiedTable(targetSchema, table) + " ALTER COLUMN " + quoteIdentifier(name) + " TYPE " + ref.Type})
		}
		if old.Nullable != ref.Nullable {
			verb, risk := "DROP NOT NULL", ChangeRisk(RiskReview)
			if !ref.Nullable {
				verb, risk = "SET NOT NULL", RiskDestructive
			}
			changes = append(changes, SchemaChange{Kind: "column", Action: "change", Table: table, Name: name,
				Before: columnDescription(old), After: columnDescription(ref), Risk: risk,
				SQL: "ALTER TABLE " + qualifiedTable(targetSchema, table) + " ALTER COLUMN " + quoteIdentifier(name) + " " + verb})
		}
		if typeChanged && ref.Default != "" {
			changes = append(changes, SchemaChange{Kind: "column", Action: "change", Table: table, Name: name, After: ref.Default, Risk: RiskReview,
				SQL: "ALTER TABLE " + qualifiedTable(targetSchema, table) + " ALTER COLUMN " + quoteIdentifier(name) + " SET DEFAULT " + ref.Default})
		} else if !typeChanged && old.Default != ref.Default {
			verb := "DROP DEFAULT"
			if ref.Default != "" {
				verb = "SET DEFAULT " + ref.Default
			}
			changes = append(changes, SchemaChange{Kind: "column", Action: "change", Table: table, Name: name, Before: old.Default, After: ref.Default, Risk: RiskReview,
				SQL: "ALTER TABLE " + qualifiedTable(targetSchema, table) + " ALTER COLUMN " + quoteIdentifier(name) + " " + verb})
		}
	}
	for _, name := range sortedKeys(targetByName) {
		if _, exists := refByName[name]; exists {
			continue
		}
		old := targetByName[name]
		changes = append(changes, SchemaChange{Kind: "column", Action: "drop", Table: table, Name: name, Before: columnDescription(old), Risk: RiskDestructive,
			SQL: "ALTER TABLE " + qualifiedTable(targetSchema, table) + " DROP COLUMN " + quoteIdentifier(name)})
	}
	return changes
}

func compareConstraints(referenceSchema, targetSchema, table string, reference, target []SchemaConstraint, changedTypes map[string]bool) []SchemaChange {
	refByName, targetByName := constraintsByName(reference), constraintsByName(target)
	var changes []SchemaChange
	for _, name := range sortedKeys(targetByName) {
		ref, exists := refByName[name]
		old := targetByName[name]
		forced := foreignKeyTouchesChangedType(old, table, changedTypes)
		if exists && !forced && constraintSignature(old, targetSchema) == constraintSignature(ref, referenceSchema) {
			continue
		}
		sql := "ALTER TABLE " + qualifiedTable(targetSchema, table) + " DROP CONSTRAINT " + quoteIdentifier(name)
		changes = append(changes, SchemaChange{Kind: "constraint", Action: "drop", Table: table, Name: name, Before: old.Definition, Risk: RiskDestructive, SQL: sql})
	}
	for _, name := range sortedKeys(refByName) {
		ref, exists := targetByName[name]
		forced := foreignKeyTouchesChangedType(refByName[name], table, changedTypes)
		if exists && !forced && constraintSignature(ref, targetSchema) == constraintSignature(refByName[name], referenceSchema) {
			continue
		}
		constraint := refByName[name]
		definition := constraint.Definition
		if constraint.ReferenceTable != "" {
			reference := constraint.ReferenceSchema
			if reference == referenceSchema {
				reference = targetSchema
			}
			definition = rewriteConstraintReference(definition, reference, constraint.ReferenceTable)
		}
		changes = append(changes, SchemaChange{Kind: "constraint", Action: "add", Table: table, Name: name, After: constraint.Definition, Risk: RiskReview,
			SQL: "ALTER TABLE " + qualifiedTable(targetSchema, table) + " ADD CONSTRAINT " + quoteIdentifier(name) + " " + definition})
	}
	return changes
}

func foreignKeyTouchesChangedType(constraint SchemaConstraint, table string, changedTypes map[string]bool) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(constraint.Definition)), "FOREIGN KEY") &&
		(changedTypes[table] || changedTypes[constraint.ReferenceTable])
}

func changedColumnTypeTables(reference, target map[string]SchemaTable) map[string]bool {
	changed := make(map[string]bool)
	for name, ref := range reference {
		old, exists := target[name]
		if !exists {
			continue
		}
		oldColumns := columnsByName(old.Columns)
		for _, column := range ref.Columns {
			if previous, exists := oldColumns[column.Name]; exists && previous.Type != column.Type {
				changed[name] = true
				break
			}
		}
	}
	return changed
}

func constraintSignature(constraint SchemaConstraint, localSchema string) string {
	if constraint.ReferenceTable == "" {
		return constraint.Definition
	}
	schema := constraint.ReferenceSchema
	if schema == localSchema {
		schema = "__local_schema__"
	}
	return rewriteConstraintReference(constraint.Definition, schema, constraint.ReferenceTable)
}

func compareIndexes(referenceSchema, targetSchema, table string, reference, target []Definition) []SchemaChange {
	refByName, targetByName := definitionsByName(reference), definitionsByName(target)
	var changes []SchemaChange
	for _, name := range sortedKeys(targetByName) {
		ref, exists := refByName[name]
		old := targetByName[name]
		if exists && indexSignature(old.SQL) == indexSignature(ref.SQL) {
			continue
		}
		changes = append(changes, SchemaChange{Kind: "index", Action: "drop", Table: table, Name: name, Before: old.SQL, Risk: RiskDestructive,
			SQL: "DROP INDEX " + pgx.Identifier{targetSchema, name}.Sanitize()})
	}
	for _, name := range sortedKeys(refByName) {
		old, exists := targetByName[name]
		definition := refByName[name]
		if exists && indexSignature(old.SQL) == indexSignature(definition.SQL) {
			continue
		}
		sql := rebaseIndex(definition.SQL, name, targetSchema, table)
		changes = append(changes, SchemaChange{Kind: "index", Action: "add", Table: table, Name: name, After: definition.SQL, Risk: RiskReview, SQL: sql})
	}
	return changes
}

func indexSignature(definition string) string {
	on := findSQLKeyword(definition, "ON", 0)
	if on < 0 {
		return strings.TrimSpace(definition)
	}
	using := findSQLKeyword(definition, "USING", on+2)
	if using < 0 {
		return strings.TrimSpace(definition)
	}
	unique := strings.HasPrefix(strings.ToUpper(definition), "CREATE UNIQUE INDEX ")
	return boolString(unique) + " " + strings.TrimSpace(definition[using:])
}

func columnDefinition(column Column) string {
	definition := quoteIdentifier(column.Name) + " " + column.Type
	if column.Default != "" {
		definition += " DEFAULT " + column.Default
	}
	if !column.Nullable {
		definition += " NOT NULL"
	}
	return definition
}

func columnDescription(column Column) string {
	return column.Type + " nullable=" + boolString(column.Nullable) + " default=" + column.Default
}
func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
func qualifiedTable(schema, table string) string { return pgx.Identifier{schema, table}.Sanitize() }
func quoteIdentifier(name string) string         { return pgx.Identifier{name}.Sanitize() }

func rewriteConstraintReference(definition, schema, table string) string {
	start := findSQLKeyword(definition, "REFERENCES", 0)
	if start < 0 {
		return definition
	}
	nameStart := start + len("REFERENCES")
	for nameStart < len(definition) && (definition[nameStart] == ' ' || definition[nameStart] == '\t') {
		nameStart++
	}
	nameEnd := nameStart
	quoted := false
	for nameEnd < len(definition) {
		c := definition[nameEnd]
		if c == '"' {
			if quoted && nameEnd+1 < len(definition) && definition[nameEnd+1] == '"' {
				nameEnd += 2
				continue
			}
			quoted = !quoted
			nameEnd++
			continue
		}
		if !quoted && (c == '(' || c == ' ' || c == '\t' || c == '\n') {
			break
		}
		nameEnd++
	}
	return definition[:nameStart] + qualifiedTable(schema, table) + definition[nameEnd:]
}

func rebaseIndex(definition, name, schema, table string) string {
	on := findSQLKeyword(definition, "ON", 0)
	if on < 0 {
		return ""
	}
	using := findSQLKeyword(definition, "USING", on+2)
	if using < 0 {
		return ""
	}
	prefix := "CREATE INDEX "
	if strings.HasPrefix(strings.ToUpper(definition), "CREATE UNIQUE INDEX ") {
		prefix = "CREATE UNIQUE INDEX "
	}
	return prefix + quoteIdentifier(name) + " ON " + qualifiedTable(schema, table) + " " + strings.TrimSpace(definition[using:])
}

func findSQLKeyword(sql, keyword string, from int) int {
	quoted := false
	for i := from; i+len(keyword) <= len(sql); i++ {
		if sql[i] == '"' {
			if quoted && i+1 < len(sql) && sql[i+1] == '"' {
				i++
				continue
			}
			quoted = !quoted
			continue
		}
		if quoted || !strings.EqualFold(sql[i:i+len(keyword)], keyword) {
			continue
		}
		beforeOK := i == 0 || sql[i-1] == ' ' || sql[i-1] == '\t' || sql[i-1] == '\n'
		after := i + len(keyword)
		afterOK := after == len(sql) || sql[after] == ' ' || sql[after] == '\t' || sql[after] == '\n'
		if beforeOK && afterOK {
			return i
		}
	}
	return -1
}

func schemaTablesByName(items []SchemaTable) map[string]SchemaTable {
	result := make(map[string]SchemaTable, len(items))
	for _, item := range items {
		result[item.Name] = item
	}
	return result
}
func columnsByName(items []Column) map[string]Column {
	result := make(map[string]Column, len(items))
	for _, item := range items {
		result[item.Name] = item
	}
	return result
}
func constraintsByName(items []SchemaConstraint) map[string]SchemaConstraint {
	result := make(map[string]SchemaConstraint, len(items))
	for _, item := range items {
		result[item.Name] = item
	}
	return result
}
func definitionsByName(items []Definition) map[string]Definition {
	result := make(map[string]Definition, len(items))
	for _, item := range items {
		result[item.Name] = item
	}
	return result
}
func sortedKeys[T any](items map[string]T) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
