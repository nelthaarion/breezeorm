package dialect

import (
	"strconv"
	"strings"
)

// MySQL implements Dialect for MySQL/MariaDB.
//
// STATUS: scaffold-level. Placeholders, quoting, LIMIT/OFFSET and upsert are
// implemented; RETURNING is unsupported prior to MySQL 8.0's rarely-used
// extensions and is intentionally left as a no-op. Flesh out generated-column
// and JSON-path handling before production use.
type MySQL struct{}

var _ Dialect = MySQL{}

func (MySQL) Name() string { return "mysql" }

func (MySQL) Placeholder(int) string { return "?" }

func (MySQL) QuoteIdentifier(ident string) string {
	parts := strings.Split(ident, ".")
	for i, p := range parts {
		p = strings.ReplaceAll(p, "`", "``")
		parts[i] = "`" + p + "`"
	}
	return strings.Join(parts, ".")
}

func (MySQL) ReturningClause([]string) string { return "" } // TODO: MySQL has no RETURNING; fetch via LAST_INSERT_ID / re-select.

func (MySQL) LimitOffset(limit, offset *int64) string {
	var b strings.Builder
	if limit != nil {
		b.WriteString("LIMIT ")
		b.WriteString(strconv.FormatInt(*limit, 10))
	}
	if offset != nil {
		if b.Len() == 0 {
			b.WriteString("LIMIT 18446744073709551615 ") // MySQL requires LIMIT before OFFSET
		}
		b.WriteString(" OFFSET ")
		b.WriteString(strconv.FormatInt(*offset, 10))
	}
	return strings.TrimSpace(b.String())
}

func (MySQL) LockClause(mode LockMode) string {
	switch mode {
	case LockForUpdate, LockForNoKeyUpdate:
		return "FOR UPDATE"
	case LockForShare:
		return "LOCK IN SHARE MODE"
	case LockForUpdateSkipLocked:
		return "FOR UPDATE SKIP LOCKED"
	case LockForUpdateNoWait:
		return "FOR UPDATE NOWAIT"
	default:
		return ""
	}
}

func (MySQL) UpsertClause(_ UpsertConflictTarget, updateColumns []string) string {
	m := MySQL{}
	if len(updateColumns) == 0 {
		return "" // caller should use INSERT IGNORE instead
	}
	sets := make([]string, len(updateColumns))
	for i, c := range updateColumns {
		q := m.QuoteIdentifier(c)
		sets[i] = q + " = VALUES(" + q + ")"
	}
	return "ON DUPLICATE KEY UPDATE " + strings.Join(sets, ", ")
}

func (MySQL) BulkInsertSupported() bool { return true }
func (MySQL) SupportsReturning() bool   { return false }

func (MySQL) ValidateIdentifier(ident string) error {
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
