package namespace

import (
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/yolocs/ocifactory/pkg/auth"
)

// DefaultPolicyCacheTTL is the default lifetime of a single entry in
// the data-plane authz policy cache. Picked short enough that an
// operator's `PUT namespace` lands in front of in-flight clients
// within a one-minute window without explicit invalidation, long
// enough that a hot namespace pulling thousands of artifacts a minute
// pays for one Store.Get + Policy compile rather than one per request.
const DefaultPolicyCacheTTL = 60 * time.Second

// DefaultPolicyCacheSize bounds the number of distinct namespaces the
// policy cache holds. Sized generously so even multi-tenant
// deployments with hundreds of namespaces never evict in practice;
// the cap exists to put a hard ceiling on memory rather than as a
// tuning knob. A value <= 0 disables the size bound for the
// underlying LRU and is rejected by [NewRegistry].
const DefaultPolicyCacheSize = 1024

// cachedPolicy is the value stored per namespace name in the policy
// cache. notFound carries a negative result from [Store.Get] so a
// burst of requests against a missing namespace doesn't repeatedly
// hit the metadata backend.
type cachedPolicy struct {
	authorizer auth.Authorizer
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
