package metadata

import "testing"

type Post struct {
	ID       int64  `db:"id,pk,autoincrement"`
	Title    string `db:"title"`
	AuthorID int64  `db:"author_id"`
}

func TestCompile_ColumnsAndPK(t *testing.T) {
	tbl, err := Compile[Post]()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if tbl.Name != "posts" {
		t.Errorf("table name = %q, want posts", tbl.Name)
	}
	if len(tbl.Columns) != 3 {
		t.Fatalf("got %d columns, want 3", len(tbl.Columns))
	}
	if len(tbl.PKColumns) != 1 || tbl.PKColumns[0].Name != "id" {
		t.Errorf("PK columns = %+v, want [id]", tbl.PKColumns)
	}
	col, ok := tbl.ColumnByName["author_id"]
	if !ok {
		t.Fatal("expected author_id column")
	}
	if col.FieldName != "AuthorID" {
		t.Errorf("FieldName = %q, want AuthorID", col.FieldName)
	}
}

func TestCompile_CachedAcrossCalls(t *testing.T) {
	t1, _ := Compile[Post]()
	t2, _ := Compile[Post]()
	if t1 != t2 {
		t.Error("expected identical *Table pointer from cache on repeat Compile calls")
	}
}

func TestCompile_RejectsNonStruct(t *testing.T) {
	if _, err := Compile[int](); err == nil {
		t.Error("expected error compiling non-struct type")
	}
}
