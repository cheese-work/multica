package middleware

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// sessionGenerationCacheTTL bounds how long ordinary routes trust a stored
// session generation, and so how long a revoked JWT can keep working there.
const sessionGenerationCacheTTL = 30 * time.Second

type cachedGeneration struct {
	gen int32
	at  time.Time
}

// ponytail: process-local and never evicted (one small entry per user who
// authenticated here); move to Redis if auth ever runs on several replicas
// and a 30 s per-replica bound is not enough.
var sessionGenerationCache sync.Map // user id string -> cachedGeneration

type freshGenerationCtxKey struct{}

// FreshSessionGeneration makes Auth read the stored generation for every
// request on the wrapped routes, never the cache. Mount it ahead of Auth on
// gated answers, action approvals, and passkey and gate routes.
func FreshSessionGeneration(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), freshGenerationCtxKey{}, true)))
	})
}

// errSessionUserUnknown means the user id is missing or malformed.
var errSessionUserUnknown = errors.New("session user unknown")

// storedSessionGeneration returns the user's current session generation. Any
// error other than errSessionUserUnknown is a storage failure the caller must
// treat as fail-closed.
func storedSessionGeneration(ctx context.Context, queries *db.Queries, userID string, useCache bool) (int32, error) {
	if useCache {
		if v, ok := sessionGenerationCache.Load(userID); ok {
			if c := v.(cachedGeneration); time.Since(c.at) < sessionGenerationCacheTTL {
				return c.gen, nil
			}
		}
	}
	uid, err := util.ParseUUID(userID)
	if err != nil {
		return 0, errSessionUserUnknown
	}
	gen, err := queries.GetUserSessionGeneration(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errSessionUserUnknown
	}
	if err != nil {
		return 0, err
	}
	sessionGenerationCache.Store(userID, cachedGeneration{gen: gen, at: time.Now()})
	return gen, nil
}
