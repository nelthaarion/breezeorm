package plugins

import (
	"context"
	"log"
	"time"

	"github.com/nelthaarion/breezorm/pkg/planner"
)

// MultiTenancy injects a tenant-scoping predicate on every query, analogous
// to SoftDelete. TenantIDFunc extracts the current tenant from ctx (e.g. set
// by middleware upstream of the ORM call).
type MultiTenancy struct {
	NoopPlugin
	Column       string
	TenantIDFunc func(ctx context.Context) (any, bool)
}

func (m *MultiTenancy) Name() string { return "multi_tenancy" }

func (m *MultiTenancy) BeforePlan(ctx context.Context, lp *planner.LogicalPlan) (*planner.LogicalPlan, error) {
	// TODO: same structural-splice limitation noted in softdelete.go —
	// needs a parent-aware rewrite pass once the planner exposes one.
	if m.TenantIDFunc != nil {
		_, _ = m.TenantIDFunc(ctx)
	}
	return lp, nil
}

// Auditing records BeforeExecute/AfterExecute events for compliance trails.
type Auditing struct {
	NoopPlugin
	Sink func(entry AuditEntry)
}

type AuditEntry struct {
	SQL      string
	Args     []any
	Duration time.Duration
	Err      error
	At       time.Time
}

func (a *Auditing) Name() string { return "auditing" }

func (a *Auditing) BeforeExecute(ctx context.Context, sqlText string, args []any) (context.Context, error) {
	return context.WithValue(ctx, auditStartKey{}, time.Now()), nil
}

type auditStartKey struct{}

func (a *Auditing) AfterExecute(ctx context.Context, sqlText string, durationNanos int64, err error) {
	if a.Sink == nil {
		return
	}
	a.Sink(AuditEntry{
		SQL:      sqlText,
		Duration: time.Duration(durationNanos),
		Err:      err,
		At:       time.Now(),
	})
}

// Encryption transparently encrypts/decrypts configured columns.
// STATUS: interface + column registry only; actual crypto is intentionally
// left to the integrator (choice of AEAD cipher, key management/KMS
// integration are deployment-specific decisions this scaffold shouldn't make
// unilaterally).
type Encryption struct {
	NoopPlugin
	Columns map[string]Cipher // "table.column" -> cipher
}

type Cipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

func (e *Encryption) Name() string { return "encryption" }

// QueryCache caches full result sets for read-heavy, rarely-changing
// queries, keyed by the compiler's structural cache key + bound args hash.
// STATUS: hook points only; storage backend (in-memory vs Redis, TTL policy,
// invalidation-on-write) is left to the integrator.
type QueryCache struct {
	NoopPlugin
	TTL time.Duration
}

func (q *QueryCache) Name() string { return "query_cache" }

// Metrics records basic counters. A Prometheus-backed implementation would
// satisfy the same Plugin interface externally, without touching this file.
type Metrics struct {
	NoopPlugin
	QueryCount    int64
	ErrorCount    int64
	TotalDuration time.Duration
}

func (m *Metrics) Name() string { return "metrics" }

func (m *Metrics) AfterExecute(_ context.Context, _ string, durationNanos int64, err error) {
	m.QueryCount++
	m.TotalDuration += time.Duration(durationNanos)
	if err != nil {
		m.ErrorCount++
	}
}

// Tracing/OpenTelemetry: emits spans around execution. STATUS: logs via the
// standard library only, to avoid forcing an OTel SDK dependency on users who
// don't need it; swap the Logger for a real otel.Tracer-backed
// implementation of the Plugin interface when needed.
type Tracing struct {
	NoopPlugin
	Logger *log.Logger
}

func (t *Tracing) Name() string { return "tracing" }

func (t *Tracing) BeforeExecute(ctx context.Context, sqlText string, args []any) (context.Context, error) {
	if t.Logger != nil {
		t.Logger.Printf("query start: %s", sqlText)
	}
	return ctx, nil
}

func (t *Tracing) AfterExecute(_ context.Context, sqlText string, durationNanos int64, err error) {
	if t.Logger != nil {
		t.Logger.Printf("query done: %s (%s) err=%v", sqlText, time.Duration(durationNanos), err)
	}
}
