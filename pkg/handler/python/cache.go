package python

import (
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// DefaultSimpleIndexCacheTTL is the default time entries live in the
// per-package simple-index cache. Picked short enough that a missed
// invalidation (e.g. an upload landing on a different replica) recovers
// quickly, long enough that pip's burst of /simple/<pkg>/ requests
// during a single resolution doesn't repeatedly hit the OCI backend.
const DefaultSimpleIndexCacheTTL = 60 * time.Second

// simpleIndexCacheSize bounds how many distinct packages we hold cached
// entries for before LRU eviction kicks in. Sized generously: even a
// large monorepo pulling from a private PyPI is unlikely to touch tens
// of thousands of distinct package names in one TTL window, so this
// effectively caps memory at a known constant without ever evicting in
// practice. If we ever grow this into a public Phase-4 pull-through
// proxy, revisit.
const simpleIndexCacheSize = 4096

// cachedFile is the per-file payload we memoise from a single
// ListFiles. The render path turns each entry into an indexFile
// (HTML or JSON) by attaching a request-specific URL — caching this
// slice (instead of rendered output) avoids tying entries to a
// particular request scheme/host and covers both HTML and JSON
// renderings off a single cache miss.
type cachedFile struct {
	Filename  string
	OwningTag string
	Sha256    string
}

// simpleIndexCache memoises the per-package simple-index payload. The
// renderer pivots between HTML and JSON off the cached []cachedFile
// without needing to re-fetch the OCI backend.
//
// Backed by hashicorp/golang-lru's expirable LRU, which gives us TTL
// expiry plus a bounded entry count for free. A ttl of zero or negative
// disables caching entirely; get always reports a miss and put is a
// no-op. The handler still calls invalidate unconditionally so adopting
// the cache is purely additive at the call sites.
type simpleIndexCache struct {
	lru *expirable.LRU[string, []cachedFile]
}

func newSimpleIndexCache(ttl time.Duration) *simpleIndexCache {
	if ttl <= 0 {
		return &simpleIndexCache{}
	}
	return &simpleIndexCache{
		lru: expirable.NewLRU[string, []cachedFile](simpleIndexCacheSize, nil, ttl),
	}
}

func (c *simpleIndexCache) get(pkg string) ([]cachedFile, bool) {
	if c.lru == nil {
		return nil, false
	}
	return c.lru.Get(pkg)
}

func (c *simpleIndexCache) put(pkg string, files []cachedFile) {
	if c.lru == nil {
		return
	}
	c.lru.Add(pkg, files)
}

func (c *simpleIndexCache) invalidate(pkg string) {
	if c.lru == nil {
		return
	}
	c.lru.Remove(pkg)
}
