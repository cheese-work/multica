package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func scopedPATFixture(t *testing.T, scopes []string) (*db.Queries, string) {
	t.Helper()
	pool := openPool(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Scoped PAT', $1) RETURNING id`, "scoped-"+uuid.NewString()+"@test.invalid").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM personal_access_token WHERE user_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, userID)
	})
	raw, err := auth.GeneratePATToken()
	if err != nil {
		t.Fatal(err)
	}
	queries := db.New(pool)
	userUUID := parseTestUUID(t, userID)
	if _, err := queries.CreatePersonalAccessToken(ctx, db.CreatePersonalAccessTokenParams{UserID: userUUID, Name: "scoped", TokenHash: auth.HashToken(raw), TokenPrefix: raw[:12], Scopes: scopes}); err != nil {
		t.Fatal(err)
	}
	return queries, raw
}

// A decisions:feed PAT reaches the answer feed and its ack and nothing else, so
// a leaked sweep token cannot disclaim answers or mint unrestricted PATs.
func TestScopedPATReachesOnlyItsRoutes(t *testing.T) {
	queries, raw := scopedPATFixture(t, []string{"decisions:feed"})
	reached := func(method, path string, mw func(http.Handler) http.Handler) int {
		handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}
	authMW := Auth(queries, auth.NewPATCache(nil), nil, nil)
	answer := uuid.NewString()
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/decisions/feed", http.StatusOK},
		{http.MethodPost, "/api/decisions/feed/" + answer + "/ack", http.StatusOK},
		{http.MethodPost, "/api/tokens", http.StatusForbidden},
		{http.MethodGet, "/api/tokens", http.StatusForbidden},
		{http.MethodGet, "/api/me", http.StatusForbidden},
		{http.MethodPost, "/api/decisions/" + answer + "/disclaim", http.StatusForbidden},
		{http.MethodPost, "/api/decisions/feed", http.StatusForbidden},
		{http.MethodGet, "/api/decisions/feed/" + answer + "/ack", http.StatusForbidden},
		{http.MethodPost, "/api/decisions/feed/" + answer + "/ack/x", http.StatusForbidden},
	} {
		if got := reached(tc.method, tc.path, authMW); got != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, got, tc.want)
		}
	}
	if got := reached(http.MethodGet, "/api/daemon/heartbeat", DaemonAuth(queries, auth.NewPATCache(nil), nil, nil)); got != http.StatusForbidden {
		t.Errorf("daemon route = %d, want 403", got)
	}
}

func TestUnscopedPATStillReachesEveryRoute(t *testing.T) {
	queries, raw := scopedPATFixture(t, nil)
	handler := Auth(queries, auth.NewPATCache(nil), nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unscoped PAT = %d, want 200", w.Code)
	}
}

func parseTestUUID(t *testing.T, s string) (id pgtype.UUID) {
	t.Helper()
	if err := id.Scan(s); err != nil {
		t.Fatal(err)
	}
	return id
}
