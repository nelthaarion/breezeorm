// Package execution turns a compiled, optimized plan into SQL text and then
// runs it against a real *sql.DB. Prepared-statement and plan caches ensure
// that compilation (and the DB-side parse/plan) happens once per distinct
// query shape.
//
// As of Task 2.1, the SQL generation logic itself lives in pkg/sqlgen (a
// leaf package with no dependency on pkg/execution, so that pkg/compiler
// can import it without a cycle). This file re-exports the public types and
// functions of pkg/sqlgen under the execution package's old names, so that
// external callers (and pkg/orm) don't have to change their imports in the
// same commit. New code should import pkg/sqlgen directly.
package execution

import (
	"github.com/nelthaarion/breezeorm/pkg/planner"
	"github.com/nelthaarion/breezeorm/pkg/query"
	"github.com/nelthaarion/breezeorm/pkg/sqlgen"
)

// GeneratedSQL is an alias for sqlgen.GeneratedSQL, preserved for backward
// compatibility.
type GeneratedSQL = sqlgen.GeneratedSQL

// GenerateSQL is an alias for sqlgen.GenerateSQL, preserved for backward
// compatibility.
func GenerateSQL(pp *planner.PhysicalPlan) (*GeneratedSQL, error) {
	return sqlgen.GenerateSQL(pp)
}

// ExtractArgs is an alias for sqlgen.ExtractArgs, preserved for backward
// compatibility.
//
// DEPRECATED: see sqlgen.ExtractArgs's doc comment for why this function
// is wrong for any path that reuses a cached CompiledQuery across calls.
// Use ExtractArgsFromBuilder instead.
func ExtractArgs(pp *planner.PhysicalPlan) ([]any, error) {
	return sqlgen.ExtractArgs(pp)
}

// ExtractArgsFromBuilder is an alias for sqlgen.ExtractArgsFromBuilder,
// preserved for backward compatibility.
func ExtractArgsFromBuilder[T any](b query.Builder[T]) ([]any, error) {
	return sqlgen.ExtractArgsFromBuilder(b)
}
