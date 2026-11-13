package main

import (
	"context"
	"fmt"
	"io"
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
		{"POST", "/api/tokens"},             // a PAT outlives the JWT and skips its generation check
		{"POST", "/api/cli-token"},          // both sign a new JWT with the stored generation
		{"POST", "/api/auth/desktop-token"}, // so a stale one would come back as a fresh session
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

// A request can pass the middleware and then hold its body open across the
// recovery commit. The PAT must not be minted: the handler rechecks the
// generation under the user-row lock, in the transaction that inserts it.
func TestSessionGeneration_PATWithADelayedBodyIsRefusedAcrossRecovery(t *testing.T) {
	id, token := revocationUser(t, "")
	var email string
	if err := testPool.QueryRow(context.Background(), `SELECT email FROM "user" WHERE id = $1`, id).Scan(&email); err != nil {
		t.Fatal(err)
	}
	body, writer := io.Pipe()
	req, err := http.NewRequest("POST", testServer.URL+"/api/tokens", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	status := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			status <- -1
			return
		}
		resp.Body.Close()
		status <- resp.StatusCode
	}()
	if _, err := writer.Write([]byte(`{"name":"stolen"`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the middleware has read the generation by now
	if _, err := passkey.RecoverUser(context.Background(), testPool, email); err != nil {
		t.Fatal(err)
	}
	writer.Write([]byte(`}`))
	writer.Close()
	if got := <-status; got != http.StatusUnauthorized {
		t.Fatalf("PAT minted across recovery: got %d, want 401", got)
	}
	var minted int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM personal_access_token WHERE user_id = $1`, id).Scan(&minted); err != nil || minted != 0 {
		t.Fatalf("PAT rows after recovery = %d (%v), want 0", minted, err)
	}
}

// The same request with no recovery in between still mints, so the 401 above is
// the recheck and not a broken fixture.
func TestSessionGeneration_PATIsMintedForACurrentSession(t *testing.T) {
	id, token := revocationUser(t, "")
	req, _ := http.NewRequest("POST", testServer.URL+"/api/tokens", strings.NewReader(`{"name":"ok"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("current session: got %d, want 201", resp.StatusCode)
	}
	testPool.Exec(context.Background(), `DELETE FROM personal_access_token WHERE user_id = $1`, id)
}

// A handoff token is signed with the stored generation. If recovery is still
// uncommitted when the request passes the middleware, the handler must wait for
// the user-row lock and then refuse, not sign a session that outlives recovery.
func TestSessionGeneration_HandoffTokenIsRefusedAcrossRecovery(t *testing.T) {
	for _, path := range []string{"/api/cli-token", "/api/auth/desktop-token"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			id, token := revocationUser(t, "")
			recovery, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer recovery.Rollback(ctx)
			if _, err := recovery.Exec(ctx, `UPDATE "user" SET session_generation = session_generation + 1 WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
			status := make(chan int, 1)
			go func() { status <- routerStatus(t, "POST", path, token) }()
			time.Sleep(300 * time.Millisecond) // the middleware has read the old generation by now
			if err := recovery.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if got := <-status; got != http.StatusUnauthorized {
				t.Fatalf("handoff token signed across recovery: got %d, want 401", got)
			}
		})
	}
}

func TestSessionGeneration_HandoffTokenIsIssuedToACurrentSession(t *testing.T) {
	for _, path := range []string{"/api/cli-token", "/api/auth/desktop-token"} {
		_, token := revocationUser(t, "")
		if got := routerStatus(t, "POST", path, token); got != http.StatusOK {
			t.Fatalf("%s for a current session: got %d, want 200", path, got)
		}
	}
}

func dialDaemonSocket(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(testServer.URL, "http") + "/api/daemon/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer " + token}})
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	return conn, resp, err
}

func sendClaimFrame(t *testing.T, conn *websocket.Conn) error {
	t.Helper()
	frame := `{"type":"daemon:rpc_request","payload":{"request_id":"claim-1","method":"tasks.claim","body":{}}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := conn.ReadMessage()
	return err
}

// A JWT daemon socket opened before recovery must not claim work after it:
// the frame is refused and the socket closed, whatever the 30 s cache holds.
func TestSessionGeneration_DaemonSocketIsClosedAcrossRecovery(t *testing.T) {
	id, token := revocationUser(t, "sid-dws")
	conn, resp, err := dialDaemonSocket(t, token)
	if err != nil {
		t.Fatalf("current JWT must open the daemon socket: %v (resp %v)", err, resp)
	}
	bumpRevocationUser(t, id)
	if err := sendClaimFrame(t, conn); err == nil {
		t.Fatal("a revoked session got an answer to tasks.claim on its open daemon socket")
	} else if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
		t.Fatalf("socket stayed open after revocation: %v", err)
	}
}

func TestSessionGeneration_DaemonSocketRefusesRevokedJWTOnConnect(t *testing.T) {
	id, token := revocationUser(t, "sid-dws2")
	routerStatus(t, "GET", "/api/daemon/workspaces", token) // warm the 30 s cache
	bumpRevocationUser(t, id)
	conn, resp, err := dialDaemonSocket(t, token)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked JWT on /api/daemon/ws with a warm cache: want 401, got conn=%v resp=%v err=%v", conn != nil, resp, err)
	}
	if got := routerStatus(t, "GET", "/api/daemon/workspaces", token); got != http.StatusUnauthorized {
		t.Fatalf("revoked JWT on a daemon HTTP route with a warm cache: got %d, want 401", got)
	}
}
