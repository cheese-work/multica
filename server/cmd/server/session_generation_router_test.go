package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/passkey"
)

// revocationUser inserts a workspace member so a bump never touches the shared
// integration user. gen-less tokens model every pre-generation JWT.
func revocationUser(t *testing.T, sid string) (id, token string) {
	t.Helper()
	ctx := context.Background()
	email := fmt.Sprintf("rev-%d@multica.ai", time.Now().UnixNano())
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Rev Test', $1) RETURNING id::text`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM member WHERE user_id = $1`, id)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, id)
	})
	claims := jwt.MapClaims{
		"sub": id, "email": email, "amr": "interactive",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}
	if sid != "" {
		claims["sid"] = sid
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	return id, signed
}

func bumpRevocationUser(t *testing.T, id string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE "user" SET session_generation = session_generation + 1 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func routerStatus(t *testing.T, method, path, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, testServer.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Gated routes read the stored generation on every request, so a revocation
// lands at once even when a recent request warmed the 30 s cache.
func TestSessionGeneration_GatedRoutesSkipTheCache(t *testing.T) {
	gated := []struct{ method, path string }{
		{"POST", "/api/passkeys/enroll/start"},
		{"POST", "/api/passkeys/00000000-0000-0000-0000-000000000001/revoke"},
		{"POST", "/api/decision-actions/00000000-0000-0000-0000-000000000001/approve"},
		{"POST", "/api/decision-requests/00000000-0000-0000-0000-000000000001/answer"},
		{"POST", "/api/decision-answers/00000000-0000-0000-0000-000000000001/disclaim"},
	}
	for _, route := range gated {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			id, token := revocationUser(t, "")
			if got := routerStatus(t, "GET", "/api/me", token); got != http.StatusOK {
				t.Fatalf("warm-up: got %d, want 200", got)
			}
			bumpRevocationUser(t, id)
			if got := routerStatus(t, route.method, route.path, token); got != http.StatusUnauthorized {
				t.Fatalf("revoked JWT on gated route: got %d, want 401", got)
			}
		})
	}
}

// Ordinary routes may trust a cached generation for 30 s, so these tests bump
// before the user's first request: nothing is cached yet.
func TestSessionGeneration_DaemonAuthRefusesRevokedJWT(t *testing.T) {
	for name, sid := range map[string]string{"with sid": "sid-d", "without sid": ""} {
		t.Run(name, func(t *testing.T) {
			id, token := revocationUser(t, sid)
			bumpRevocationUser(t, id)
			if got := routerStatus(t, "GET", "/api/daemon/workspaces", token); got != http.StatusUnauthorized {
				t.Fatalf("revoked JWT on daemon route: got %d, want 401", got)
			}
		})
	}
}

func TestSessionGeneration_WebSocketRefusesRevokedJWT(t *testing.T) {
	for name, sid := range map[string]string{"with sid": "sid-w", "without sid": ""} {
		t.Run(name, func(t *testing.T) {
			id, token := revocationUser(t, sid)
			bumpRevocationUser(t, id)
			url := "ws" + strings.TrimPrefix(testServer.URL, "http") + "/ws?workspace_id=" + testWorkspaceID
			conn, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Cookie": {auth.AuthCookieName + "=" + token}})
			if conn != nil {
				conn.Close()
			}
			if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("revoked JWT on /ws: want 401 on upgrade, got resp=%v err=%v", resp, err)
			}
		})
	}
}

// A current session still works on both, so the 401s above are revocation and
// not a broken fixture.
func TestSessionGeneration_CurrentJWTStillAccepted(t *testing.T) {
	_, token := revocationUser(t, "sid-ok")
	if got := routerStatus(t, "GET", "/api/daemon/workspaces", token); got != http.StatusOK {
		t.Fatalf("daemon route: got %d, want 200", got)
	}
	url := "ws" + strings.TrimPrefix(testServer.URL, "http") + "/ws?workspace_id=" + testWorkspaceID
	conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Cookie": {auth.AuthCookieName + "=" + token}})
	if err != nil {
		t.Fatalf("/ws dial: %v", err)
	}
	conn.Close()
}

// Recovery bumps the generation in the C00 command; every session minted
// before it must die at once on a gated route and on an ordinary one, and a
// cookie inside the renewal window must not be renewed into a new session.
func TestSessionGeneration_RecoveryKillsOldSessionsAndRenewal(t *testing.T) {
	for name, sid := range map[string]string{"with sid": "sid-r", "without sid": ""} {
		t.Run(name, func(t *testing.T) {
			id, token := revocationUser(t, sid)
			var email string
			if err := testPool.QueryRow(context.Background(), `SELECT email FROM "user" WHERE id = $1`, id).Scan(&email); err != nil {
				t.Fatal(err)
			}
			if _, err := passkey.RecoverUser(context.Background(), testPool, email); err != nil {
				t.Fatal(err)
			}
			for _, route := range []struct{ method, path string }{
				{"GET", "/api/daemon/workspaces"},
				{"POST", "/api/decision-actions/00000000-0000-0000-0000-000000000001/approve"},
				{"POST", "/api/passkeys/enroll/start"},
			} {
				if got := routerStatus(t, route.method, route.path, token); got != http.StatusUnauthorized {
					t.Fatalf("%s %s with a pre-recovery JWT: got %d, want 401", route.method, route.path, got)
				}
			}
			claims := jwt.MapClaims{
				"sub": id, "email": email, "amr": "interactive",
				"exp": time.Now().Add(auth.AuthRenewThreshold() / 2).Unix(), "iat": time.Now().Add(-time.Hour).Unix(),
			}
			if sid != "" {
				claims["sid"] = sid
			}
			cookie, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(auth.JWTSecret())
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest("GET", testServer.URL+"/api/me", nil)
			req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: cookie})
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("renewal-window cookie after recovery: got %d, want 401", resp.StatusCode)
			}
			for _, c := range resp.Cookies() {
				if c.Name == auth.AuthCookieName && c.Value != "" {
					t.Fatal("recovery let the old session renew itself")
				}
			}
		})
	}
}
