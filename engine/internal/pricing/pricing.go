// Package pricing supplies the monthly rate table the analysis engine
// uses for savings math (docs/architecture.md §3.1).
//
// Three layers: a static default table (always available), a remote
// source (AWS Price List API), and wrappers that memoize and fall back —
// analysis must never fail because pricing is unreachable.
package pricing

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"consize/internal/analysis"
)

// Service supplies the current rate table.
type Service interface {
	Prices(ctx context.Context) (analysis.Prices, error)
}

// Static returns a fixed rate table from configuration.
type Static struct{ P analysis.Prices }

// Prices implements Service.
func (s Static) Prices(context.Context) (analysis.Prices, error) { return s.P, nil }

// DefaultStatic returns the shipped default table (GKE-style rates).
func DefaultStatic() analysis.Prices { return analysis.DefaultPrices() }

// Cached memoizes a source for a TTL — the AWS index is ~20 MB, so it is
// fetched at most once per TTL regardless of how often analysis runs.
type Cached struct {
	src Service
	ttl time.Duration

	mu       sync.Mutex
	fetched  time.Time
	cached   analysis.Prices
	cachedOK bool
}

// NewCached wraps src with a TTL cache.
func NewCached(src Service, ttl time.Duration) *Cached {
	return &Cached{src: src, ttl: ttl}
}

// Prices implements Service. A stale cache is refreshed in the caller's
// goroutine; concurrent callers share one fetch and one result.
func (c *Cached) Prices(ctx context.Context) (analysis.Prices, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedOK && time.Since(c.fetched) < c.ttl {
		return c.cached, nil
	}
	p, err := c.src.Prices(ctx)
	if err != nil {
		return analysis.Prices{}, err
	}
	c.cached, c.cachedOK = p, true
	c.fetched = time.Now()
	return p, nil
}

// Resilient prefers a remote source and falls back to a static table when
// it is unreachable — the engine keeps producing recommendations with
// clearly-stale-but-sane pricing instead of failing closed.
type Resilient struct {
	primary  Service
	fallback analysis.Prices
	Log      *slog.Logger
}

// NewResilient wraps primary with the given fallback table.
func NewResilient(primary Service, fallback analysis.Prices) *Resilient {
	return &Resilient{primary: primary, fallback: fallback, Log: slog.Default()}
}

// Prices implements Service.
func (r *Resilient) Prices(ctx context.Context) (analysis.Prices, error) {
	p, err := r.primary.Prices(ctx)
	if err != nil {
		r.Log.Warn("pricing: primary source failed, using static fallback", "err", err)
		return r.fallback, nil
	}
	return p, nil
}
