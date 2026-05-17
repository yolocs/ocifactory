package indexcache

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestNegativeCache_HitAfterRecordMiss(t *testing.T) {
	t.Parallel()

	c := NewNegativeCache()

	const key = "https://pypi.example.com/simple/does-not-exist/"
	if c.IsKnownMissing(key) {
		t.Fatalf("IsKnownMissing(%q) = true before RecordMiss, want false", key)
	}
	c.RecordMiss(key)
	if !c.IsKnownMissing(key) {
		t.Errorf("IsKnownMissing(%q) = false after RecordMiss, want true", key)
	}
}

// TestNegativeCache_TTLExpiry verifies the documented contract that an
// entry stops suppressing once its TTL has lapsed. A short TTL plus a
// real time.Sleep is the cheapest faithful test — the expirable LRU
// owns its own clock and there's no seam to inject a fake.
func TestNegativeCache_TTLExpiry(t *testing.T) {
	t.Parallel()

	const ttl = 50 * time.Millisecond
	c := NewNegativeCache(WithNegativeCacheTTL(ttl))

	const key = "https://pypi.example.com/simple/temp-404/"
	c.RecordMiss(key)
	if !c.IsKnownMissing(key) {
		t.Fatalf("IsKnownMissing(%q) immediately after RecordMiss = false, want true", key)
	}

	// Sleep well past the TTL so the eviction goroutine has time to
	// reap the entry; expirable's reaper runs at the TTL interval.
	time.Sleep(ttl * 4)

	if c.IsKnownMissing(key) {
		t.Errorf("IsKnownMissing(%q) after TTL = true, want false", key)
	}
}

// TestNegativeCache_SizeEviction confirms the LRU honors its size cap
// by evicting the oldest entry once the cap is exceeded.
func TestNegativeCache_SizeEviction(t *testing.T) {
	t.Parallel()

	const size = 4
	c := NewNegativeCache(WithNegativeCacheSize(size))

	keys := make([]string, 0, size+1)
	for i := 0; i < size+1; i++ {
		k := "key-" + strconv.Itoa(i)
		keys = append(keys, k)
		c.RecordMiss(k)
	}

	if got := c.Len(); got != size {
		t.Errorf("Len() = %d, want %d (size cap)", got, size)
	}
	// The first key inserted is the oldest; it must have been
	// evicted to make room for the (size+1)th entry.
	if c.IsKnownMissing(keys[0]) {
		t.Errorf("IsKnownMissing(%q) = true, want false (evicted as oldest)", keys[0])
	}
	// Every subsequent key must still be present.
	for _, k := range keys[1:] {
		if !c.IsKnownMissing(k) {
			t.Errorf("IsKnownMissing(%q) = false, want true (within size cap)", k)
		}
	}
}

func TestNegativeCache_Defaults(t *testing.T) {
	t.Parallel()

	c := NewNegativeCache()
	if c.lru == nil {
		t.Fatal("NewNegativeCache() lru = nil, want non-nil")
	}
	// The expirable LRU reports its cap via cap-derived behaviour;
	// the simplest invariant we can assert without reaching into
	// hashicorp internals is that the documented default is the one
	// the option's fallback uses too.
	if got, want := DefaultNegativeCacheSize, 1024; got != want {
		t.Errorf("DefaultNegativeCacheSize = %d, want %d", got, want)
	}
	if got, want := DefaultNegativeCacheTTL, 60*time.Second; got != want {
		t.Errorf("DefaultNegativeCacheTTL = %v, want %v", got, want)
	}
}

// TestNegativeCache_NonPositiveOptionsIgnored locks in the contract
// that misconfiguration (a 0 or negative size/TTL) falls back to the
// defaults rather than producing an unusable cache.
func TestNegativeCache_NonPositiveOptionsIgnored(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []NegativeCacheOption
	}{
		{name: "zero size", opts: []NegativeCacheOption{WithNegativeCacheSize(0)}},
		{name: "negative size", opts: []NegativeCacheOption{WithNegativeCacheSize(-5)}},
		{name: "zero ttl", opts: []NegativeCacheOption{WithNegativeCacheTTL(0)}},
		{name: "negative ttl", opts: []NegativeCacheOption{WithNegativeCacheTTL(-time.Second)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := NewNegativeCache(tc.opts...)
			c.RecordMiss("key")
			if !c.IsKnownMissing("key") {
				t.Errorf("IsKnownMissing(key) = false after RecordMiss on default-fallback cache, want true")
			}
		})
	}
}

func TestNegativeCache_Len(t *testing.T) {
	t.Parallel()

	c := NewNegativeCache()
	if got := c.Len(); got != 0 {
		t.Errorf("Len() on empty cache = %d, want 0", got)
	}
	c.RecordMiss("a")
	c.RecordMiss("b")
	c.RecordMiss("a") // refresh, not insert — Len stays at 2.
	if got := c.Len(); got != 2 {
		t.Errorf("Len() after 2 distinct + 1 refresh = %d, want 2", got)
	}
}

func TestNegativeCache_Purge(t *testing.T) {
	t.Parallel()

	c := NewNegativeCache()
	c.RecordMiss("a")
	c.RecordMiss("b")
	c.Purge()

	if c.IsKnownMissing("a") || c.IsKnownMissing("b") {
		t.Errorf("IsKnownMissing(...) = true after Purge, want false for all keys")
	}
	if got := c.Len(); got != 0 {
		t.Errorf("Len() after Purge = %d, want 0", got)
	}
}

// TestNegativeCache_ConcurrentAccess exercises RecordMiss/IsKnownMissing
// from multiple goroutines so `go test -race` can flag any unsynchronised
// access in the wrapper. The underlying expirable LRU is safe; this
// guards against future drift in the wrapper itself.
func TestNegativeCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	c := NewNegativeCache()
	const goroutines = 8
	const perGoroutine = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				k := fmt.Sprintf("g%d-k%d", g, i%32)
				c.RecordMiss(k)
				_ = c.IsKnownMissing(k)
			}
		}()
	}
	wg.Wait()
}
