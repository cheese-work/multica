package auth

import (
	"strconv"
	"testing"
	"time"
)

func TestLocalTTLCache_EntryEndsAtTTL(t *testing.T) {
	c := newLocalTTLCache[string]()
	now := time.Unix(1_000, 0)
	c.now = func() time.Time { return now }
	c.set("k", "v", time.Minute)
	now = now.Add(time.Minute - time.Nanosecond)
	if _, ok := c.get("k"); !ok {
		t.Fatal("entry must hit just before its TTL")
	}
	now = now.Add(time.Nanosecond)
	if _, ok := c.get("k"); ok {
		t.Fatal("entry must miss at its TTL")
	}
	if len(c.entries) != 0 {
		t.Fatal("expired entry must be dropped on read")
	}
}

func TestLocalTTLCache_BoundedAndPrefersExpiredEntries(t *testing.T) {
	c := newLocalTTLCache[int]()
	now := time.Unix(1_000, 0)
	c.now = func() time.Time { return now }
	for i := 0; i < localCacheMaxEntries; i++ {
		c.set(strconv.Itoa(i), i, time.Minute)
	}
	// Full of live entries: a new key is not cached, a live key is not evicted.
	c.set("extra", -1, time.Minute)
	if _, ok := c.get("extra"); ok {
		t.Fatal("new key must not be cached when the cache is full of live entries")
	}
	if len(c.entries) != localCacheMaxEntries {
		t.Fatalf("size = %d, want %d", len(c.entries), localCacheMaxEntries)
	}
	if _, ok := c.get("0"); !ok {
		t.Fatal("a live entry must not be evicted")
	}
	// Once the old entries expire, a new key makes room.
	now = now.Add(2 * time.Minute)
	c.set("extra", -1, time.Minute)
	if v, ok := c.get("extra"); !ok || v != -1 {
		t.Fatal("new key must be cached after expired entries are swept")
	}
	if len(c.entries) != 1 {
		t.Fatalf("size = %d, want 1 after sweep", len(c.entries))
	}
	// Overwriting an existing key at the cap is always allowed.
	for i := 0; i < localCacheMaxEntries-1; i++ {
		c.set("n"+strconv.Itoa(i), i, time.Minute)
	}
	c.set("extra", 7, time.Minute)
	if v, _ := c.get("extra"); v != 7 {
		t.Fatalf("overwrite at cap = %d, want 7", v)
	}
}
