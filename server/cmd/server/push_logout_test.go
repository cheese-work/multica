package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/pushnotify"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1795: logout has to end the
// session for good, and a send admitted before it must be over, or visibly
// still running, by the time logout answers.

type gateSender struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGateSender() *gateSender {
	return &gateSender{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (g *gateSender) Send(ctx context.Context, _ pushnotify.Subscription, _ []byte) (int, error) {
	g.entered <- struct{}{}
	select {
	case <-g.release:
		return 201, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (g *gateSender) open() { g.once.Do(func() { close(g.release) }) }

// startAdmittedSend registers a device for `sid`, makes one push due and runs a
// worker until it is inside Send, i.e. past its check of the session.
func startAdmittedSend(t *testing.T, sid string) (token string, gate *gateSender, pass <-chan error) {
	t.Helper()
	enablePush(t)
	cleanPushState(t, testUserID)
	token = sessionJWT(t, testUserID, sid, time.Now().Add(time.Hour))
	endpoint := "https://push.example.com/send/" + sid
	registerPush(t, token, pushSubscriptionBody(t, endpoint), http.StatusOK)
	setChannel(t, "inbox", "push", true)
	newInboxItem(t, testUserID)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE push_delivery d SET next_attempt_at = now() - interval '1 second' FROM push_subscription s WHERE d.subscription_id = s.id AND s.endpoint = $1`, endpoint); err != nil {
		t.Fatalf("make the push due: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM push_session_end WHERE session_id = $1`, sid) })

	gate = newGateSender()
	t.Cleanup(gate.open)
	done := make(chan error, 1)
	go func() {
		done <- pushnotify.NewWorker(db.New(testPool), testPool, gate).ProcessOnce(context.Background())
	}()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never reached the send")
	}
	return token, gate, done
}

func endedSession(t *testing.T, sid string) bool {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM push_session_end WHERE session_id = $1`, sid).Scan(&n); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return n == 1
}

func TestLogoutWaitsForASendAdmittedBeforeIt(t *testing.T) {
	token, gate, pass := startAdmittedSend(t, "sid-drain")

	logout := make(chan int, 1)
	started := time.Now()
	go func() {
		resp := pushRequest(t, http.MethodPost, "/auth/logout", token, nil)
		resp.Body.Close()
		logout <- resp.StatusCode
	}()
	select {
	case <-logout:
		gate.open()
		<-pass
		t.Fatal("logout answered while a send admitted before it was still in flight: no send may start after logout answers")
	case <-time.After(700 * time.Millisecond):
	}
	gate.open()
	select {
	case status := <-logout:
		if status != http.StatusOK {
			t.Errorf("logout: status %d, want 200", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logout did not answer after the send ended")
	}
	if err := <-pass; err != nil {
		t.Fatalf("process pass: %v", err)
	}
	if time.Since(started) > 4*time.Second {
		t.Errorf("logout took %v for a send that lasted under a second", time.Since(started))
	}
	if !endedSession(t, "sid-drain") {
		t.Fatal("logout did not record the session as ended")
	}
}

// CHE-1795: when the drain cannot
// show that no admitted send is left, logout must not report success. The
// marker stays recorded, the cookie stays, and a retry after the worker is
// done completes. Holding the sender gate at Send entry stands for a worker
// that is admitted (it holds the shared session lock) and has not yet sent.
func TestLogoutRefusesToSucceedWhileAnAdmittedSendOutlastsTheDrain(t *testing.T) {
	token, gate, pass := startAdmittedSend(t, "sid-slow")

	started := time.Now()
	resp := pushRequest(t, http.MethodPost, "/auth/logout", token, nil)
	resp.Body.Close()
	elapsed := time.Since(started)

	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		gate.open()
		<-pass
		t.Fatalf("logout behind a send that outlasts the drain: status %d, Retry-After %q, want 503 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if clearsAuthCookie(resp) {
		t.Error("the auth cookie was cleared although the drain did not finish: the client could no longer retry")
	}
	if elapsed > 8*time.Second {
		t.Errorf("logout took %v behind a slow send, want it bounded by the drain budget plus teardown", elapsed)
	}
	if !endedSession(t, "sid-slow") {
		t.Error("the marker was not kept: later deliveries would be admitted again")
	}

	gate.open() // the admitted worker finishes
	if err := <-pass; err != nil {
		t.Fatalf("process pass: %v", err)
	}
	retry := pushRequest(t, http.MethodPost, "/auth/logout", token, nil)
	retry.Body.Close()
	if retry.StatusCode != http.StatusOK || !clearsAuthCookie(retry) {
		t.Fatalf("retry after the worker finished: status %d, cookie cleared %v, want 200 and the cookie cleared", retry.StatusCode, clearsAuthCookie(retry))
	}
}

// clearsAuthCookie reports whether the response expires the auth cookie, the
// signal that the client is logged out.
func clearsAuthCookie(resp *http.Response) bool {
	for _, cookie := range resp.Cookies() {
		if cookie.Name == auth.AuthCookieName && (cookie.MaxAge < 0 || cookie.Value == "") {
			return true
		}
	}
	return false
}

func TestEndedSessionCannotRegisterAgain(t *testing.T) {
	enablePush(t)
	cleanPushState(t, testUserID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM push_session_end WHERE session_id = 'sid-renewed'`)
	})
	token := sessionJWT(t, testUserID, "sid-renewed", time.Now().Add(time.Hour))
	registerPush(t, token, pushSubscriptionBody(t, "https://push.example.com/send/renewed-first"), http.StatusOK)
	resp := pushRequest(t, http.MethodPost, "/auth/logout", token, nil)
	resp.Body.Close()

	// Sliding renewal keeps the `sid`: the same session, a later token.
	renewed := sessionJWT(t, testUserID, "sid-renewed", time.Now().Add(30*24*time.Hour))
	resp = pushRequest(t, http.MethodPost, "/api/push/subscriptions", renewed, pushSubscriptionBody(t, "https://push.example.com/send/renewed-second"))
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("registering with an ended session: status %d, want 401", resp.StatusCode)
	}
	if _, found := pushSubscriptionOwner(t, "https://push.example.com/send/renewed-second"); found {
		t.Fatal("a subscription was stored for an ended session")
	}
}

// CHE-1795: ending a session must
// not depend on the current push configuration. Subscriptions registered while
// push was configured survive a configuration change, and the same keys may
// come back before the saved session expiry.

// countingSender counts the pushes that would leave the process.
type countingSender struct {
	mu sync.Mutex
	n  int
}

func (c *countingSender) Send(context.Context, pushnotify.Subscription, []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return 201, nil
}

func (c *countingSender) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestLogoutEndsTheSessionWhateverThePushConfigurationIs(t *testing.T) {
	gates := map[string][]string{
		"public key missing":   {"MULTICA_VAPID_PUBLIC_KEY"},
		"private key missing":  {"MULTICA_VAPID_PRIVATE_KEY"},
		"subject missing":      {"MULTICA_VAPID_SUBJECT"},
		"push off":             {"MULTICA_VAPID_PUBLIC_KEY", "MULTICA_VAPID_PRIVATE_KEY", "MULTICA_VAPID_SUBJECT"},
		"no gate (worker off)": nil,
	}
	for name, unset := range gates {
		t.Run(name, func(t *testing.T) {
			sid := "sid-cfg-" + strings.ReplaceAll(strings.ReplaceAll(name, " ", "-"), "(", "")
			sid = strings.ReplaceAll(sid, ")", "")
			enablePush(t)
			cleanPushState(t, testUserID)
			t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM push_session_end WHERE session_id = $1`, sid) })
			token := sessionJWT(t, testUserID, sid, time.Now().Add(time.Hour))
			endpoint := "https://push.example.com/send/" + sid
			registerPush(t, token, pushSubscriptionBody(t, endpoint), http.StatusOK)
			setChannel(t, "inbox", "push", true)
			newInboxItem(t, testUserID)
			if _, err := testPool.Exec(context.Background(),
				`UPDATE push_delivery d SET next_attempt_at = now() - interval '1 second' FROM push_subscription s WHERE d.subscription_id = s.id AND s.endpoint = $1`, endpoint); err != nil {
				t.Fatalf("make the push due: %v", err)
			}

			// Push is misconfigured, or off, only while the user logs out.
			saved := map[string]string{}
			for _, key := range unset {
				saved[key] = os.Getenv(key)
				os.Unsetenv(key)
			}
			resp := pushRequest(t, http.MethodPost, "/auth/logout", token, nil)
			resp.Body.Close()
			for key, value := range saved {
				os.Setenv(key, value)
			}

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("logout: status %d, want 200", resp.StatusCode)
			}
			if !endedSession(t, sid) {
				t.Fatal("logout with push unconfigured did not record the session end: re-enabling push would send to the old session")
			}
			// Push is back before the saved session expiry.
			sender := &countingSender{}
			if err := pushnotify.NewWorker(db.New(testPool), testPool, sender).ProcessOnce(context.Background()); err != nil {
				t.Fatalf("process pass: %v", err)
			}
			if sender.count() != 0 {
				t.Fatal("a worker sent to a device of the ended session after push came back")
			}
			resp = pushRequest(t, http.MethodPost, "/api/push/subscriptions", sessionJWT(t, testUserID, sid, time.Now().Add(time.Hour)),
				pushSubscriptionBody(t, endpoint+"-again"))
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("re-registering the ended session: status %d, want 401", resp.StatusCode)
			}
		})
	}
}

func TestANewLoginRegistersAfterAnotherSessionEnded(t *testing.T) {
	enablePush(t)
	cleanPushState(t, testUserID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM push_session_end WHERE session_id = 'sid-old-login'`)
	})
	old := sessionJWT(t, testUserID, "sid-old-login", time.Now().Add(time.Hour))
	resp := pushRequest(t, http.MethodPost, "/auth/logout", old, nil)
	resp.Body.Close()

	registerPush(t, sessionJWT(t, testUserID, "sid-new-login", time.Now().Add(time.Hour)),
		pushSubscriptionBody(t, "https://push.example.com/send/new-login"), http.StatusOK)
}
