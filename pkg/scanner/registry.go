package scanner

import "sync"

// FastScanFunc is the signature of a code-generated per-row scanner.
// It calls rows.Scan(&dest.Field, &dest.Field, ...) directly — no Plan,
// no Assignments loop, no []any targets slice, no unsafe.Pointer arithmetic.
// This matches hand-written sqlx performance.
type FastScanFunc[T any] func(rows RowsSource, dest *T) error

// FastScanAllFunc is the multi-row variant: scans every remaining row into
// a freshly allocated []T. sizeHint pre-sizes the slice (e.g., from a LIMIT).
type FastScanAllFunc[T any] func(rows RowsSource, sizeHint int) ([]T, error)

// fastScanEntry bundles the single-row and multi-row generated functions
// for one (struct type, result-column-shape) pair.
type fastScanEntry[T any] struct {
	one FastScanFunc[T]
	all FastScanAllFunc[T]
}

// registry maps a uint64 cache key (the same structural hash used by
// compiledCache / scanPlanCache) to a fastScanEntry for some type T.
// The registry is global and populated via RegisterFastScan, typically
// called from init() in code-generated files (see pkg/scanner/gen).
//
// Why a sync.Map and not a ShardedLRU: the registry is write-once (each
// distinct shape is registered exactly once at program start, or lazily on
// first lookup) and read-many. sync.Map's optimized read path (atomic
// pointer load) is cheaper than an LRU Get for this access pattern, and
// there's no eviction needed — the set of registered shapes is finite and
// program-defined.
var registry sync.Map // uint64 key → any (fastScanEntry[T] for some T)

// RegisterFastScan associates a cache key with generated single-row and
// multi-row scanners for type T. Safe to call multiple times for the same
// key (last write wins); safe to call from multiple goroutines; safe to
// call from init().
//
// The cache key MUST be the same structural hash pkg/compiler.PreHash
// produces for the query shape this scanner handles. Mismatches silently
// route to the slow path (LookupFastScan returns false), so a wrong key is
// a performance bug, not a correctness bug — but tests should catch it.
//
// 'all' may be nil if only the single-row (First/FindByID) path is
// optimized; the multi-row (Find) path will fall back to the slow path.
func RegisterFastScan[T any](cacheKey uint64, one FastScanFunc[T], all FastScanAllFunc[T]) {
	registry.Store(cacheKey, fastScanEntry[T]{one: one, all: all})
}

// LookupFastScan returns the registered scanners for cacheKey, or zero
// values + false if none is registered. The type parameter T must match
// the type the scanner was registered with.
//
// Cost: one sync.Map.Load (~10ns) + one type assertion. Cheaper than even
// a single LRU Get.
func LookupFastScan[T any](cacheKey uint64) (FastScanFunc[T], FastScanAllFunc[T], bool) {
	v, ok := registry.Load(cacheKey)
	if !ok {
		return nil, nil, false
	}
	entry, ok := v.(fastScanEntry[T])
	if !ok {
		// Type mismatch — registered for a different T. Treat as not-found
		// rather than panicking; the slow path is correct, just slower.
		return nil, nil, false
	}
	return entry.one, entry.all, true
}
