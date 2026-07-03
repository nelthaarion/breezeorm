// pkg/scanner/registry.go
package scanner

import "sync"

// fastScanRegistry holds generated FastScanFuncs, keyed by the same
// uint64 CacheKey the scan-Plan cache already uses (compiler.CompiledQuery
// .CacheKey — same query shape → same generated function). Populated by
// generated init() funcs, e.g.:
//   func init() { scanner.RegisterFastScan[gen.User](someCacheKey, gen.ScanUser) }
var fastScanRegistry sync.Map // uint64 -> any (FastScanFunc[T], type-erased)

func RegisterFastScan[T any](key uint64, fn FastScanFunc[T]) {
	fastScanRegistry.Store(key, fn)
}

func LookupFastScan[T any](key uint64) (FastScanFunc[T], bool) {
	v, ok := fastScanRegistry.Load(key)
	if !ok {
		return nil, false
	}
	fn, ok := v.(FastScanFunc[T])
	return fn, ok
}
