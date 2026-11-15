package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

// SessionGenerationHeader carries a JWT's `gen` claim from the auth middleware
// to a handler. The middleware strips any client value and sets it for a JWT only.
const SessionGenerationHeader = "X-Session-Generation"

// SessionGenerationCacheTTL bounds how long ordinary routes trust a stored
// session generation, and so how long a revoked JWT can keep working there.
const SessionGenerationCacheTTL = 30 * time.Second

// SessionGenerationReader is satisfied by *db.Queries.
type SessionGenerationReader interface {
	GetUserSessionGeneration(ctx context.Context, id pgtype.UUID) (int32, error)
}

type cachedGeneration struct {
	gen int32
	at  time.Time
}

// ponytail: process-local and never evicted (one small entry per user who
// authenticated here); move to Redis if auth ever runs on several replicas
// and a 30 s per-replica bound is not enough.
var sessionGenerationCache sync.Map // user id string -> cachedGeneration

// errSessionUserUnknown means the user id is missing or malformed.
var errSessionUserUnknown = errors.New("session user unknown")

// CheckSessionGeneration compares the token's generation with the stored one.
// It returns nil for a current session, ErrStaleSession for a revoked one or
// an unknown user, and any other error for a storage failure the caller must
// treat as fail-closed (503). useCache=false reads storage every time.
func CheckSessionGeneration(ctx context.Context, reader SessionGenerationReader, claims jwt.MapClaims, useCache bool) error {
	sub, _ := claims["sub"].(string)
	current, err := storedSessionGeneration(ctx, reader, sub, useCache)
	switch {
	case errors.Is(err, errSessionUserUnknown):
		return ErrStaleSession
	case err != nil:
		return err
	case current != SessionGeneration(claims):
		return ErrStaleSession
	}
	return nil
}

func storedSessionGeneration(ctx context.Context, reader SessionGenerationReader, userID string, useCache bool) (int32, error) {
	if useCache {
		if v, ok := sessionGenerationCache.Load(userID); ok {
			if c, ok := v.(cachedGeneration); ok && time.Since(c.at) < SessionGenerationCacheTTL {
				return c.gen, nil
			}
		}
	}
	uid, err := util.ParseUUID(userID)
	if err != nil {
		return 0, errSessionUserUnknown
	}
	gen, err := reader.GetUserSessionGeneration(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errSessionUserUnknown
	}
	if err != nil {
		return 0, err
	}
	sessionGenerationCache.Store(userID, cachedGeneration{gen: gen, at: time.Now()})
	return gen, nil
}
