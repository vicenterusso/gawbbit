package model

import (
	"embed"
	"strings"
	"text/template"
)

//go:embed sql/*.sql
var sqlFiles embed.FS

var templates = template.Must(template.New("sql").Option("missingkey=error").ParseFS(sqlFiles, "sql/*.sql"))

// render executes one embedded SQL template. Templates are fixed at build
// time and their inputs are validated identifiers, so failure is a bug.
func render(name string, data any) string {
	var b strings.Builder
	if err := templates.ExecuteTemplate(&b, name, data); err != nil {
		panic("model: render " + name + ": " + err.Error())
	}
	return strings.TrimSpace(b.String())
}

// Relation quotes a database-qualified relation like dbt-clickhouse.
func Relation(database, identifier string) string {
	return "`" + database + "`.`" + identifier + "`"
}

func quoteColumns(columns []string) string {
	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = `"` + c + `"`
	}
	return strings.Join(quoted, ", ")
}

// CreateTableAs is the empty-create DDL of the target or scratch table.
func CreateTableAs(relation, sql string) string {
	return render("create_table_as.sql", map[string]any{"Relation": relation, "SQL": sql})
}

// InsertInto loads the model select into relation's columns.
func InsertInto(relation string, columns []string, sql string) string {
	return render("insert_into.sql", map[string]any{"Relation": relation, "Columns": quoteColumns(columns), "SQL": sql})
}

// DeleteExisting removes target keys that are replayed by the new data.
func DeleteExisting(target, newData string) string {
	return render("delete_existing.sql", map[string]any{"Target": target, "NewData": newData})
}

// InsertNewData copies the window winners into the target.
func InsertNewData(target, newData string, columns []string) string {
	return render("insert_new_data.sql", map[string]any{"Target": target, "NewData": newData,
		"Columns": quoteColumns(columns)})
}

// DropIfExists drops a relation of the given dbt relation type.
func DropIfExists(kind, relation string) string {
	return "drop " + kind + " if exists " + relation
}

// Rename renames a relation (dbt-clickhouse rename_relation).
func Rename(from, to string) string {
	return "rename table " + from + " to " + to
}

// ColumnsQuery lists a relation's columns in table order.
func ColumnsQuery(database, identifier string) string {
	return "select name, type from system.columns where table = '" + identifier +
		"' and database = '" + database + "' order by position"
}

// RelationsQuery lists the relations of a database with their engines.
func RelationsQuery(database string) string {
	return "select name, engine from system.tables where database = '" + database + "'"
}

// TestSQL wraps a data test query in dbt's failure-count wrapper.
func TestSQL(test DataTest, relation string) string {
	var inner string
	if test.Column != "" {
		inner = render("not_null.sql", map[string]any{"Column": test.Column, "Relation": relation})
	} else {
		inner = render("rawbbit_events_load_unique.sql", map[string]any{"Relation": relation})
	}
	return render("test_wrapper.sql", map[string]any{"SQL": inner})
}
