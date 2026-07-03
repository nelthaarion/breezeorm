package migrations

import "github.com/nelthaarion/breezeorm/pkg/metadata"

// DesiredSchema is the schema implied by compiled model metadata — i.e. what
// "auto migration" wants the database to look like.
type DesiredSchema struct {
	Tables []*metadata.Table
}

// ActualSchema is what auto-migration introspects from the live database.
// Population of this (via information_schema / pg_catalog / etc. queries per
// dialect) is intentionally left as a TODO — it needs one implementation per
// dialect and is the single largest remaining piece of the migration engine.
type ActualSchema struct {
	Tables []TableInfo
}

type TableInfo struct {
	Name        string
	Columns     []ColumnInfo
	Indexes     []IndexInfo
	Constraints []ConstraintInfo
}

type ColumnInfo struct {
	Name     string
	DBType   string
	Nullable bool
	Default  string
}

type IndexInfo struct {
	Name    string
	Columns []string
	Unique  bool
}

type ConstraintInfo struct {
	Name string
	Kind string
}

// SchemaDiff is the set of DDL operations needed to reconcile Actual -> Desired.
type SchemaDiff struct {
	CreateTables []string
	AddColumns   []ColumnDiff
	DropColumns  []ColumnDiff
	AddIndexes   []IndexDiff
	DropIndexes  []IndexDiff
}

type ColumnDiff struct {
	Table  string
	Column string
}

type IndexDiff struct {
	Table string
	Index string
}

// Diff computes the schema changes required to migrate actual to desired.
//
// STATUS: structural comparison only (tables/columns/indexes present or
// absent by name). Type-compatibility diffing (e.g. VARCHAR(255) ->
// VARCHAR(500)) and constraint diffing are TODO — they require per-dialect
// type-name normalization, which belongs in pkg/dialect once implemented.
func Diff(desired DesiredSchema, actual ActualSchema) SchemaDiff {
	actualTables := make(map[string]TableInfo, len(actual.Tables))
	for _, t := range actual.Tables {
		actualTables[t.Name] = t
	}

	var d SchemaDiff
	for _, want := range desired.Tables {
		got, exists := actualTables[want.Name]
		if !exists {
			d.CreateTables = append(d.CreateTables, want.Name)
			continue
		}
		gotCols := make(map[string]bool, len(got.Columns))
		for _, c := range got.Columns {
			gotCols[c.Name] = true
		}
		for _, wc := range want.Columns {
			if !gotCols[wc.Name] {
				d.AddColumns = append(d.AddColumns, ColumnDiff{Table: want.Name, Column: wc.Name})
			}
		}

		gotIdx := make(map[string]bool, len(got.Indexes))
		for _, ix := range got.Indexes {
			gotIdx[ix.Name] = true
		}
		for _, wi := range want.Indexes {
			if !gotIdx[wi.Name] {
				d.AddIndexes = append(d.AddIndexes, IndexDiff{Table: want.Name, Index: wi.Name})
			}
		}
	}
	return d
}
