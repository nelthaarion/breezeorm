// Package metadata implements the ORM's metadata compiler.
//
// Design goals:
//   - Reflection happens exactly once per model type (sync.Once).
//   - The resulting Table is immutable and safe for concurrent lock-free reads.
//   - Field offsets are precomputed so the scanner never needs reflection
//     again at execution time (see pkg/scanner).
package metadata

import (
        "fmt"
        "reflect"
        "strings"
        "sync"
        "sync/atomic"
        "unsafe"
)

// Column describes a single mapped struct field.
type Column struct {
        Name        string       // DB column name
        FieldName   string       // Go struct field name
        FieldIndex  []int        // reflect.Type.FieldByIndex path (handles embedding)
        Offset      uintptr      // precomputed unsafe offset from struct base
        Type        reflect.Type // Go field type
        IsPK        bool
        IsAutoIncr  bool
        IsNullable  bool
        IsGenerated bool
        IsUnique    bool
        Default     string
        Tag         Tag
}

// Relationship describes a HasOne / HasMany / BelongsTo / ManyToMany edge.
type Relationship struct {
        Name         string
        Kind         RelationKind
        FieldIndex   []int
        Related      reflect.Type
        ForeignKey   []string // composite FK supported: >1 element
        References   []string
        JoinTable    string // ManyToMany only
        JoinFK       string
        JoinRefFK    string
        SelfReferent bool
}

type RelationKind uint8

const (
        RelationHasOne RelationKind = iota
        RelationHasMany
        RelationBelongsTo
        RelationManyToMany
)

// Index describes a declared index (from struct tags or explicit registration).
type Index struct {
        Name       string
        Columns    []string
        Unique     bool
        Partial    string // partial index predicate, dialect-specific raw SQL
        Expression string // functional/expression index
}

// Constraint describes check/unique/foreign-key constraints beyond PK/FK columns.
type Constraint struct {
        Name string
        Kind string // "check", "unique", "foreign_key"
        Expr string
}

// Table is the fully compiled, immutable metadata for a model type.
// Once built, a Table is never mutated — safe for lock-free concurrent reads.
type Table struct {
        Name         string
        GoType       reflect.Type
        Columns      []Column
        ColumnByName map[string]*Column
        PKColumns    []*Column
        Relations    []Relationship
        Indexes      []Index
        Constraints  []Constraint
        Size         uintptr // unsafe.Sizeof(T) — used by scanner for unsafe field access bounds checks
}

// registry caches one compiled Table per reflect.Type, compiled exactly once.
//
// Lock-free reads: the entry map is held behind an atomic.Pointer, so the
// common path (type already compiled, which is every call after the first
// for a given model type) is a single atomic load + map read with zero
// mutex contention. Writes are copy-on-write (build a new map, atomic
// pointer swap) and serialized behind writeMu — the same pattern
// pkg/cache.Cache uses. The previous RWMutex-based version took an RLock
// on every orm.Model[T](db) call, which under heavy concurrent load with
// many goroutines spinning up fresh queries showed up as real read-lock
// contention for no correctness benefit (the map is written once per type
// and never mutated after).
type registry struct {
        snapshot atomic.Pointer[map[reflect.Type]*entry]
        writeMu  sync.Mutex
}

type entry struct {
        once  sync.Once
        table *Table
        err   error
}

var globalRegistry = func() *registry {
        r := &registry{}
        empty := map[reflect.Type]*entry{}
        r.snapshot.Store(&empty)
        return r
}()

// Compile returns the compiled, cached Table metadata for T.
// The struct is reflected on exactly once per process, on first call;
// every subsequent call (from any goroutine) is a lock-free map read
// followed by returning the cached, immutable *Table.
func Compile[T any]() (*Table, error) {
        // reflect.TypeOf((*T)(nil)).Elem() instead of reflect.TypeOf(zero-value-T):
        // converting a *T (even a nil one) to `any` is a free pointer-shaped
        // interface conversion, whereas boxing a zero-value T directly would
        // heap-allocate a full copy of T just to read its type descriptor — on
        // every single Compile[T]() call, i.e. every Model[T](db), even though
        // the *Table lookup below is otherwise a cached, allocation-free map
        // read. The pointer/value stripping loop right after already handles
        // T itself being a pointer type, so starting one level higher (*T) here
        // doesn't change what type ultimately gets looked up.
        t := reflect.TypeOf((*T)(nil)).Elem()
        for t != nil && t.Kind() == reflect.Ptr {
                t = t.Elem()
        }
        if t == nil {
                return nil, fmt.Errorf("metadata: cannot compile nil type")
        }
        if t.Kind() != reflect.Struct {
                return nil, fmt.Errorf("metadata: %s is not a struct", t)
        }

        e := globalRegistry.getOrCreateEntry(t)
        e.once.Do(func() {
                e.table, e.err = compileStruct(t)
        })
        return e.table, e.err
}

func (r *registry) getOrCreateEntry(t reflect.Type) *entry {
        // Lock-free fast path: atomic pointer load + map read. No mutex at all
        // on the common (already-compiled) path. This is the hot path — every
        // orm.Model[T](db) call hits this.
        m := *r.snapshot.Load()
        if e, ok := m[t]; ok {
                return e
        }
        // Slow path: type not yet registered. Serialize writers so we don't
        // build N throwaway maps for N concurrent first-time callers for the
        // same new type. Re-check under the write lock to avoid duplicate
        // registration if another goroutine won the race.
        r.writeMu.Lock()
        defer r.writeMu.Unlock()
        m = *r.snapshot.Load()
        if e, ok := m[t]; ok {
                return e
        }
        e := &entry{}
        next := make(map[reflect.Type]*entry, len(m)+1)
        for k, v := range m {
                next[k] = v
        }
        next[t] = e
        r.snapshot.Store(&next)
        return e
}

func compileStruct(t reflect.Type) (*Table, error) {
        tbl := &Table{
                Name:         toSnakeCasePlural(t.Name()),
                GoType:       t,
                ColumnByName: make(map[string]*Column),
                Size:         t.Size(),
        }

        if named, ok := reflect.New(t).Interface().(interface{ TableName() string }); ok {
                tbl.Name = named.TableName()
        }

        if err := walkFields(t, nil, tbl); err != nil {
                return nil, err
        }

        for i := range tbl.Columns {
                c := &tbl.Columns[i]
                tbl.ColumnByName[c.Name] = c
                if c.IsPK {
                        tbl.PKColumns = append(tbl.PKColumns, c)
                }
        }
        if len(tbl.PKColumns) == 0 {
                if c, ok := tbl.ColumnByName["id"]; ok {
                        c.IsPK = true
                        tbl.PKColumns = append(tbl.PKColumns, c)
                }
        }
        return tbl, nil
}

func walkFields(t reflect.Type, prefixIndex []int, tbl *Table) error {
        for i := 0; i < t.NumField(); i++ {
                f := t.Field(i)
                if f.PkgPath != "" && !f.Anonymous {
                        continue // unexported
                }
                tag := parseTag(f.Tag.Get("db"))
                if tag.Skip {
                        continue
                }

                idx := append(append([]int{}, prefixIndex...), i)

                // Embedded struct: flatten (unless it's a relation-tagged field).
                if f.Anonymous && f.Type.Kind() == reflect.Struct && tag.Relation == "" {
                        if err := walkFields(f.Type, idx, tbl); err != nil {
                                return err
                        }
                        continue
                }

                // Relationship field (struct or slice-of-struct with a relation tag,
                // or naturally inferred by kind).
                if kind, ok := detectRelationKind(f, tag); ok {
                        related := f.Type
                        if related.Kind() == reflect.Slice || related.Kind() == reflect.Ptr {
                                related = related.Elem()
                        }
                        tbl.Relations = append(tbl.Relations, Relationship{
                                Name:       f.Name,
                                Kind:       kind,
                                FieldIndex: idx,
                                Related:    related,
                                ForeignKey: tag.ForeignKey,
                                References: tag.References,
                                JoinTable:  tag.JoinTable,
                        })
                        continue
                }

                name := tag.Column
                if name == "" {
                        name = toSnakeCase(f.Name)
                }

                col := Column{
                        Name:        name,
                        FieldName:   f.Name,
                        FieldIndex:  idx,
                        Offset:      f.Offset, // valid for top-level; nested handled via FieldIndex in scanner
                        Type:        f.Type,
                        IsPK:        tag.PrimaryKey,
                        IsAutoIncr:  tag.AutoIncrement,
                        IsNullable:  f.Type.Kind() == reflect.Ptr || tag.Nullable,
                        IsGenerated: tag.Generated,
                        IsUnique:    tag.Unique,
                        Default:     tag.Default,
                        Tag:         tag,
                }
                tbl.Columns = append(tbl.Columns, col)
        }
        return nil
}

func detectRelationKind(f reflect.StructField, tag Tag) (RelationKind, bool) {
        switch tag.Relation {
        case "has_one":
                return RelationHasOne, true
        case "has_many":
                return RelationHasMany, true
        case "belongs_to":
                return RelationBelongsTo, true
        case "many_to_many":
                return RelationManyToMany, true
        }
        // Heuristic fallback: slice-of-struct => has_many, struct/ptr-to-struct
        // with no db tag and a matching *ID field elsewhere => left for explicit
        // tags in production use; heuristics intentionally conservative here.
        return 0, false
}

// FieldPointer returns an unsafe.Pointer to this column's field within a
// struct value pointed to by base. Used by the zero-reflection scan path.
func (c *Column) FieldPointer(base unsafe.Pointer) unsafe.Pointer {
        return unsafe.Pointer(uintptr(base) + c.Offset)
}

func toSnakeCase(s string) string {
        var b strings.Builder
        for i, r := range s {
                if i > 0 && r >= 'A' && r <= 'Z' {
                        b.WriteByte('_')
                }
                b.WriteRune(r)
        }
        return strings.ToLower(b.String())
}

func toSnakeCasePlural(s string) string {
        sc := toSnakeCase(s)
        if strings.HasSuffix(sc, "s") {
                return sc + "es"
        }
        if strings.HasSuffix(sc, "y") {
                return sc[:len(sc)-1] + "ies"
        }
        return sc + "s"
}
