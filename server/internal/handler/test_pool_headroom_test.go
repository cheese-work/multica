package handler

import "testing"

// CHE-7000: pgxpool defaults to max(4, NumCPU) connections, so on a 4-vCPU CI
// runner the lock-wait tests (a held tx, two or three blocked writers and the
// poll query) starved the pool and sat until the 20m shard timeout. The pool
// must not depend on the runner's core count.
func TestSharedTestPoolHasHeadroomForLockWaitTests(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	if got := testPool.Config().MaxConns; got < 16 {
		t.Fatalf("shared test pool MaxConns = %d, want >= 16", got)
	}
}
