package indexcache

import (
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

const (
	// DefaultNegativeCacheSize is the entry cap a [NegativeCache]
	// uses when [WithNegativeCacheSize] is not supplied. Sized to
	// hold a typical proxy namespace's worth of recent 404s without
	// noticeably accruing memory.
	DefaultNegativeCacheSize = 1024

	// DefaultNegativeCacheTTL is the lifetime each [NegativeCache]
	// entry retains when [WithNegativeCacheTTL] is not supplied.
	// Short enough that an upstream republish becomes observable
	// within a minute; long enough to absorb a misbehaving client
	// looping on the same bad URL.
	DefaultNegativeCacheTTL = 60 * time.Second
)

// NegativeCache remembers upstream lookups that recently returned a
// definitive "not found" so per-format proxy fetchers can suppress
// duplicate upstream calls for the same key.
//
// Keys are opaque to the cache. The convention is the upstream URL
// the fetcher would otherwise call, but a fetcher may key on a
// composed tuple (format, package, version, file) if that better
// matches its address space.
//
// A NegativeCache is safe for concurrent use. A zero value is not
// usable; construct with [NewNegativeCache].
type NegativeCache struct {
	lru *expirable.LRU[string, struct{}]
}

// NegativeCacheOption configures a [NegativeCache] at construction time.
type NegativeCacheOption func(*negativeCacheConfig)

type negativeCacheConfig struct {
	size int
	ttl  time.Duration
}

// WithNegativeCacheSize overrides [DefaultNegativeCacheSize]. A
// non-positive size falls back to the default; this matches what
// hashicorp/golang-lru does internally and keeps misconfiguration
// from silently producing a zero-capacity cache.
func WithNegativeCacheSize(size int) NegativeCacheOption {
	return func(c *negativeCacheConfig) {
		if size > 0 {
			c.size = size
		}
	}
}

// WithNegativeCacheTTL overrides [DefaultNegativeCacheTTL]. A
// non-positive TTL falls back to the default.
func WithNegativeCacheTTL(ttl time.Duration) NegativeCacheOption {
	return func(c *negativeCacheConfig) {
		if ttl > 0 {
			c.ttl = ttl
		}
	}
}

// NewNegativeCache returns a [NegativeCache] ready for use. Entries
// expire after the TTL or are evicted in LRU order once the size cap
// is reached, whichever comes first. The expirable LRU runs its own
// eviction goroutine; there is nothing for the caller to start or
// stop.
func NewNegativeCache(opts ...NegativeCacheOption) *NegativeCache {
	cfg := negativeCacheConfig{
		size: DefaultNegativeCacheSize,
		ttl:  DefaultNegativeCacheTTL,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return &NegativeCache{
		lru: expirable.NewLRU[string, struct{}](cfg.size, nil, cfg.ttl),
	}
}

// RecordMiss marks key as known-missing. Repeated calls refresh the
// entry's TTL, so a client looping on the same bad URL keeps the
// suppression active for as long as the loop runs.
func (c *NegativeCache) RecordMiss(key string) {
	c.lru.Add(key, struct{}{})
}

// IsKnownMissing reports whether key was recorded as missing within
// the current TTL window. A cache miss (never recorded, or expired)
// returns false so the caller proceeds with the upstream lookup.
func (c *NegativeCache) IsKnownMissing(key string) bool {
	_, ok := c.lru.Get(key)
	return ok
}

// Len returns the current number of live entries. Intended for the
// `proxy_negative_cache_size` gauge; the metric registration lives
// at the call site so the `format` label is set by the per-format
// proxy that owns the cache.
func (c *NegativeCache) Len() int {
	return c.lru.Len()
}

// Purge removes every entry. Provided for tests and operator-driven
// cache flushes; production code paths use TTL expiry.
func (c *NegativeCache) Purge() {
	c.lru.Purge()
}
