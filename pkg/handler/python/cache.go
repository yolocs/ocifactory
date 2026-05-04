package python

import (
	"sync"
	"time"

	"github.com/yolocs/ocifactory/pkg/oci"
)

// DefaultSimpleIndexCacheTTL is the default time entries live in the
// per-package simple-index cache. Picked short enough that a missed
// invalidation (e.g. an upload landing on a different replica) recovers
// quickly, long enough that pip's burst of /simple/<pkg>/ requests
// during a single resolution doesn't repeatedly hit the OCI backend.
const DefaultSimpleIndexCacheTTL = 60 * time.Second

// simpleIndexCache memoises the per-package file list returned by
// Registry.ListFiles. handlePackageIndex turns each entry into a
// pip-friendly link at render time, so caching the slice — not the
// rendered HTML — avoids tying entries to a specific request scheme/host.
//
// A ttl of zero or negative disables caching entirely; get always reports
// a miss and put is a no-op. The handler still calls invalidate
// unconditionally so adopting the cache is purely additive.
type simpleIndexCache struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]simpleIndexCacheEntry
	now     func() time.Time
}

type simpleIndexCacheEntry struct {
	files     []*oci.RepoFile
	expiresAt time.Time
}

func newSimpleIndexCache(ttl time.Duration) *simpleIndexCache {
	return &simpleIndexCache{
		ttl:     ttl,
		entries: map[string]simpleIndexCacheEntry{},
		now:     time.Now,
	}
}

func (c *simpleIndexCache) get(pkg string) ([]*oci.RepoFile, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[pkg]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.expiresAt) {
		delete(c.entries, pkg)
		return nil, false
	}
	return e.files, true
}

func (c *simpleIndexCache) put(pkg string, files []*oci.RepoFile) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[pkg] = simpleIndexCacheEntry{
		files:     files,
		expiresAt: c.now().Add(c.ttl),
	}
}

func (c *simpleIndexCache) invalidate(pkg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, pkg)
}
