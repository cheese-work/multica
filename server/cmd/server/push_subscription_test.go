package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/auth"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-1795 (N3): Web Push subscriptions and the delivery outbox. These tests
// go through the real router and the existing inbox writers, so they name no
// symbol N3 adds.

const pushTestPublicKey = "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM"

func enablePush(t *testing.T) {
	t.Helper()
	t.Setenv("MULTICA_VAPID_PUBLIC_KEY", pushTestPublicKey)
	t.Setenv("MULTICA_VAPID_PRIVATE_KEY", "UUxI4O8-FbRouAevSmBQ6o18hgE4nSG3qwvJTfKc-ls")
	t.Setenv("MULTICA_VAPID_SUBJECT", "mailto:ops@example.com")
}

// sessionJWT is an interactive login: the `sid` claim names the session.
func sessionJWT(t *testing.T, userID, sessionID string, exp time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{"sub": userID, "email": "push-test@multica.ai", "amr": "interactive", "exp": exp.Unix(), "iat": time.Now().Unix()}
	if sessionID != "" {
		claims["sid"] = sessionID
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatalf("sign session jwt: %v", err)
	}
	return token
}

// pushRequest authenticates with the given token only for this call, so
// concurrent requests of different users do not share state.
func pushRequest(t *testing.T, method, path, token string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, testServer.URL+path, reader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("request failed: %v", err)
		return &http.Response{StatusCode: 0, Body: http.NoBody, Header: http.Header{}}
	}
	return resp
}

func pushSubscriptionBody(t *testing.T, endpoint string) map[string]any {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate p256dh: %v", err)
	}
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}
	return map[string]any{
		"endpoint": endpoint,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(secret),
		},
	}
}

// cleanPushState removes every subscription (and its outbox rows) of the given
// users, now and when the test ends.
func cleanPushState(t *testing.T, userIDs ...string) {
	t.Helper()
	clean := func() {
		for _, userID := range userIDs {
			testPool.Exec(context.Background(), `DELETE FROM push_delivery WHERE subscription_id IN (SELECT id FROM push_subscription WHERE user_id = $1)`, userID)
			testPool.Exec(context.Background(), `DELETE FROM push_subscription WHERE user_id = $1`, userID)
		}
		testPool.Exec(context.Background(), `DELETE FROM inbox_item WHERE workspace_id = $1 AND recipient_id = $2 AND details->>'push_test' = 'true'`, testWorkspaceID, testUserID)
		testPool.Exec(context.Background(), `DELETE FROM notification_channel_preference WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, testUserID)
	}
	clean()
	t.Cleanup(clean)
}

func registerPush(t *testing.T, token string, body map[string]any, want int) {
	t.Helper()
	resp := pushRequest(t, http.MethodPost, "/api/push/subscriptions", token, body)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("POST /api/push/subscriptions: expected %d, got %d", want, resp.StatusCode)
	}
}

func pushSubscriptionOwner(t *testing.T, endpoint string) (string, bool) {
	t.Helper()
	var owner string
	err := testPool.QueryRow(context.Background(), `SELECT user_id::text FROM push_subscription WHERE endpoint = $1`, endpoint).Scan(&owner)
	return owner, err == nil
}

func TestPushConfigIsDisabledWithoutKeys(t *testing.T) {
	t.Setenv("MULTICA_VAPID_PUBLIC_KEY", "")
	t.Setenv("MULTICA_VAPID_PRIVATE_KEY", "")
	t.Setenv("MULTICA_VAPID_SUBJECT", "")

	resp := authRequest(t, http.MethodGet, "/api/push/config", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/push/config: expected 200, got %d", resp.StatusCode)
	}
	var cfg struct {
		Enabled   bool   `json:"enabled"`
		PublicKey string `json:"public_key"`
	}
	readJSON(t, resp, &cfg)
	if cfg.Enabled || cfg.PublicKey != "" {
		t.Fatalf("config = %+v, want disabled with no key", cfg)
	}

	// No keys, no registrations: nothing could ever be sent to them.
	cleanPushState(t, testUserID)
	registerPush(t, sessionJWT(t, testUserID, "sid-disabled", time.Now().Add(time.Hour)),
		pushSubscriptionBody(t, "https://push.example.com/send/disabled"), http.StatusServiceUnavailable)
}

func TestPushConfigExposesOnlyThePublicKey(t *testing.T) {
	enablePush(t)

	resp := authRequest(t, http.MethodGet, "/api/push/config", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/push/config: expected 200, got %d", resp.StatusCode)
	}
	var raw map[string]any
	readJSON(t, resp, &raw)
	if raw["enabled"] != true || raw["public_key"] != pushTestPublicKey || len(raw) != 2 {
		t.Fatalf("config = %+v, want {enabled:true, public_key:<key>} and nothing else", raw)
	}
}

func TestPushSubscriptionUpsertsByEndpoint(t *testing.T) {
	enablePush(t)
	cleanPushState(t, testUserID)
	token := sessionJWT(t, testUserID, "sid-upsert", time.Now().Add(time.Hour))
	endpoint := "https://push.example.com/send/upsert"

	registerPush(t, token, pushSubscriptionBody(t, endpoint), http.StatusOK)
	registerPush(t, token, pushSubscriptionBody(t, endpoint), http.StatusOK)

	var n int
	var sessionID string
	var validUntil time.Time
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*), max(session_id), max(session_valid_until) FROM push_subscription WHERE endpoint = $1`, endpoint).Scan(&n, &sessionID, &validUntil); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if n != 1 || sessionID != "sid-upsert" {
		t.Fatalf("rows = %d, session = %q, want one row stamped with the registering session", n, sessionID)
	}
	if d := time.Until(validUntil); d < 50*time.Minute || d > 70*time.Minute {
		t.Fatalf("session_valid_until in %v, want about the session's own expiry (1h)", d)
	}
}

func TestPushSubscriptionRejectsWhatCannotBeDelivered(t *testing.T) {
	enablePush(t)
	cleanPushState(t, testUserID)
	session := sessionJWT(t, testUserID, "sid-reject", time.Now().Add(time.Hour))

	for name, endpoint := range map[string]string{
		"plain http":      "http://push.example.com/send/x",
		"loopback":        "https://127.0.0.1/send/x",
		"localhost":       "https://localhost/send/x",
		"private literal": "https://10.0.0.5/send/x",
		"no host":         "https:///send/x",
		"not a url":       "push-service",
		"too long":        "https://push.example.com/" + strings.Repeat("a", 2100),
	} {
		t.Run(name, func(t *testing.T) { registerPush(t, session, pushSubscriptionBody(t, endpoint), http.StatusBadRequest) })
	}

	t.Run("bad keys", func(t *testing.T) {
		body := pushSubscriptionBody(t, "https://push.example.com/send/keys")
		body["keys"] = map[string]string{"p256dh": "AAAA", "auth": "AAAA"}
		registerPush(t, session, body, http.StatusBadRequest)
	})
	t.Run("no keys", func(t *testing.T) {
		registerPush(t, session, map[string]any{"endpoint": "https://push.example.com/send/none"}, http.StatusBadRequest)
	})
	t.Run("session without sid", func(t *testing.T) {
		registerPush(t, sessionJWT(t, testUserID, "", time.Now().Add(time.Hour)),
			pushSubscriptionBody(t, "https://push.example.com/send/nosid"), http.StatusBadRequest)
	})

	if n := dbfx.New(testPool, testWorkspaceID, testUserID).Count(t, `SELECT count(*) FROM push_subscription WHERE user_id = $1`, testUserID); n != 0 {
		t.Fatalf("%d subscriptions stored after rejected requests, want 0", n)
	}
}

func TestPushSubscriptionMovesToTheNewLogin(t *testing.T) {
	enablePush(t)
	fx := dbfx.New(testPool, testWorkspaceID, testUserID)
	other := fx.User(t, "Push Other", fmt.Sprintf("push-other-%d@multica.ai", time.Now().UnixNano()))
	cleanPushState(t, testUserID, other)
	endpoint := "https://push.example.com/send/shared-device"

	registerPush(t, sessionJWT(t, testUserID, "sid-first", time.Now().Add(time.Hour)), pushSubscriptionBody(t, endpoint), http.StatusOK)
	registerPush(t, sessionJWT(t, other, "sid-second", time.Now().Add(time.Hour)), pushSubscriptionBody(t, endpoint), http.StatusOK)

	if owner, ok := pushSubscriptionOwner(t, endpoint); !ok || owner != other {
		t.Fatalf("owner = %q (found %v), want the user who logged in last (%s)", owner, ok, other)
	}

	// The previous account can no longer remove, or stop, the new owner's device.
	resp := pushRequest(t, http.MethodDelete, "/api/push/subscriptions", sessionJWT(t, testUserID, "sid-first", time.Now().Add(time.Hour)), map[string]string{"endpoint": endpoint})
	resp.Body.Close()
	if _, ok := pushSubscriptionOwner(t, endpoint); !ok {
		t.Fatalf("DELETE by the previous owner removed the new owner's subscription (status %d)", resp.StatusCode)
	}
}

func TestPushSubscriptionDeleteByEndpoint(t *testing.T) {
	enablePush(t)
	cleanPushState(t, testUserID)
	token := sessionJWT(t, testUserID, "sid-delete", time.Now().Add(time.Hour))
	endpoint := "https://push.example.com/send/delete"
	registerPush(t, token, pushSubscriptionBody(t, endpoint), http.StatusOK)

	resp := pushRequest(t, http.MethodDelete, "/api/push/subscriptions", token, map[string]string{"endpoint": endpoint})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", resp.StatusCode)
	}
	if _, ok := pushSubscriptionOwner(t, endpoint); ok {
		t.Fatal("subscription still stored after DELETE")
	}

	// Deleting what is already gone is not an error: logout must always finish.
	resp = pushRequest(t, http.MethodDelete, "/api/push/subscriptions", token, map[string]string{"endpoint": endpoint})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second DELETE: expected 204, got %d", resp.StatusCode)
	}
}

func TestLogoutEndsTheRegisteringSessionsSubscriptions(t *testing.T) {
	enablePush(t)
	cleanPushState(t, testUserID)
	loggedOut := sessionJWT(t, testUserID, "sid-logout", time.Now().Add(time.Hour))
	stillIn := sessionJWT(t, testUserID, "sid-other-device", time.Now().Add(time.Hour))
	registerPush(t, loggedOut, pushSubscriptionBody(t, "https://push.example.com/send/logout"), http.StatusOK)
	registerPush(t, stillIn, pushSubscriptionBody(t, "https://push.example.com/send/stays"), http.StatusOK)

	// The client could not send its DELETE (offline, expired page): logout alone must stop the pushes.
	resp := pushRequest(t, http.MethodPost, "/auth/logout", loggedOut, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /auth/logout: expected 200, got %d", resp.StatusCode)
	}

	ended := func(sessionID string) bool {
		return dbfx.New(testPool, testWorkspaceID, testUserID).Count(t, `SELECT count(*) FROM push_session_end WHERE session_id = $1`, sessionID) == 1
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM push_session_end WHERE session_id IN ('sid-logout', 'sid-other-device')`)
	})
	if !ended("sid-logout") {
		t.Fatal("the logged-out session is not marked ended")
	}
	if ended("sid-other-device") {
		t.Fatal("logout of one session ended another session")
	}
}

// lockDevice holds the device row the way an in-flight send does.
func lockDevice(t *testing.T, endpoint string) (release func()) {
	t.Helper()
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `SELECT 1 FROM push_subscription WHERE endpoint = $1 FOR NO KEY UPDATE`, endpoint); err != nil {
		t.Fatalf("lock device: %v", err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			_ = tx.Commit(context.Background())
		}
	}
	t.Cleanup(release)
	return release
}

// CHE-1795: registration and logout do not get an end-to-end 15 s
// guarantee from the send timeout. Contract under test: a registration or
// delete that cannot get the device row quickly fails with 503 and Retry-After,
// and logout never waits behind a send.

func TestPushRegistrationFailsFastWhenTheDeviceIsBusy(t *testing.T) {
	enablePush(t)
	fx := dbfx.New(testPool, testWorkspaceID, testUserID)
	other := fx.User(t, "Push Busy Other", fmt.Sprintf("push-busy-%d@multica.ai", time.Now().UnixNano()))
	cleanPushState(t, testUserID, other)
	endpoint := "https://push.example.com/send/busy"
	registerPush(t, sessionJWT(t, testUserID, "sid-busy", time.Now().Add(time.Hour)), pushSubscriptionBody(t, endpoint), http.StatusOK)
	release := lockDevice(t, endpoint)

	type result struct {
		status     int
		retryAfter string
	}
	call := func(method string, token string, body any) <-chan result {
		out := make(chan result, 1)
		go func() {
			resp := pushRequest(t, method, "/api/push/subscriptions", token, body)
			resp.Body.Close()
			out <- result{resp.StatusCode, resp.Header.Get("Retry-After")}
		}()
		return out
	}
	transfer := call(http.MethodPost, sessionJWT(t, other, "sid-busy-other", time.Now().Add(time.Hour)), pushSubscriptionBody(t, endpoint))
	remove := call(http.MethodDelete, sessionJWT(t, testUserID, "sid-busy", time.Now().Add(time.Hour)), map[string]string{"endpoint": endpoint})

	started := time.Now()
	for name, ch := range map[string]<-chan result{"transfer": transfer, "delete": remove} {
		select {
		case r := <-ch:
			t.Logf("%s answered %d after %v", name, r.status, time.Since(started).Round(100*time.Millisecond))
			if r.status != http.StatusServiceUnavailable || r.retryAfter == "" {
				t.Errorf("%s on a busy device: status %d, Retry-After %q, want 503 with Retry-After", name, r.status, r.retryAfter)
			}
		case <-time.After(8 * time.Second): // 5 s request budget + 2 s teardown budget, with slack
			release()
			t.Errorf("%s still waiting on a busy device after 8 s: it must fail fast instead of queueing behind the send", name)
		}
	}
	release()
}

func TestLogoutIsNotBlockedByAnInFlightSend(t *testing.T) {
	enablePush(t)
	cleanPushState(t, testUserID)
	token := sessionJWT(t, testUserID, "sid-logout-busy", time.Now().Add(time.Hour))
	endpoint := "https://push.example.com/send/logout-busy"
	registerPush(t, token, pushSubscriptionBody(t, endpoint), http.StatusOK)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM push_session_end WHERE session_id = 'sid-logout-busy'`)
	})
	release := lockDevice(t, endpoint)

	done := make(chan int, 1)
	go func() {
		resp := pushRequest(t, http.MethodPost, "/auth/logout", token, nil)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case status := <-done:
		if status != http.StatusOK {
			t.Errorf("logout: status %d, want 200", status)
		}
	case <-time.After(2 * time.Second):
		release()
		<-done
		t.Fatal("logout waited behind an in-flight send: ending a session must not need the device row")
	}
	release()
	if n := dbfx.New(testPool, testWorkspaceID, testUserID).Count(t, `SELECT count(*) FROM push_session_end WHERE session_id = 'sid-logout-busy'`); n != 1 {
		t.Fatal("logout did not mark the session ended")
	}
}

// ---- outbox ---------------------------------------------------------------

func setChannel(t *testing.T, source, channel string, enabled bool) {
	t.Helper()
	resp := authRequest(t, http.MethodPatch, "/api/notification-channels", map[string]any{"source": source, "channel": channel, "enabled": enabled})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH channel %s/%s: expected 200, got %d", source, channel, resp.StatusCode)
	}
}

func pendingDeliveries(t *testing.T, endpoint string) []time.Time {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT d.next_attempt_at FROM push_delivery d JOIN push_subscription s ON s.id = d.subscription_id
		  WHERE s.endpoint = $1 AND d.sent_at IS NULL ORDER BY d.next_attempt_at`, endpoint)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		out = append(out, at)
	}
	return out
}

func newInboxItem(t *testing.T, userID string) db.InboxItem {
	t.Helper()
	item, err := db.New(testPool).CreateInboxItem(context.Background(), db.CreateInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: parseTestUUID(t, testWorkspaceID), RecipientType: "member", RecipientID: parseTestUUID(t, userID),
		Type: "mentioned", Severity: "info", Title: "Push outbox", Details: []byte(`{"push_test":"true"}`),
	})
	if err != nil {
		t.Fatalf("create inbox item: %v", err)
	}
	return item
}

func parseTestUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}

func replyToDM(t *testing.T, sessionID pgtype.UUID) db.InboxItem {
	t.Helper()
	item, err := db.New(testPool).UpsertDMReplyInboxItem(context.Background(), db.UpsertDMReplyInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: parseTestUUID(t, testWorkspaceID), RecipientID: parseTestUUID(t, testUserID),
		Title: "DM", ChatSessionID: sessionID, ChatMessageID: dbid.NewV7(),
		DedupeKey: pgtype.Text{String: "dm:" + fmt.Sprint(sessionID.Bytes), Valid: true},
	})
	if err != nil {
		t.Fatalf("upsert dm reply: %v", err)
	}
	testPool.Exec(context.Background(), `UPDATE inbox_item SET details = details || '{"push_test":"true"}'::jsonb WHERE id = $1`, item.ID)
	return item
}

func registerDevice(t *testing.T, name string) string {
	t.Helper()
	endpoint := "https://push.example.com/send/" + name
	registerPush(t, sessionJWT(t, testUserID, "sid-"+name, time.Now().Add(time.Hour)), pushSubscriptionBody(t, endpoint), http.StatusOK)
	return endpoint
}

func TestPushOutboxQueuesAnInboxItemTenSecondsOut(t *testing.T) {
	enablePush(t)
	cleanPushState(t, testUserID)
	endpoint := registerDevice(t, "outbox-inbox")
	setChannel(t, "inbox", "push", true)

	newInboxItem(t, testUserID)

	rows := pendingDeliveries(t, endpoint)
	if len(rows) != 1 {
		t.Fatalf("pending deliveries = %d, want 1", len(rows))
	}
	if d := time.Until(rows[0]); d < 5*time.Second || d > 15*time.Second {
		t.Fatalf("first attempt in %v, want about 10s so a quick read can cancel it", d)
	}
}

func TestPushOutboxFollowsTheSourceSwitch(t *testing.T) {
	enablePush(t)
	cleanPushState(t, testUserID)
	endpoint := registerDevice(t, "outbox-switch")

	newInboxItem(t, testUserID)
	replyToDM(t, dbid.NewV7())
	if n := len(pendingDeliveries(t, endpoint)); n != 0 {
		t.Fatalf("pending deliveries = %d with every push switch off (the default), want 0", n)
	}

	setChannel(t, "dm", "push", true)
	newInboxItem(t, testUserID)
	if n := len(pendingDeliveries(t, endpoint)); n != 0 {
		t.Fatalf("pending deliveries = %d: the dm switch must not enable inbox pushes", n)
	}
	replyToDM(t, dbid.NewV7())
	if n := len(pendingDeliveries(t, endpoint)); n != 1 {
		t.Fatalf("pending deliveries = %d after a DM reply with dm push on, want 1", n)
	}

	setChannel(t, "dm", "push", false)
	setChannel(t, "inbox", "push", true)
	replyToDM(t, dbid.NewV7())
	if n := len(pendingDeliveries(t, endpoint)); n != 1 {
		t.Fatalf("pending deliveries = %d: the inbox switch must not enable DM pushes", n)
	}
}

func TestPushOutboxCoalescesRepliesIntoOnePendingRow(t *testing.T) {
	enablePush(t)
	cleanPushState(t, testUserID)
	endpoint := registerDevice(t, "outbox-coalesce")
	setChannel(t, "dm", "push", true)
	session := dbid.NewV7()

	replyToDM(t, session)
	first := pendingDeliveries(t, endpoint)
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 9; i++ {
		replyToDM(t, session)
	}

	rows := pendingDeliveries(t, endpoint)
	if len(rows) != 1 {
		t.Fatalf("pending deliveries = %d after ten replies, want 1", len(rows))
	}
	if len(first) != 1 || !rows[0].After(first[0]) {
		t.Fatalf("next attempt %v did not move past %v: a coalesced reply must push the send out, not add a row", rows, first)
	}
}

func TestPushOutboxSkipsOtherUsersSubscriptions(t *testing.T) {
	enablePush(t)
	fx := dbfx.New(testPool, testWorkspaceID, testUserID)
	other := fx.User(t, "Push Bystander", fmt.Sprintf("push-bystander-%d@multica.ai", time.Now().UnixNano()))
	cleanPushState(t, testUserID, other)
	registerPush(t, sessionJWT(t, other, "sid-bystander", time.Now().Add(time.Hour)),
		pushSubscriptionBody(t, "https://push.example.com/send/bystander"), http.StatusOK)
	setChannel(t, "inbox", "push", true)

	newInboxItem(t, testUserID)

	if n := len(pendingDeliveries(t, "https://push.example.com/send/bystander")); n != 0 {
		t.Fatalf("pending deliveries = %d on another user's device, want 0", n)
	}
}
