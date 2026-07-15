package execution

import (
	"fmt"

	"github.com/nelthaarion/breezeorm/pkg/dialect"
	"github.com/nelthaarion/breezeorm/pkg/sqlgen"
)

// MaxBulkInsertRows bounds how many rows a single GenerateBulkInsert call
// will accept. This exists for the same reason request-body size limits
// exist on an HTTP server: without a cap, a caller (malicious or just buggy)
// building one giant INSERT from an unbounded slice can produce a statement
// that blows past the driver's parameter limit (Postgres: 65535 total bind
// parameters; MySQL: max_allowed_packet) or simply pins an enormous amount
// of memory building the SQL text and argument slice. Callers with more rows
// than this should chunk and issue multiple statements/transactions.
const MaxBulkInsertRows = 5000

// GenerateBulkInsert is a re-export of sqlgen.GenerateBulkInsert, kept in
// pkg/execution for backward compatibility with existing callers.
//
// As of Task 2.1, the implementation lives in pkg/sqlgen so that
// pkg/compiler can import it without a cycle.
func GenerateBulkInsert(d dialect.Dialect, table string, columns []string, rows [][]any) (*GeneratedSQL, error) {
	if len(rows) > MaxBulkInsertRows {
		return nil, fmt.Errorf("execution: bulk insert of %d rows exceeds MaxBulkInsertRows (%d); chunk the input", len(rows), MaxBulkInsertRows)
	}
	return sqlgen.GenerateBulkInsert(d, table, columns, rows)
}
