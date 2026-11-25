package middleware

// CHE-7639 PR 1: with no Redis, the token caches are bounded in-process caches.
// These tests run the real Auth/DaemonAuth middleware against Postgres and count
// the statements the pool actually starts, so a cache that is wired but not hit
// still fails.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/auth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type stmtCounter struct {
	mu     sync.Mutex
	counts map[string]int
	// afterQuery runs once, right after the named query finishes and before the
	// caller sees its rows. It lets a test interleave a revoke between the
	// middleware's DB read and its cache fill.
	afterQuery map[string]func()
}

type stmtNameKey struct{}

func (c *stmtCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, name := range []string{"GetPersonalAccessTokenByHash", "UpdatePersonalAccessTokenLastUsed", "GetDaemonTokenByHash"} {
		if strings.Contains(d.SQL, "name: "+name) {
			c.counts[name]++
			ctx = context.WithValue(ctx, stmtNameKey{}, name)
		}
	}
	return ctx
}
func (c *stmtCounter) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	name, _ := ctx.Value(stmtNameKey{}).(string)
	if name == "" {
		return
	}
	c.mu.Lock()
	fn := c.afterQuery[name]
	delete(c.afterQuery, name)
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}
func (c *stmtCounter) get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[name]
}

type tokenCacheEnv struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	count   *stmtCounter
	userID  string
	wsID    string
}

func newTokenCacheEnv(t *testing.T) *tokenCacheEnv {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	counter := &stmtCounter{counts: map[string]int{}}
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skipping: database not reachable: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	env := &tokenCacheEnv{pool: pool, queries: db.New(pool), count: counter}
	sfx := uuid.NewString()
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Token cache', $1) RETURNING id`, "tc-"+sfx+"@test.invalid").Scan(&env.userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ('Token cache', $1, '', 'TKC') RETURNING id`, "tc-"+sfx).Scan(&env.wsID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM personal_access_token WHERE user_id = $1`, env.userID)
		_, _ = pool.Exec(ctx, `DELETE FROM workspace WHERE id = $1`, env.wsID)
		_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, env.userID)
	})
	return env
}

func (e *tokenCacheEnv) newPAT(t *testing.T, scopes []string, expires time.Time) (raw string, id string) {
	t.Helper()
	raw, err := auth.GeneratePATToken()
	if err != nil {
		t.Fatal(err)
	}
	if scopes == nil {
		scopes = []string{}
	}
	var exp any
	if !expires.IsZero() {
		exp = expires
	}
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO personal_access_token (user_id, name, token_hash, token_prefix, scopes, expires_at) VALUES ($1,'tc',$2,$3,$4,$5) RETURNING id`,
		e.userID, auth.HashToken(raw), raw[:12], scopes, exp).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return raw, id
}

func (e *tokenCacheEnv) newDaemonToken(t *testing.T) string {
	t.Helper()
	raw := "mdt_" + uuid.NewString()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO daemon_token (token_hash, workspace_id, daemon_id, expires_at, owner_id) VALUES ($1,$2,'tc-daemon', now() + interval '1 hour', $3)`,
		auth.HashToken(raw), e.wsID, e.userID); err != nil {
		t.Fatal(err)
	}
	return raw
}

func tcServe(h http.Handler, token string) int {
	req := httptest.NewRequest("GET", "/api/x", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

var tcOKHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

// waitForCount waits for the async last_used goroutines to start, then a grace
// period so an extra (wrong) write would also be counted.
func waitForCount(t *testing.T, c *stmtCounter, name string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.get(name) < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
}

func TestNoRedis_PATCollapsesToOneLookupAndOneWrite(t *testing.T) {
	for name, build := range map[string]func(e *tokenCacheEnv, c *auth.PATCache) http.Handler{
		"Auth": func(e *tokenCacheEnv, c *auth.PATCache) http.Handler {
			return Auth(e.queries, c, nil, nil)(tcOKHandler)
		},
		"DaemonAuth": func(e *tokenCacheEnv, c *auth.PATCache) http.Handler {
			return DaemonAuth(e.queries, c, nil, nil)(tcOKHandler)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTokenCacheEnv(t)
			raw, _ := e.newPAT(t, nil, time.Time{})
			h := build(e, auth.NewPATCache(nil))
			for i := 0; i < 100; i++ {
				if code := tcServe(h, raw); code != http.StatusOK {
					t.Fatalf("request %d: got %d", i, code)
				}
			}
			waitForCount(t, e.count, "UpdatePersonalAccessTokenLastUsed", 1)
			if got := e.count.get("GetPersonalAccessTokenByHash"); got != 1 {
				t.Errorf("GetPersonalAccessTokenByHash starts = %d, want 1", got)
			}
			if got := e.count.get("UpdatePersonalAccessTokenLastUsed"); got != 1 {
				t.Errorf("UpdatePersonalAccessTokenLastUsed starts = %d, want 1", got)
			}
		})
	}
}

func TestNoRedis_DaemonTokenCollapsesToOneLookup(t *testing.T) {
	e := newTokenCacheEnv(t)
	raw := e.newDaemonToken(t)
	h := DaemonAuth(e.queries, auth.NewPATCache(nil), auth.NewDaemonTokenCache(nil), nil)(tcOKHandler)
	for i := 0; i < 100; i++ {
		if code := tcServe(h, raw); code != http.StatusOK {
			t.Fatalf("request %d: got %d", i, code)
		}
	}
	if got := e.count.get("GetDaemonTokenByHash"); got != 1 {
		t.Errorf("GetDaemonTokenByHash starts = %d, want 1", got)
	}
}

func TestNoRedis_RevokedPATIsRefusedAfterInvalidate(t *testing.T) {
	e := newTokenCacheEnv(t)
	raw, id := e.newPAT(t, nil, time.Time{})
	cache := auth.NewPATCache(nil)
	h := Auth(e.queries, cache, nil, nil)(tcOKHandler)
	if code := tcServe(h, raw); code != http.StatusOK {
		t.Fatalf("before revoke: %d", code)
	}
	// Same two steps as handler.RevokePersonalAccessToken.
	if _, err := e.queries.RevokePersonalAccessToken(context.Background(), db.RevokePersonalAccessTokenParams{ID: parseTestUUID(t, id), UserID: parseTestUUID(t, e.userID)}); err != nil {
		t.Fatal(err)
	}
	cache.Invalidate(context.Background(), auth.HashToken(raw))
	if code := tcServe(h, raw); code != http.StatusUnauthorized {
		t.Fatalf("after revoke: got %d, want 401", code)
	}
}

func TestNoRedis_DeletedDaemonTokenIsRefusedAfterInvalidate(t *testing.T) {
	e := newTokenCacheEnv(t)
	raw := e.newDaemonToken(t)
	cache := auth.NewDaemonTokenCache(nil)
	h := DaemonAuth(e.queries, nil, cache, nil)(tcOKHandler)
	if code := tcServe(h, raw); code != http.StatusOK {
		t.Fatalf("before delete: %d", code)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM daemon_token WHERE token_hash = $1`, auth.HashToken(raw)); err != nil {
		t.Fatal(err)
	}
	cache.Invalidate(context.Background(), auth.HashToken(raw))
	if code := tcServe(h, raw); code != http.StatusUnauthorized {
		t.Fatalf("after delete: got %d, want 401", code)
	}
}

func TestNoRedis_PATIsNotAcceptedAfterExpiresAt(t *testing.T) {
	e := newTokenCacheEnv(t)
	raw, _ := e.newPAT(t, nil, time.Now().Add(1500*time.Millisecond))
	h := Auth(e.queries, auth.NewPATCache(nil), nil, nil)(tcOKHandler)
	if code := tcServe(h, raw); code != http.StatusOK {
		t.Fatalf("before expiry: %d", code)
	}
	time.Sleep(1700 * time.Millisecond)
	if code := tcServe(h, raw); code != http.StatusUnauthorized {
		t.Fatalf("after expiry: got %d, want 401", code)
	}
}

func TestNoRedis_ScopedPATIsLookedUpOnEveryRequest(t *testing.T) {
	e := newTokenCacheEnv(t)
	raw, _ := e.newPAT(t, []string{"decisions:feed"}, time.Time{})
	h := Auth(e.queries, auth.NewPATCache(nil), nil, nil)(tcOKHandler)
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/api/decisions/feed", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	if got := e.count.get("GetPersonalAccessTokenByHash"); got != 5 {
		t.Errorf("scoped PAT lookups = %d, want 5 (never cached)", got)
	}
}

// A request that read the token as valid, then lost a race with the revoke,
// must not write the stale identity back into the cache: later requests would
// accept a revoked token for up to AuthCacheTTL.
func TestNoRedis_RevokeDuringInFlightLookupIsNotUndoneByCacheFill(t *testing.T) {
	ctx := context.Background()
	t.Run("PAT/Auth", func(t *testing.T) {
		e := newTokenCacheEnv(t)
		raw, id := e.newPAT(t, nil, time.Time{})
		cache := auth.NewPATCache(nil)
		h := Auth(e.queries, cache, nil, nil)(tcOKHandler)
		e.count.afterQuery = map[string]func(){"GetPersonalAccessTokenByHash": func() {
			// Same two steps as handler.RevokePersonalAccessToken, run on a
			// separate pool connection while the lookup above is in flight.
			if _, err := e.queries.RevokePersonalAccessToken(ctx, db.RevokePersonalAccessTokenParams{ID: parseTestUUID(t, id), UserID: parseTestUUID(t, e.userID)}); err != nil {
				t.Error(err)
			}
			cache.Invalidate(ctx, auth.HashToken(raw))
		}}
		_ = tcServe(h, raw) // in-flight request: authenticated before the revoke, either outcome is fine
		if code := tcServe(h, raw); code != http.StatusUnauthorized {
			t.Fatalf("request after revoke: got %d, want 401 (stale entry was cached)", code)
		}
	})
	t.Run("PAT/DaemonAuth", func(t *testing.T) {
		e := newTokenCacheEnv(t)
		raw, id := e.newPAT(t, nil, time.Time{})
		cache := auth.NewPATCache(nil)
		h := DaemonAuth(e.queries, cache, nil, nil)(tcOKHandler)
		e.count.afterQuery = map[string]func(){"GetPersonalAccessTokenByHash": func() {
			if _, err := e.queries.RevokePersonalAccessToken(ctx, db.RevokePersonalAccessTokenParams{ID: parseTestUUID(t, id), UserID: parseTestUUID(t, e.userID)}); err != nil {
				t.Error(err)
			}
			cache.Invalidate(ctx, auth.HashToken(raw))
		}}
		_ = tcServe(h, raw)
		if code := tcServe(h, raw); code != http.StatusUnauthorized {
			t.Fatalf("request after revoke: got %d, want 401 (stale entry was cached)", code)
		}
	})
	t.Run("DaemonToken/DaemonAuth", func(t *testing.T) {
		e := newTokenCacheEnv(t)
		raw := e.newDaemonToken(t)
		cache := auth.NewDaemonTokenCache(nil)
		h := DaemonAuth(e.queries, nil, cache, nil)(tcOKHandler)
		e.count.afterQuery = map[string]func(){"GetDaemonTokenByHash": func() {
			if _, err := e.pool.Exec(ctx, `DELETE FROM daemon_token WHERE token_hash = $1`, auth.HashToken(raw)); err != nil {
				t.Error(err)
			}
			cache.Invalidate(ctx, auth.HashToken(raw))
		}}
		_ = tcServe(h, raw)
		if code := tcServe(h, raw); code != http.StatusUnauthorized {
			t.Fatalf("request after delete: got %d, want 401 (stale entry was cached)", code)
		}
	})
}
