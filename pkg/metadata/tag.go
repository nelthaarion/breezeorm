package metadata

import "strings"

// Tag is the parsed representation of a `db:"..."` struct tag, e.g.:
//
//	db:"email,unique"
//	db:"id,pk,autoincrement"
//	db:"-"
//	db:"posts,relation=has_many,fk=author_id"
type Tag struct {
	Column        string
	Skip          bool
	PrimaryKey    bool
	AutoIncrement bool
	Nullable      bool
	Unique        bool
	Generated     bool
	Default       string
	Relation      string
	ForeignKey    []string
	References    []string
	JoinTable     string
}

func parseTag(raw string) Tag {
	var t Tag
	if raw == "-" {
		t.Skip = true
		return t
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 0 {
		t.Column = strings.TrimSpace(parts[0])
	}
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		switch {
		case p == "pk" || p == "primarykey":
			t.PrimaryKey = true
		case p == "autoincrement" || p == "auto_increment":
			t.AutoIncrement = true
		case p == "nullable":
			t.Nullable = true
		case p == "unique":
			t.Unique = true
		case p == "generated":
			t.Generated = true
		case strings.HasPrefix(p, "default="):
			t.Default = strings.TrimPrefix(p, "default=")
		case strings.HasPrefix(p, "relation="):
			t.Relation = strings.TrimPrefix(p, "relation=")
		case strings.HasPrefix(p, "fk="):
			t.ForeignKey = strings.Split(strings.TrimPrefix(p, "fk="), ";")
		case strings.HasPrefix(p, "references="):
			t.References = strings.Split(strings.TrimPrefix(p, "references="), ";")
		case strings.HasPrefix(p, "join="):
			t.JoinTable = strings.TrimPrefix(p, "join=")
		}
	}
	return t
}
