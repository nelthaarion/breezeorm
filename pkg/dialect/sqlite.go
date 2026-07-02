package dialect

import (
	"strconv"
	"strings"
)

// SQLite implements Dialect for SQLite.
//
// STATUS: scaffold-level. Good enough for tests/dev; concurrent-index and
// some ALTER TABLE limitations of SQLite are not yet modeled in the migration
// engine and should be addressed before production use.
type SQLite struct{}

var _ Dialect = SQLite{}

func (SQLite) Name() string { return "sqlite" }

func (SQLite) Placeholder(int) string { return "?" }

func (SQLite) QuoteIdentifier(ident string) string {
	parts := strings.Split(ident, ".")
	for i, p := range parts {
		p = strings.ReplaceAll(p, `"`, `""`)
		parts[i] = `"` + p + `"`
	}
	return strings.Join(parts, ".")
}

func (SQLite) ReturningClause(columns []string) string {
	if len(columns) == 0 {
		return ""
	}
	s := SQLite{}
	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = s.QuoteIdentifier(c)
	}
	return "RETURNING " + strings.Join(quoted, ", ")
}

func (SQLite) LimitOffset(limit, offset *int64) string {
	var b strings.Builder
	if limit != nil {
		b.WriteString("LIMIT ")
		b.WriteString(strconv.FormatInt(*limit, 10))
	}
	if offset != nil {
		if b.Len() == 0 {
			b.WriteString("LIMIT -1 ")
		}
		b.WriteString(" OFFSET ")
		b.WriteString(strconv.FormatInt(*offset, 10))
	}
	return strings.TrimSpace(b.String())
}

func (SQLite) LockClause(LockMode) string { return "" } // SQLite has no row-level locking clauses.

func (SQLite) UpsertClause(target UpsertConflictTarget, updateColumns []string) string {
	s := SQLite{}
	var b strings.Builder
	b.WriteString("ON CONFLICT")
	if len(target.Columns) > 0 {
		quoted := make([]string, len(target.Columns))
		for i, c := range target.Columns {
			quoted[i] = s.QuoteIdentifier(c)
		}
		b.WriteString(" (" + strings.Join(quoted, ", ") + ")")
	}
	if len(updateColumns) == 0 {
		b.WriteString(" DO NOTHING")
		return b.String()
	}
	b.WriteString(" DO UPDATE SET ")
	sets := make([]string, len(updateColumns))
	for i, c := range updateColumns {
		q := s.QuoteIdentifier(c)
		sets[i] = q + " = excluded." + q
	}
	b.WriteString(strings.Join(sets, ", "))
	return b.String()
}

func (SQLite) BulkInsertSupported() bool { return true }
func (SQLite) SupportsReturning() bool   { return true }

func (SQLite) ValidateIdentifier(ident string) error {
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
