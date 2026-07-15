// Package compiler wires together the planner and optimizer into the
// query-compilation pipeline: query.Builder -> LogicalPlan -> optimized
// LogicalPlan -> PhysicalPlan. The result is cacheable by CacheKey so that
// structurally identical queries (same shape, different literal values)
// reuse the same plan and, downstream, the same prepared statement.
package compiler

import (
        "context"
        "fmt"
        "hash/maphash"
        "sync"

        "github.com/nelthaarion/breezeorm/pkg/dialect"
        "github.com/nelthaarion/breezeorm/pkg/optimizer"
        "github.com/nelthaarion/breezeorm/pkg/planner"
        "github.com/nelthaarion/breezeorm/pkg/plugins"
        "github.com/nelthaarion/breezeorm/pkg/query"
        "github.com/nelthaarion/breezeorm/pkg/sqlgen"
)

// CompiledQuery is the immutable output of the compilation pipeline, ready
// for SQL generation and execution.
//
// SQL text is computed lazily on first call to SQL() and cached in sqlText
// via sqlOnce. This replaces the old pkg/execution.Executor.planTextCache,
// which was a separate bounded LRU keyed by CacheKey. Storing the SQL text
// directly on CompiledQuery eliminates one cache layer (one mutex
// acquisition, one map lookup, one list.MoveToFront) per query, with no
// downside: the CompiledQuery is already cached by CacheKey in
// DB.compiledCache, so the SQL text is implicitly cached too.
type CompiledQuery struct {
        Physical *planner.PhysicalPlan
        CacheKey uint64

        // LimitHint is pre-extracted from the LogicalPlan at compile time so
        // that scanner.ScanAllHint can pre-size its []T output slice without
        // re-walking the plan tree on every Find call. 0 means "no LIMIT".
        LimitHint int

        sqlOnce sync.Once
        sqlText string
        sqlErr  error
}

// SQL returns the rendered SQL text for this CompiledQuery, computing it
// once on first call and caching the result. Subsequent calls return the
// cached string with zero overhead (a single atomic.Load on the sync.Once
// + a struct-field read).
//
// This method is safe for concurrent use: sync.Once guarantees the
// generation runs exactly once even if multiple goroutines call SQL()
// simultaneously on a freshly-compiled CompiledQuery.
func (cq *CompiledQuery) SQL() (string, error) {
        cq.sqlOnce.Do(func() {
                gen, err := sqlgen.GenerateSQL(cq.Physical)
                if err != nil {
                        cq.sqlErr = err
                        return
                }
                cq.sqlText = gen.SQL
        })
        return cq.sqlText, cq.sqlErr
}

// Compile runs the full pipeline for a builder against a target dialect:
// logical plan construction, plugin BeforePlan rewrites (soft delete,
// multi-tenancy, etc.), optimizer passes, and physical planning.
func Compile[T any](ctx context.Context, b query.Builder[T], d dialect.Dialect, passes []optimizer.Pass, chain plugins.Chain) (*CompiledQuery, error) {
        lp := planner.Build(b)

        lp, err := chain.RunBeforePlan(ctx, lp)
        if err != nil {
                return nil, fmt.Errorf("compiler: plugin BeforePlan: %w", err)
        }

        lp = optimizer.Optimize(lp, passes)
        pp := planner.PlanPhysical(lp, d)

        return &CompiledQuery{
                Physical:  pp,
                CacheKey:  structuralHash(pp.Logical.Root, d.Name()),
                LimitHint: extractLimitHint(pp),
        }, nil
}

// extractLimitHint walks the (just-compiled) physical plan's node tree
// looking for a NodeLimit, and returns its Limit value. This is a
// compile-time cost (paid once per distinct query shape, then cached on
// the CompiledQuery), so that Find doesn't have to re-walk the tree on
// every call just to pre-size its []T output slice.
func extractLimitHint(pp *planner.PhysicalPlan) int {
        if pp == nil || pp.Logical == nil {
                return 0
        }
        for n := pp.Logical.Root; n != nil; n = n.Input {
                if n.Kind == planner.NodeLimit && n.Limit != nil {
                        return int(*n.Limit)
                }
        }
        return 0
}

// hashSeed is fixed once per process (chosen randomly by maphash on first
// use) and reused by every hash below. This is deliberate, not an oversight:
// hash/maphash's zero value picks a NEW random seed on first write if you
// don't set one explicitly, which would make structuralHash/PreHash return a
// different value every call for the identical input — silently breaking
// every cache keyed by it. A single fixed-per-process seed keeps hashing
// stable within a process (so caching works) while still randomizing across
// process restarts (so the hash isn't a static target for anyone trying to
// force cache-key collisions from outside).
var hashSeed = maphash.MakeSeed()

// Separator bytes between logically distinct fields, so e.g. hashing
// ("ab", "c") can never collide with ("a", "bc"). Arbitrary values outside
// normal identifier character ranges, chosen just to be distinct from each
// other and from likely table/column name content.
const (
        sepField byte = 0x1f // unit separator
        sepOpen  byte = 0x1c // "open paren" marker for nested expressions
        sepClose byte = 0x1d // "close paren" marker
)

// structuralHash produces a cache key that depends only on query *shape*
// (table names, operator tree structure, column names, operators) — never on
// bound literal values — so that e.g. `Where(Eq("id", 1))` and
// `Where(Eq("id", 2))` hit the same cached plan and prepared statement.
//
// PERFORMANCE NOTE: this used to be built with crypto/sha256 + fmt.Fprintf +
// hex.EncodeToString. Profiling a real benchmark run showed that combination
// costing ~11% of total per-query CPU time — for a cache *lookup key*,
// computed on every single call whether or not the cache hit. fmt.Fprintf is
// reflection-based and was the majority of that cost; crypto/sha256 is
// unnecessary cryptographic strength for an in-process, non-adversarial (or
// at worst hash-flooding-resistant-via-seeding, which maphash already gives
// us) cache key; hex.EncodeToString added an allocation to turn the digest
// into a string key when a plain uint64 works fine as a map key. Rewritten
// with hash/maphash (fast, seeded) and direct WriteString/WriteByte calls
// (zero allocation for string fields, since maphash.Hash.WriteString takes a
// string directly instead of forcing a []byte conversion).
func structuralHash(n *planner.LogicalNode, dialectName string) uint64 {
        var h maphash.Hash
        h.SetSeed(hashSeed)
        h.WriteString(dialectName)
        h.WriteByte(sepField)
        writeNode(&h, n)
        return h.Sum64()
}

func writeNode(h *maphash.Hash, n *planner.LogicalNode) {
        if n == nil {
                h.WriteByte(0) // distinct single-byte marker for "no node"
                return
        }
        h.WriteByte(byte(n.Kind))
        h.WriteByte(sepField)
        h.WriteString(n.Table)
        h.WriteByte(sepField)
        h.WriteString(n.Alias)
        h.WriteByte(sepField)
        writeExpr(h, n.Predicate)
        writeExpr(h, n.Having)
        for _, g := range n.GroupBy {
                h.WriteString(g)
                h.WriteByte(sepField)
        }
        for _, o := range n.OrderBy {
                h.WriteString(o.Column)
                writeBool(h, o.Desc)
        }
        // Limit/Offset: presence only, never the bound value — the value is a
        // bind param, not part of the query's shape.
        writeBool(h, n.Limit != nil)
        writeBool(h, n.Offset != nil)
        for _, p := range n.Projections {
                h.WriteString(p.Expr)
                h.WriteByte(sepField)
                h.WriteString(p.Alias)
                h.WriteByte(sepField)
        }
        writeNode(h, n.Input)
        for _, in := range n.Inputs {
                writeNode(h, in)
        }
}

func writeExpr(h *maphash.Hash, e query.Expr) {
        switch v := e.(type) {
        case nil:
                h.WriteByte(0)
        case query.Predicate:
                h.WriteByte('p')
                h.WriteString(v.Column)
                h.WriteByte(sepField)
                h.WriteString(string(v.Op))
                h.WriteByte(sepField)
        case query.LogicalExpr:
                h.WriteByte('l')
                h.WriteString(string(v.Op))
                h.WriteByte(sepOpen)
                for _, c := range v.Children {
                        writeExpr(h, c)
                }
                h.WriteByte(sepClose)
        case query.RawExpr:
                h.WriteByte('r')
                h.WriteString(v.SQL)
                h.WriteByte(sepField)
        default:
                h.WriteByte('?')
        }
}

func writeBool(h *maphash.Hash, b bool) {
        if b {
                h.WriteByte(1)
        } else {
                h.WriteByte(0)
        }
}
