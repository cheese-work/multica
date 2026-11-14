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

// A decisions:ci_request PAT reaches POST /api/decisions/ci and nothing else; a
// decisions:feed PAT does not reach it.
func TestCIRequestScopedPATReachesOnlyItsRoute(t *testing.T) {
	reached := func(scopes []string, method, path string) int {
		queries, raw := scopedPATFixture(t, scopes)
		handler := Auth(queries, auth.NewPATCache(nil), nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}
	ci, feed := []string{"decisions:ci_request"}, []string{"decisions:feed"}
	for _, tc := range []struct {
		scopes       []string
		method, path string
		want         int
	}{
		{ci, http.MethodPost, "/api/decisions/ci", http.StatusOK},
		{ci, http.MethodGet, "/api/decisions/ci", http.StatusForbidden},
		{ci, http.MethodPost, "/api/decisions/ci/x", http.StatusForbidden},
		{ci, http.MethodPost, "/api/decisions", http.StatusForbidden},
		{ci, http.MethodPost, "/api/decisions/ask", http.StatusForbidden},
		{ci, http.MethodGet, "/api/decisions/feed", http.StatusForbidden},
		{ci, http.MethodGet, "/api/me", http.StatusForbidden},
		{feed, http.MethodPost, "/api/decisions/ci", http.StatusForbidden},
	} {
		if got := reached(tc.scopes, tc.method, tc.path); got != tc.want {
			t.Errorf("%v %s %s = %d, want %d", tc.scopes, tc.method, tc.path, got, tc.want)
		}
	}
}

// A decisions:read PAT reaches GET /api/decisions/{id}/state and nothing else;
// the other scopes do not reach it.
func TestReadScopedPATReachesOnlyTheStateRoute(t *testing.T) {
	reached := func(scopes []string, method, path string) int {
		queries, raw := scopedPATFixture(t, scopes)
		handler := Auth(queries, auth.NewPATCache(nil), nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}
	read, feed, ci := []string{"decisions:read"}, []string{"decisions:feed"}, []string{"decisions:ci_request"}
	const id = "/api/decisions/0b4f7c1e-1111-4222-8333-444455556666"
	for _, tc := range []struct {
		scopes       []string
		method, path string
		want         int
	}{
		{read, http.MethodGet, id + "/state", http.StatusOK},
		{read, http.MethodPost, id + "/state", http.StatusForbidden},
		{read, http.MethodGet, id, http.StatusForbidden},
		{read, http.MethodGet, id + "/artifacts", http.StatusForbidden},
		{read, http.MethodGet, "/api/decisions/feed", http.StatusForbidden},
		{read, http.MethodGet, "/api/decisions//state", http.StatusForbidden},
		{read, http.MethodGet, id + "/x/state", http.StatusForbidden},
		{read, http.MethodPost, id + "/withdraw", http.StatusForbidden},
		{read, http.MethodGet, "/api/me", http.StatusForbidden},
		{feed, http.MethodGet, id + "/state", http.StatusForbidden},
		{ci, http.MethodGet, id + "/state", http.StatusForbidden},
	} {
		if got := reached(tc.scopes, tc.method, tc.path); got != tc.want {
			t.Errorf("%v %s %s = %d, want %d", tc.scopes, tc.method, tc.path, got, tc.want)
		}
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
