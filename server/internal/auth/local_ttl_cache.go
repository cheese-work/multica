package auth

import (
	"sync"
	"sync/atomic"
	"time"
)

// localCacheMaxEntries bounds a localTTLCache. One entry is a 64-char hash plus
// a small value (~200 B), so the cap holds the map near 2 MiB. At the cap a new
// key is not cached (the request just goes to Postgres); an entry is never
// evicted early, so a hit is always a value that passed its own TTL.
const localCacheMaxEntries = 10_000

// localTTLCache is the in-process store behind PATCache and DaemonTokenCache
// when no Redis client is configured (CHE-7639). Same contract as the Redis
// path: Get, Set with a TTL, Invalidate. It is per process, so a revoke in one
// process does not clear another process's entry; that entry ends at its TTL
// (at most AuthCacheTTL), the same bound as a missed Redis invalidation.
type localTTLCache[V any] struct {
	mu      sync.Mutex
	entries map[string]localTTLEntry[V]
	now     func() time.Time // test seam
}

type localTTLEntry[V any] struct {
	value   V
	expires time.Time
}

func newLocalTTLCache[V any]() *localTTLCache[V] {
	return &localTTLCache[V]{entries: make(map[string]localTTLEntry[V]), now: time.Now}
}

func (c *localTTLCache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	if !c.now().Before(e.expires) {
		delete(c.entries, key)
		var zero V
		return zero, false
	}
	return e.value, true
}

func (c *localTTLCache[V]) set(key string, v V, ttl time.Duration) {
	c.setIf(key, v, ttl, nil)
}

// setIf stores the entry only if ok (when non-nil) still holds. ok runs under
// the cache lock, so it cannot interleave with invalidate's delete.
func (c *localTTLCache[V]) setIf(key string, v V, ttl time.Duration, ok func() bool) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok != nil && !ok() {
		return
	}
	now := c.now()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= localCacheMaxEntries {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= localCacheMaxEntries {
			return
		}
	}
	c.entries[key] = localTTLEntry[V]{value: v, expires: now.Add(ttl)}
}

func (c *localTTLCache[V]) invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// invalidationFence lets a cache fill detect a same-process invalidate that ran
// after the fill read its row. A request reads the token from Postgres, a revoke
// deletes the cache entry, then the request fills the cache with the identity it
// read: without a fence that stale entry would outlive the revoke for up to
// AuthCacheTTL. The counter is process-wide, not per key: a revoke is rare, and
// a skipped fill only costs one more Postgres lookup.
type invalidationFence struct{ gen atomic.Uint64 }

// begin returns the generation to hold across the Postgres read.
func (f *invalidationFence) begin() uint64 { return f.gen.Load() }

// bump marks an invalidate. Call it BEFORE deleting the entry.
func (f *invalidationFence) bump() { f.gen.Add(1) }

// unchanged reports that no invalidate ran since begin returned gen.
func (f *invalidationFence) unchanged(gen uint64) bool { return f.gen.Load() == gen }
