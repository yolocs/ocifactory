package namespace

import (
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/yolocs/ocifactory/pkg/auth"
)

// cachedPolicy is the value stored per namespace name in the policy
// cache. notFound carries a negative result from [Store.Get] so a
// burst of requests against a missing namespace doesn't repeatedly
// hit the metadata backend.
//
// spec is the raw [Spec] body the authorizer was compiled from. It is
// held alongside the authorizer so format handlers can dispatch on
// [Spec.Mode] / read [Spec.Proxy] without paying a second
// [Store.Get]; spec and authorizer always reflect the same point-in-time
// document. The cache value is treated as immutable — callers receive
// the pointer for free read access and must not mutate it.
type cachedPolicy struct {
	authorizer auth.Authorizer
	spec       *Spec
	notFound   bool
}

// policyCache memoises the per-namespace [auth.Authorizer] compiled
// from the namespace's Policy. Backed by hashicorp/golang-lru's
// expirable LRU, which gives us TTL expiry plus a bounded entry count
// for free.
//
// A ttl of zero or negative disables caching entirely; lookup always
// reports a miss. The wrapper's Invalidate path is still wired so
// flipping TTL on at startup does not require code changes elsewhere.
type policyCache struct {
	lru *expirable.LRU[string, cachedPolicy]
}

func newPolicyCache(size int, ttl time.Duration) *policyCache {
	if ttl <= 0 {
		return &policyCache{}
	}
	return &policyCache{
		lru: expirable.NewLRU[string, cachedPolicy](size, nil, ttl),
	}
}

func (c *policyCache) get(name string) (cachedPolicy, bool) {
	if c.lru == nil {
		return cachedPolicy{}, false
	}
	return c.lru.Get(name)
}

func (c *policyCache) put(name string, v cachedPolicy) {
	if c.lru == nil {
		return
	}
	c.lru.Add(name, v)
}

func (c *policyCache) invalidate(name string) {
	if c.lru == nil {
		return
	}
	c.lru.Remove(name)
}

func (c *policyCache) purge() {
	if c.lru == nil {
		return
	}
	c.lru.Purge()
}
