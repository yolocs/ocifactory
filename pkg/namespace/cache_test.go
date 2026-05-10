package namespace

import (
	"testing"
	"time"

	"github.com/yolocs/ocifactory/pkg/auth"
)

func TestPolicyCache_RoundTrip(t *testing.T) {
	t.Parallel()

	c := newPolicyCache(8, time.Minute)
	az := auth.AllowAll
	c.put("alpha", cachedPolicy{authorizer: az})

	got, ok := c.get("alpha")
	if !ok {
		t.Fatalf("get(alpha) miss, want hit")
	}
	if got.authorizer != az {
		t.Errorf("get(alpha) authorizer = %v, want %v", got.authorizer, az)
	}
	if got.notFound {
		t.Errorf("get(alpha) notFound = true, want false")
	}
}

func TestPolicyCache_NegativeEntry(t *testing.T) {
	t.Parallel()

	c := newPolicyCache(8, time.Minute)
	c.put("missing", cachedPolicy{notFound: true})

	got, ok := c.get("missing")
	if !ok {
		t.Fatalf("get(missing) miss, want hit")
	}
	if !got.notFound {
		t.Errorf("get(missing) notFound = false, want true")
	}
}

func TestPolicyCache_Invalidate(t *testing.T) {
	t.Parallel()

	c := newPolicyCache(8, time.Minute)
	c.put("alpha", cachedPolicy{authorizer: auth.AllowAll})
	c.invalidate("alpha")

	if _, ok := c.get("alpha"); ok {
		t.Errorf("get(alpha) hit after invalidate, want miss")
	}
}

// TestPolicyCache_DisabledByZeroTTL pins the documented contract that
// ttl=0 means "no caching at all". Operators flip this on in tests
// where a fresh Put must take effect on the very next request without
// sleeping out the TTL.
func TestPolicyCache_DisabledByZeroTTL(t *testing.T) {
	t.Parallel()

	c := newPolicyCache(8, 0)
	c.put("alpha", cachedPolicy{authorizer: auth.AllowAll})
	if _, ok := c.get("alpha"); ok {
		t.Errorf("get(alpha) hit on disabled cache, want miss")
	}
}

func TestPolicyCache_Purge(t *testing.T) {
	t.Parallel()

	c := newPolicyCache(8, time.Minute)
	c.put("alpha", cachedPolicy{authorizer: auth.AllowAll})
	c.put("beta", cachedPolicy{authorizer: auth.AllowAll})
	c.purge()

	if _, ok := c.get("alpha"); ok {
		t.Errorf("get(alpha) hit after purge, want miss")
	}
	if _, ok := c.get("beta"); ok {
		t.Errorf("get(beta) hit after purge, want miss")
	}
}
