package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/multica-ai/multica/server/internal/auth"
)

// freshGenerationPrefixes are the routes that act on a human's behalf with a
// passkey or an approval, mint a non-expiring PAT or hand off a new session token. They read the stored
// generation on every request, not the 30 s cache, so a revocation lands at
// once. Decided by path because Auth runs before any per-route middleware.
var freshGenerationPrefixes = []string{
	"/api/passkeys/",
	"/api/tokens",
	"/api/cli-token",
	"/api/auth/desktop-token",
	"/api/decision-actions/",
	"/api/decision-answers/",
	"/api/decision-requests/",
}

func requiresFreshGeneration(path string) bool {
	for _, p := range freshGenerationPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// checkSessionGeneration refuses a revoked session with 401 and a storage
// failure with 503 (fail closed). It reports whether the request may proceed.
func checkSessionGeneration(w http.ResponseWriter, r *http.Request, claims jwt.MapClaims, queries auth.SessionGenerationReader, useCache bool) bool {
	err := auth.CheckSessionGeneration(r.Context(), queries, claims, useCache)
	switch {
	case err == nil:
		return true
	case errors.Is(err, auth.ErrStaleSession):
		slog.Warn("auth: revoked session rejected", "path", r.URL.Path, "user_id", claims["sub"])
		writeAuthenticationError(w, r, `{"error":"session revoked"}`)
	default:
		slog.Error("auth: session generation read failed", "path", r.URL.Path, "error", err)
		writeError(w, http.StatusServiceUnavailable, "session check unavailable")
	}
	return false
}
