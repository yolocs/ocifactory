package indexcache

import (
	"context"
	"sync"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
)

// InMemoryTargets is a [TargetFactory] backed by per-repo
// [memory.Store] instances. One store is created on first access for
// each unique repoPath, so namespace and package isolation surface
// naturally — two cache entries in different namespaces (or in the
// same namespace but different packages) live in different stores,
// exactly like they would on a real OCI backend.
//
// It exists for tests in other packages that need a working [Cache]
// without standing up a real OCI server. Production wiring uses
// [NewCache] with a real registry URL.
//
// A zero value is not usable; construct via [NewInMemoryTargets].
type InMemoryTargets struct {
	mu     sync.Mutex
	stores map[string]*memory.Store
}

// NewInMemoryTargets returns an [InMemoryTargets] ready for use.
func NewInMemoryTargets() *InMemoryTargets {
	return &InMemoryTargets{stores: map[string]*memory.Store{}}
}

// Open is the [TargetFactory] entry point. It is safe for concurrent
// use; the same repoPath always returns the same underlying store.
func (f *InMemoryTargets) Open(_ context.Context, repoPath string) (oras.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.stores[repoPath]; ok {
		return s, nil
	}
	s := memory.New()
	f.stores[repoPath] = s
	return s, nil
}

// NewInMemoryCache returns a [Cache] wired to a fresh
// [InMemoryTargets]. Convenient one-liner for tests that don't care
// about reusing the target factory across multiple caches.
func NewInMemoryCache(opts ...Option) (*Cache, error) {
	return NewCacheWithTargets(NewInMemoryTargets().Open, opts...)
}
