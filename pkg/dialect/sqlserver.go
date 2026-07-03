package dialect

import (
	"strconv"
	"strings"
)

// SQLServer implements Dialect for Microsoft SQL Server.
//
// STATUS: scaffold-level / partial. OFFSET...FETCH NEXT requires an ORDER BY
// in real T-SQL; the planner must guarantee one is always present when this
// dialect is active (TODO: enforce in optimizer). OUTPUT clause is used in
// place of RETURNING. Upsert uses MERGE, which is intentionally minimal here.
type SQLServer struct{}

var _ Dialect = SQLServer{}

func (SQLServer) Name() string { return "sqlserver" }

func (SQLServer) Placeholder(n int) string { return "@p" + strconv.Itoa(n) }

func (SQLServer) QuoteIdentifier(ident string) string {
	parts := strings.Split(ident, ".")
	for i, p := range parts {
		p = strings.ReplaceAll(p, "]", "]]")
		parts[i] = "[" + p + "]"
	}
	return strings.Join(parts, ".")
}

func (SQLServer) ReturningClause(columns []string) string {
	if len(columns) == 0 {
		return ""
	}
	s := SQLServer{}
	quoted := make([]string, len(columns))
	for i, c := range columns {
		if c == "*" {
			// see the identical guard + comment in postgres.go's
			// ReturningClause: "*" is the wildcard, not an identifier —
			// T-SQL wants INSERTED.*, not INSERTED.[*].
			quoted[i] = "INSERTED.*"
			continue
		}
		quoted[i] = "INSERTED." + s.QuoteIdentifier(c)
	}
	return "OUTPUT " + strings.Join(quoted, ", ")
}

func (SQLServer) LimitOffset(limit, offset *int64) string {
	// NOTE: valid T-SQL requires ORDER BY before OFFSET/FETCH; the SQL
	// generator is responsible for guaranteeing that when this dialect is active.
	if offset == nil && limit == nil {
		return ""
	}
	off := int64(0)
	if offset != nil {
		off = *offset
	}
	var b strings.Builder
	b.WriteString("OFFSET ")
	b.WriteString(strconv.FormatInt(off, 10))
	b.WriteString(" ROWS")
	if limit != nil {
		b.WriteString(" FETCH NEXT ")
		b.WriteString(strconv.FormatInt(*limit, 10))
		b.WriteString(" ROWS ONLY")
	}
	return b.String()
}

func (SQLServer) LockClause(mode LockMode) string {
	switch mode {
	case LockForUpdate, LockForNoKeyUpdate:
		return "WITH (UPDLOCK, ROWLOCK)"
	case LockForShare:
		return "WITH (HOLDLOCK, ROWLOCK)"
	case LockForUpdateSkipLocked:
		return "WITH (UPDLOCK, ROWLOCK, READPAST)"
	case LockForUpdateNoWait:
		return "WITH (UPDLOCK, ROWLOCK, NOWAIT)"
	default:
		return ""
	}
}

func (SQLServer) UpsertClause(target UpsertConflictTarget, updateColumns []string) string {
	// TODO: full MERGE statement generation belongs in the SQL generator
	// since it needs the source/target table context; this returns the
	// fragment identifying intent for now.
	return "-- MERGE upsert: see sqlgen.BuildMerge"
}

func (SQLServer) BulkInsertSupported() bool { return true }
func (SQLServer) SupportsReturning() bool   { return true }

func (SQLServer) ValidateIdentifier(ident string) error {
	if ident == "" {
		return &ErrInvalidIdentifier{Ident: ident}
	}
	for _, r := range ident {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '.'
		if !ok {
			return &ErrInvalidIdentifier{Ident: ident}
		}
	}
	return nil
}
