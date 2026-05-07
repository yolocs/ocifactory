package python

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestSimpleIndexCache exercises the wrapper's contract: get, put, and
// invalidate behave correctly across enabled (ttl > 0) and disabled
// (ttl <= 0) configurations. TTL-driven eviction is owned by the
// underlying hashicorp/golang-lru/v2/expirable LRU and tested upstream;
// here we just verify the wiring with a short real-clock TTL.
func TestSimpleIndexCache(t *testing.T) {
	t.Parallel()

	files := func(names ...string) []cachedFile {
		out := make([]cachedFile, len(names))
		for i, n := range names {
			out[i] = cachedFile{Filename: n, OwningTag: "1.0.0"}
		}
		return out
	}

	type cacheOp struct {
		op    string // "put", "get", "invalidate", "sleep"
		pkg   string
		files []cachedFile
		sleep time.Duration
		// Only meaningful for "get" steps.
		wantOK    bool
		wantFiles []cachedFile
	}

	cases := []struct {
		name string
		ttl  time.Duration
		ops  []cacheOp
	}{
		{
			name: "miss then hit",
			ttl:  time.Minute,
			ops: []cacheOp{
				{op: "get", pkg: "requests", wantOK: false},
				{op: "put", pkg: "requests", files: files("requests-1.0.0.whl")},
				{op: "get", pkg: "requests", wantOK: true, wantFiles: files("requests-1.0.0.whl")},
			},
		},
		{
			name: "entry expires after TTL",
			ttl:  50 * time.Millisecond,
			ops: []cacheOp{
				{op: "put", pkg: "requests", files: files("requests-1.0.0.whl")},
				{op: "get", pkg: "requests", wantOK: true, wantFiles: files("requests-1.0.0.whl")},
				{op: "sleep", sleep: 120 * time.Millisecond},
				{op: "get", pkg: "requests", wantOK: false},
			},
		},
		{
			name: "invalidate clears one package, leaves others alone",
			ttl:  time.Minute,
			ops: []cacheOp{
				{op: "put", pkg: "requests", files: files("requests-1.0.0.whl")},
				{op: "put", pkg: "flask", files: files("flask-2.3.0.tar.gz")},
				{op: "invalidate", pkg: "requests"},
				{op: "get", pkg: "requests", wantOK: false},
				{op: "get", pkg: "flask", wantOK: true, wantFiles: files("flask-2.3.0.tar.gz")},
			},
		},
		{
			name: "ttl<=0 disables get and put",
			ttl:  0,
			ops: []cacheOp{
				{op: "put", pkg: "requests", files: files("requests-1.0.0.whl")},
				{op: "get", pkg: "requests", wantOK: false},
				// Invalidate must remain a safe no-op when caching is disabled.
				{op: "invalidate", pkg: "requests"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := newSimpleIndexCache(tc.ttl)

			for i, step := range tc.ops {
				switch step.op {
				case "put":
					c.put(step.pkg, step.files)
				case "get":
					gotFiles, gotOK := c.get(step.pkg)
					if gotOK != step.wantOK {
						t.Errorf("step %d (%q get %q): ok=%t, want %t", i, tc.name, step.pkg, gotOK, step.wantOK)
					}
					if diff := cmp.Diff(step.wantFiles, gotFiles); diff != "" {
						t.Errorf("step %d (%q get %q): files mismatch (-want +got):\n%s", i, tc.name, step.pkg, diff)
					}
				case "invalidate":
					c.invalidate(step.pkg)
				case "sleep":
					time.Sleep(step.sleep)
				default:
					t.Fatalf("unknown op %q", step.op)
				}
			}
		})
	}
}

func TestSimpleIndexCache_Concurrent(t *testing.T) {
	t.Parallel()

	c := newSimpleIndexCache(time.Minute)

	const writers, readers, ops = 4, 4, 200
	var wg sync.WaitGroup
	wg.Add(writers + readers)
	var hits, misses int64
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				pkg := "pkg" + string(rune('a'+(j%4)))
				switch j % 3 {
				case 0:
					c.put(pkg, []cachedFile{{Filename: "f", OwningTag: "1"}})
				case 1:
					c.invalidate(pkg)
				case 2:
					c.put(pkg, nil)
				}
			}
		}()
	}
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				pkg := "pkg" + string(rune('a'+(j%4)))
				if _, ok := c.get(pkg); ok {
					atomic.AddInt64(&hits, 1)
				} else {
					atomic.AddInt64(&misses, 1)
				}
			}
		}()
	}
	wg.Wait()

	if hits+misses != int64(readers*ops) {
		t.Errorf("hit+miss = %d, want %d", hits+misses, int64(readers*ops))
	}
}
