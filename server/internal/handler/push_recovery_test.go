package handler

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/multica-ai/multica/server/internal/auth"
)

// CHE-3431: a session issued before passkey recovery must not register a push
// device after it, including inside the generation cache window that lets the
// middleware accept the token.

func pushRegisterRequest(t *testing.T, endpoint, sid string, generation int) *http.Request {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	body := map[string]any{"endpoint": endpoint, "keys": map[string]string{
		"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		"auth":   base64.RawURLEncoding.EncodeToString(secret),
	}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": testUserID, "sid": sid, "gen": generation,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := newRequest(http.MethodPost, "/api/push/subscriptions", body)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func bumpGenerationForTest(t *testing.T) {
	t.Helper()
	var stored int
	if err := testPool.QueryRow(context.Background(), `SELECT session_generation FROM "user" WHERE id = $1`, testUserID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE "user" SET session_generation = $2 WHERE id = $1`, testUserID, stored)
	})
}

func pushRowsFor(t *testing.T, endpoint string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM push_subscription WHERE endpoint = $1`, endpoint).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPushRegistrationFromARevokedGenerationIsRefused(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	pushHandlerEnv(t)
	endpoint := "https://push.example.com/send/revoked-generation"
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM push_subscription WHERE endpoint = $1`, endpoint)
	})
	bumpGenerationForTest(t)
	var stored int
	testPool.QueryRow(context.Background(), `SELECT session_generation FROM "user" WHERE id = $1`, testUserID).Scan(&stored)
	if _, err := testPool.Exec(context.Background(), `UPDATE "user" SET session_generation = session_generation + 1 WHERE id = $1`, testUserID); err != nil {
		t.Fatal(err)
	}

	// The token still carries the old generation: the middleware's cache may have accepted it.
	w := httptest.NewRecorder()
	testHandler.UpsertPushSubscription(w, pushRegisterRequest(t, endpoint, "sid-revoked-gen", stored))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session: status %d, want 401", w.Code)
	}
	if n := pushRowsFor(t, endpoint); n != 0 {
		t.Fatalf("a revoked session left %d push subscriptions", n)
	}

	w = httptest.NewRecorder()
	testHandler.UpsertPushSubscription(w, pushRegisterRequest(t, endpoint, "sid-current-gen", stored+1))
	if w.Code != http.StatusOK {
		t.Fatalf("current session: status %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// Recovery commits while the registration waits on the user row: the request
// that began before recovery must see the bumped generation, not register.
func TestPushRegistrationRacingRecoveryIsRefused(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	pushHandlerEnv(t)
	endpoint := "https://push.example.com/send/racing-recovery"
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM push_subscription WHERE endpoint = $1`, endpoint)
	})
	bumpGenerationForTest(t)
	var stored int
	if err := testPool.QueryRow(context.Background(), `SELECT session_generation FROM "user" WHERE id = $1`, testUserID).Scan(&stored); err != nil {
		t.Fatal(err)
	}

	recovery, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovery.Rollback(context.Background()) })
	if _, err := recovery.Exec(context.Background(), `SELECT id FROM "user" WHERE id = $1 FOR UPDATE`, testUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.Exec(context.Background(), `UPDATE "user" SET session_generation = session_generation + 1 WHERE id = $1`, testUserID); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		testHandler.UpsertPushSubscription(w, pushRegisterRequest(t, endpoint, "sid-racing", stored))
		done <- w.Code
	}()
	time.Sleep(300 * time.Millisecond)
	if err := recovery.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != http.StatusUnauthorized {
		t.Fatalf("registration racing recovery: status %d, want 401", code)
	}
	if n := pushRowsFor(t, endpoint); n != 0 {
		t.Fatalf("the racing registration left %d push subscriptions after recovery", n)
	}
}
