package execution

import (
	"fmt"
	"strings"

	"github.com/nelthaarion/breezorm/pkg/dialect"
	"github.com/nelthaarion/breezorm/pkg/pool"
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

// GenerateBulkInsert renders a single multi-row INSERT statement:
//
//	INSERT INTO t (c1, c2) VALUES ($1,$2), ($3,$4), ... RETURNING ...
//
// columns and table are validated identifiers, exactly like GenerateSQL;
// rows[i] must have the same length and column order as columns.
func GenerateBulkInsert(d dialect.Dialect, table string, columns []string, rows [][]any) (*GeneratedSQL, error) {
	if !d.BulkInsertSupported() {
		return nil, fmt.Errorf("execution: dialect %s does not support bulk insert", d.Name())
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("execution: bulk insert requires at least one row")
	}
	if len(rows) > MaxBulkInsertRows {
		return nil, fmt.Errorf("execution: bulk insert of %d rows exceeds MaxBulkInsertRows (%d); chunk the input", len(rows), MaxBulkInsertRows)
	}
	for i, r := range rows {
		if len(r) != len(columns) {
			return nil, fmt.Errorf("execution: row %d has %d values, want %d (len(columns))", i, len(r), len(columns))
		}
	}

	g := &sqlGen{d: d, b: pool.Buffers.Get()}
	defer pool.Buffers.Put(g.b)

	g.b.WriteString("INSERT INTO ")
	g.b.WriteString(g.quoteIdent(table))
	g.b.WriteString(" (")
	g.b.WriteString(strings.Join(g.quoteIdents(columns), ", "))
	g.b.WriteString(") VALUES ")

	for ri, row := range rows {
		if ri > 0 {
			g.b.WriteString(", ")
		}
		g.b.WriteString("(")
		for ci, v := range row {
			if ci > 0 {
				g.b.WriteString(", ")
			}
			g.b.WriteString(g.bind(v))
		}
		g.b.WriteString(")")
	}

	if ret := d.ReturningClause([]string{"*"}); ret != "" {
		g.b.WriteString(" " + ret)
	}

	if g.err != nil {
		return nil, g.err
	}
	return &GeneratedSQL{SQL: g.b.String(), Args: g.args}, nil
}
