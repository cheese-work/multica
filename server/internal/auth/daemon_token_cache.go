package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// daemonTokenCachePrefix namespaces daemon-token cache keys separately
// from PAT (mul:auth:pat:*) so the two key spaces can't collide and an
// invalidation on one kind of token doesn't accidentally hit the other.
//
// v2 (MUL-8154) added OwnerID; entries written before it are never read, so a
// pre-upgrade entry cannot authorize a token without its owner binding.
const daemonTokenCachePrefix = "mul:auth:daemon:v2:"

// DaemonTokenIdentity is what DaemonAuth needs from the cached lookup —
// the workspace_id and daemon_id that the middleware injects into the
// request context. We deliberately omit token_hash, expires_at, and the
// row id; cache entries should leak the minimum.
type DaemonTokenIdentity struct {
	WorkspaceID string `json:"w"`
	DaemonID    string `json:"d"`
	// OwnerID is the runtime owner the token was issued for ("" for
	// Qoder-managed tokens). MUL-8154.
	OwnerID string `json:"o,omitempty"`
}

// DaemonTokenCache caches resolved daemon-token (mdt_) lookups in Redis, or
// in process when no Redis client is configured. A nil *DaemonTokenCache is
// safe to use — every method becomes a no-op or reports a cache miss.
type DaemonTokenCache struct {
	rdb   redis.UniversalClient
	local *localTTLCache[DaemonTokenIdentity] // set only when rdb is nil
}

// NewDaemonTokenCache returns a cache backed by rdb. With a nil rdb it
// returns a bounded in-process cache with the same contract (CHE-7639).
func NewDaemonTokenCache(rdb redis.UniversalClient) *DaemonTokenCache {
	if rdb == nil {
		return &DaemonTokenCache{local: newLocalTTLCache[DaemonTokenIdentity]()}
	}
	return &DaemonTokenCache{rdb: rdb}
}

func daemonTokenCacheKey(hash string) string { return daemonTokenCachePrefix + hash }

// Get returns the cached identity for a token hash. ok=false on cache
// miss or any Redis / decode error — a dead Redis must not take down
// auth.
func (c *DaemonTokenCache) Get(ctx context.Context, hash string) (DaemonTokenIdentity, bool) {
	if c == nil {
		return DaemonTokenIdentity{}, false
	}
	if c.local != nil {
		return c.local.get(hash)
	}
	raw, err := c.rdb.Get(ctx, daemonTokenCacheKey(hash)).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			slog.Warn("daemon_token_cache: get failed; falling back to DB", "error", err)
		}
		return DaemonTokenIdentity{}, false
	}
	var id DaemonTokenIdentity
	if err := json.Unmarshal(raw, &id); err != nil {
		slog.Warn("daemon_token_cache: malformed entry; falling back to DB", "error", err)
		return DaemonTokenIdentity{}, false
	}
	return id, true
}

// Set populates the cache with the given TTL. Use TTLForExpiry to clamp
// the TTL to the token's remaining lifetime so a daemon token expiring
// in <AuthCacheTTL can't outlive its expires_at on a cache hit.
//
// Errors are logged and swallowed — a cache write failure is not a
// request failure.
func (c *DaemonTokenCache) Set(ctx context.Context, hash string, id DaemonTokenIdentity, ttl time.Duration) {
	if c == nil || ttl <= 0 {
		return
	}
	if c.local != nil {
		c.local.set(hash, id, ttl)
		return
	}
	raw, err := json.Marshal(id)
	if err != nil {
		slog.Warn("daemon_token_cache: marshal failed", "error", err)
		return
	}
	if err := c.rdb.Set(ctx, daemonTokenCacheKey(hash), raw, ttl).Err(); err != nil {
		slog.Warn("daemon_token_cache: set failed", "error", err)
	}
}

// Invalidate removes the entry for hash. Called when a daemon token is
// deleted so the deletion takes effect immediately rather than waiting
// for the TTL.
func (c *DaemonTokenCache) Invalidate(ctx context.Context, hash string) {
	if c == nil {
		return
	}
	if c.local != nil {
		c.local.invalidate(hash)
		return
	}
	if err := c.rdb.Del(ctx, daemonTokenCacheKey(hash)).Err(); err != nil {
		slog.Warn("daemon_token_cache: invalidate failed; entry will expire on TTL", "error", err)
	}
}
