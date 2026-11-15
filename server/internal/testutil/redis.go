package testutil

import "os"

// RedisTestDB picks the Redis logical DB a test package flushes. urlDB is the
// index in REDIS_TEST_URL and fixed is the package's own constant.
//
// Under scripts/go-test-db-exec.sh (MULTICA_TEST_DB_PER_PACKAGE=1) each test
// process holds a unique index from the URL. A package-fixed index is shared
// by the parallel handler shards, and by packages that picked the same
// constant, so their FlushDB calls delete each other's live keys. Unwrapped
// runs keep the fixed index, which keeps `go test ./...` packages apart.
func RedisTestDB(urlDB, fixed int) int {
	if os.Getenv("MULTICA_TEST_DB_PER_PACKAGE") == "1" {
		return urlDB
	}
	return fixed
}
