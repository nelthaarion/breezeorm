package scanner

import (
	"testing"
)

func TestFastScan_ScanOneFast_DecodesSingleRow(t *testing.T) {
	type Tiny struct{ ID int64 }
	one := func(rows RowsSource, dest *Tiny) error {
		return rows.Scan(&dest.ID)
	}
	rows := &valueRows{idx: -1, rows: [][]any{{int64(7)}}}
	v, err := ScanOneFast[Tiny](rows, one)
	if err != nil {
		t.Fatalf("ScanOneFast: %v", err)
	}
	if v.ID != 7 {
		t.Errorf("got ID=%d, want 7", v.ID)
	}
}

func TestFastScan_ScanOneFast_NoRowsReturnsErrNoRows(t *testing.T) {
	type Tiny struct{ ID int64 }
	one := func(rows RowsSource, dest *Tiny) error { return rows.Scan(&dest.ID) }
	rows := &valueRows{idx: -1, rows: nil}
	_, err := ScanOneFast[Tiny](rows, one)
	if err == nil {
		t.Fatal("expected error for no rows, got nil")
	}
}

func TestFastScan_ScanAllHintFast_DecodesAllRows(t *testing.T) {
	type Tiny struct{ ID int64 }
	all := func(rows RowsSource, sizeHint int) ([]Tiny, error) {
		out := make([]Tiny, 0, sizeHint)
		for rows.Next() {
			var t Tiny
			if err := rows.Scan(&t.ID); err != nil {
				return nil, err
			}
			out = append(out, t)
		}
		return out, rows.Err()
	}
	rows := &valueRows{idx: -1, rows: [][]any{{int64(1)}, {int64(2)}, {int64(3)}}}
	out, err := ScanAllHintFast[Tiny](rows, all, 3)
	if err != nil {
		t.Fatalf("ScanAllHintFast: %v", err)
	}
	if len(out) != 3 || out[0].ID != 1 || out[2].ID != 3 {
		t.Errorf("got %v, want [1 2 3]", out)
	}
}

func TestLookupFastScan_ReturnsFalseForUnregisteredKey(t *testing.T) {
	_, _, ok := LookupFastScan[struct{}](0)
	if ok {
		t.Fatal("expected LookupFastScan to return false for unregistered key")
	}
}

func TestLookupFastScan_ReturnsFalseForWrongTypeParam(t *testing.T) {
	type A struct{ X int }
	type B struct{ X int }
	RegisterFastScan[A](999, func(r RowsSource, d *A) error { return nil }, nil)
	// Lookup with the wrong type param — must return false, not panic.
	_, _, ok := LookupFastScan[B](999)
	if ok {
		t.Fatal("expected LookupFastScan to return false when type param doesn't match")
	}
}

func TestLookupFastScan_ReturnsTrueForRegisteredKey(t *testing.T) {
	type C struct{ X int }
	one := func(r RowsSource, d *C) error { return nil }
	all := func(r RowsSource, sizeHint int) ([]C, error) { return nil, nil }
	RegisterFastScan[C](12345, one, all)
	gotOne, gotAll, ok := LookupFastScan[C](12345)
	if !ok {
		t.Fatal("expected LookupFastScan to return true for registered key")
	}
	if gotOne == nil || gotAll == nil {
		t.Fatal("expected non-nil scanners for registered key")
	}
}
