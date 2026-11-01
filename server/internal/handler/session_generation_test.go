package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// generationUser inserts a throwaway user so a bump never touches the shared
// handler-test user, and returns its id and email.
func generationUser(t *testing.T) (id, email string) {
	t.Helper()
	email = fmt.Sprintf("gen-%d@multica.ai", time.Now().UnixNano())
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO "user" (name, email) VALUES ('Gen Test', $1) RETURNING id::text`, email).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, id) })
	return id, email
}

func bumpGeneration(t *testing.T, userID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE "user" SET session_generation = session_generation + 1 WHERE id = $1`, userID); err != nil {
		t.Fatalf("bump generation: %v", err)
	}
}

// generationToken signs a session for userID. gen < 0 omits the claim, which is
// how every pre-generation token looks.
func generationToken(t *testing.T, userID, email, sid string, gen int, remaining time.Duration) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":   userID,
		"email": email,
		"exp":   time.Now().Add(remaining).Unix(),
		"iat":   time.Now().Add(-time.Hour).Unix(),
	}
	if sid != "" {
		claims["sid"] = sid
	}
	if gen >= 0 {
		claims["gen"] = gen
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func serveThroughAuth(queries *db.Queries, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	middleware.Auth(queries, nil, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	return rec
}

func bearerGET(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/issues", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func cookieGET(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/issues", nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: token})
	return req
}

func TestSessionGeneration_MiddlewareRefusesStaleJWT(t *testing.T) {
	t.Run("token without gen reads as generation 0", func(t *testing.T) {
		id, email := generationUser(t)
		tok := generationToken(t, id, email, "sid-a", -1, time.Hour)
		if rec := serveThroughAuth(testHandler.Queries, bearerGET(tok)); rec.Code != http.StatusOK {
			t.Fatalf("legacy token at generation 0: got %d, want 200", rec.Code)
		}
	})
	for name, sid := range map[string]string{"with sid": "sid-b", "without sid": ""} {
		t.Run("bumped generation refuses old JWT "+name, func(t *testing.T) {
			id, email := generationUser(t)
			bumpGeneration(t, id)
			tok := generationToken(t, id, email, sid, -1, time.Hour)
			if rec := serveThroughAuth(testHandler.Queries, bearerGET(tok)); rec.Code != http.StatusUnauthorized {
				t.Fatalf("stale token: got %d, want 401", rec.Code)
			}
		})
	}
	t.Run("token carrying the current gen passes", func(t *testing.T) {
		id, email := generationUser(t)
		bumpGeneration(t, id)
		tok := generationToken(t, id, email, "sid-c", 1, time.Hour)
		if rec := serveThroughAuth(testHandler.Queries, bearerGET(tok)); rec.Code != http.StatusOK {
			t.Fatalf("current token: got %d, want 200", rec.Code)
		}
	})
	t.Run("storage read error fails closed with 503", func(t *testing.T) {
		id, email := generationUser(t)
		pool, err := pgxpool.New(context.Background(), testPool.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		pool.Close()
		tok := generationToken(t, id, email, "sid-d", 0, time.Hour)
		if rec := serveThroughAuth(db.New(pool), bearerGET(tok)); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("read error: got %d, want 503", rec.Code)
		}
	})
}

func TestSessionGeneration_MiddlewareRenewalCopiesOrRefuses(t *testing.T) {
	window := auth.AuthRenewThreshold() / 2

	t.Run("stale token in the renewal window is refused, not renewed", func(t *testing.T) {
		id, email := generationUser(t)
		bumpGeneration(t, id)
		rec := serveThroughAuth(testHandler.Queries, cookieGET(generationToken(t, id, email, "sid-e", -1, window)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", rec.Code)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatal("a refused session must not receive a renewed cookie")
		}
	})

	t.Run("current token is renewed and gen is copied", func(t *testing.T) {
		id, email := generationUser(t)
		bumpGeneration(t, id)
		rec := serveThroughAuth(testHandler.Queries, cookieGET(generationToken(t, id, email, "sid-f", 1, window)))
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d, want 200", rec.Code)
		}
		var renewed string
		for _, c := range rec.Result().Cookies() {
			if c.Name == auth.AuthCookieName {
				renewed = c.Value
			}
		}
		claims, err := auth.ParseSessionToken(renewed)
		if err != nil {
			t.Fatalf("renewed cookie missing or invalid: %v", err)
		}
		if gen, _ := claims["gen"].(float64); gen != 1 {
			t.Fatalf("renewed gen = %v, want 1", claims["gen"])
		}
	})
}

func TestSessionGeneration_RefreshEndpoint(t *testing.T) {
	window := auth.AuthRenewThreshold() / 2

	t.Run("stale token is refused", func(t *testing.T) {
		id, email := generationUser(t)
		bumpGeneration(t, id)
		req := bearerRefreshRequest(t, generationToken(t, id, email, "sid-g", -1, window))
		req.Header.Set("X-User-ID", id)
		testutil.Call(t, testHandler.RefreshSession, req).Want(http.StatusUnauthorized)
	})

	t.Run("current token is renewed with gen copied", func(t *testing.T) {
		id, email := generationUser(t)
		bumpGeneration(t, id)
		req := bearerRefreshRequest(t, generationToken(t, id, email, "sid-h", 1, window))
		req.Header.Set("X-User-ID", id)
		var resp RefreshSessionResponse
		testutil.Call(t, testHandler.RefreshSession, req).Want(http.StatusOK).JSON(&resp)
		if !resp.Renewed || resp.Token == "" {
			t.Fatalf("expected renewal, got %+v", resp)
		}
		claims, err := auth.ParseSessionToken(resp.Token)
		if err != nil {
			t.Fatal(err)
		}
		if gen, _ := claims["gen"].(float64); gen != 1 {
			t.Fatalf("renewed gen = %v, want 1", claims["gen"])
		}
	})
}

func TestSessionGeneration_LoginTokenCarriesGen(t *testing.T) {
	tok, err := testHandler.issueJWT(db.User{ID: parseUUID(testUserID), Email: handlerTestEmail, SessionGeneration: 2}, "interactive")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := auth.ParseSessionToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if gen, _ := claims["gen"].(float64); gen != 2 {
		t.Fatalf("gen claim = %v, want 2", claims["gen"])
	}
}

func TestSessionGeneration_FreshRoutesSkipTheCache(t *testing.T) {
	id, email := generationUser(t)
	tok := generationToken(t, id, email, "sid-i", 0, time.Hour)
	if rec := serveThroughAuth(testHandler.Queries, bearerGET(tok)); rec.Code != http.StatusOK {
		t.Fatalf("before bump: got %d, want 200", rec.Code)
	}
	bumpGeneration(t, id)

	// The generation just read is cached for ordinary routes (<= 30 s).
	if rec := serveThroughAuth(testHandler.Queries, bearerGET(tok)); rec.Code != http.StatusOK {
		t.Fatalf("ordinary route inside cache window: got %d, want 200", rec.Code)
	}
	rec := httptest.NewRecorder()
	middleware.FreshSessionGeneration(middleware.Auth(testHandler.Queries, nil, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))).ServeHTTP(rec, bearerGET(tok))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("fresh route after bump: got %d, want 401", rec.Code)
	}
}
