// Package pool provides sync.Pool-backed object and buffer pools for the
// hot execution path: SQL string building, argument slices, and row-scan
// scratch space. All pools reset state on Put so callers never observe
// stale data on Get.
package pool

import (
	"bytes"
	"sync"
)

// BufferPool pools *bytes.Buffer for SQL string generation.
type BufferPool struct {
	pool sync.Pool
}

var Buffers = NewBufferPool()

func NewBufferPool() *BufferPool {
	return &BufferPool{
		pool: sync.Pool{New: func() any { return new(bytes.Buffer) }},
	}
}

func (p *BufferPool) Get() *bytes.Buffer {
	return p.pool.Get().(*bytes.Buffer)
}

func (p *BufferPool) Put(b *bytes.Buffer) {
	b.Reset()
	p.pool.Put(b)
}

// ArgsPool pools []any slices used to accumulate bound query parameters.
type ArgsPool struct {
	pool sync.Pool
}

var Args = NewArgsPool()

func NewArgsPool() *ArgsPool {
	return &ArgsPool{
		pool: sync.Pool{New: func() any { s := make([]any, 0, 8); return &s }},
	}
}

func (p *ArgsPool) Get() *[]any {
	return p.pool.Get().(*[]any)
}

func (p *ArgsPool) Put(s *[]any) {
	*s = (*s)[:0]
	p.pool.Put(s)
}

// Generic is a small typed wrapper around sync.Pool for any reusable object
// with a reset function, e.g. row-scan scratch buffers.
type Generic[T any] struct {
	pool  sync.Pool
	reset func(*T)
}

func NewGeneric[T any](newFn func() *T, reset func(*T)) *Generic[T] {
	return &Generic[T]{
		pool:  sync.Pool{New: func() any { return newFn() }},
		reset: reset,
	}
}

func (g *Generic[T]) Get() *T {
	return g.pool.Get().(*T)
}

func (g *Generic[T]) Put(v *T) {
	if g.reset != nil {
		g.reset(v)
	}
	g.pool.Put(v)
}
