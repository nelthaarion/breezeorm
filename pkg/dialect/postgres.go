package dialect

import (
	"strconv"
	"strings"
)

// Postgres implements Dialect for PostgreSQL.
type Postgres struct{}

var _ Dialect = Postgres{}

func (Postgres) Name() string { return "postgres" }

func (Postgres) Placeholder(n int) string {
	return "$" + strconv.Itoa(n)
}

func (Postgres) QuoteIdentifier(ident string) string {
	// Split on '.' to correctly quote schema.table / table.column references.
	parts := strings.Split(ident, ".")
	for i, p := range parts {
		p = strings.ReplaceAll(p, `"`, `""`)
		parts[i] = `"` + p + `"`
	}
	return strings.Join(parts, ".")
}

func (Postgres) ReturningClause(columns []string) string {
	if len(columns) == 0 {
		return ""
	}
	quoted := make([]string, len(columns))
	p := Postgres{}
	for i, c := range columns {
		if c == "*" {
			// "*" is the wildcard, not an identifier — quoting it produces
			// RETURNING "*", which Postgres parses as a column literally
			// named *, not "all columns" (SQLSTATE 42703: column "*" does
			// not exist). Every other caller passes real column names,
			// which do need quoting, so this only special-cases the
			// wildcard sqlgen.go passes for "RETURNING everything".
			quoted[i] = c
			continue
		}
		quoted[i] = p.QuoteIdentifier(c)
	}
	return "RETURNING " + strings.Join(quoted, ", ")
}

func (Postgres) LimitOffset(limit, offset *int64) string {
	var b strings.Builder
	if limit != nil {
		b.WriteString("LIMIT ")
		b.WriteString(strconv.FormatInt(*limit, 10))
	}
	if offset != nil {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("OFFSET ")
		b.WriteString(strconv.FormatInt(*offset, 10))
	}
	return b.String()
}

func (Postgres) LockClause(mode LockMode) string {
	switch mode {
	case LockForUpdate:
		return "FOR UPDATE"
	case LockForShare:
		return "FOR SHARE"
	case LockForNoKeyUpdate:
		return "FOR NO KEY UPDATE"
	case LockForUpdateSkipLocked:
		return "FOR UPDATE SKIP LOCKED"
	case LockForUpdateNoWait:
		return "FOR UPDATE NOWAIT"
	default:
		return ""
	}
}

func (Postgres) UpsertClause(target UpsertConflictTarget, updateColumns []string) string {
	p := Postgres{}
	var b strings.Builder
	b.WriteString("ON CONFLICT ")
	switch {
	case target.Constraint != "":
		b.WriteString("ON CONSTRAINT ")
		b.WriteString(target.Constraint)
	case len(target.Columns) > 0:
		quoted := make([]string, len(target.Columns))
		for i, c := range target.Columns {
			quoted[i] = p.QuoteIdentifier(c)
		}
		b.WriteByte('(')
		b.WriteString(strings.Join(quoted, ", "))
		b.WriteByte(')')
	}
	if len(updateColumns) == 0 {
		b.WriteString(" DO NOTHING")
		return b.String()
	}
	b.WriteString(" DO UPDATE SET ")
	sets := make([]string, len(updateColumns))
	for i, c := range updateColumns {
		q := p.QuoteIdentifier(c)
		sets[i] = q + " = EXCLUDED." + q
	}
	b.WriteString(strings.Join(sets, ", "))
	return b.String()
}

func (Postgres) BulkInsertSupported() bool { return true }
func (Postgres) SupportsReturning() bool   { return true }

func (Postgres) ValidateIdentifier(ident string) error {
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
