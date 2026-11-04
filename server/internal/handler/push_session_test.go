package handler

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/auth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-1795: logout must fail
// loudly when the session end cannot be recorded, and every teardown path of a
// push request needs its own deadline.

func pushHandlerEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MULTICA_VAPID_PUBLIC_KEY", "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM")
	t.Setenv("MULTICA_VAPID_PRIVATE_KEY", "UUxI4O8-FbRouAevSmBQ6o18hgE4nSG3qwvJTfKc-ls")
	t.Setenv("MULTICA_VAPID_SUBJECT", "mailto:ops@example.com")
}

func pushSessionToken(t *testing.T, sid string) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": testUserID, "sid": sid, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

// pushFailingDB is a store that is down.
type pushFailingDB struct{}

func (pushFailingDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("marker store is down")
}
func (pushFailingDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, errors.New("marker store is down")
}
func (pushFailingDB) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return pushFailingRow{}
}

type pushFailingRow struct{}

func (pushFailingRow) Scan(...interface{}) error { return errors.New("marker store is down") }

func TestLogoutFailsLoudlyWhenTheSessionEndCannotBeRecorded(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	pushHandlerEnv(t)
	saved := testHandler.Queries
	testHandler.Queries = db.New(pushFailingDB{})
	t.Cleanup(func() { testHandler.Queries = saved })

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+pushSessionToken(t, "sid-store-down"))
	rec := httptest.NewRecorder()
	testHandler.Logout(rec, req)

	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("logout with the marker store down: status %d, Retry-After %q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == auth.AuthCookieName {
			t.Fatal("the auth cookie was cleared although the session end was not recorded: the client could no longer retry")
		}
	}
}

type pushRecordingStarter struct {
	pool      pushTxPool
	mu        sync.Mutex
	remaining []time.Duration
}

type pushTxPool interface {
	Begin(context.Context) (pgx.Tx, error)
}

type pushRecordingTx struct {
	pgx.Tx
	owner *pushRecordingStarter
}

func (s *pushRecordingStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return pushRecordingTx{Tx: tx, owner: s}, nil
}

func (t pushRecordingTx) Rollback(ctx context.Context) error {
	t.owner.mu.Lock()
	if deadline, ok := ctx.Deadline(); ok {
		t.owner.remaining = append(t.owner.remaining, time.Until(deadline))
	} else {
		t.owner.remaining = append(t.owner.remaining, -1)
	}
	t.owner.mu.Unlock()
	return t.Tx.Rollback(ctx)
}

func TestBusyDeviceRequestRollsBackWithinADeadline(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	pushHandlerEnv(t)
	endpoint := "https://push.example.com/send/handler-busy"
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
	if _, err := testHandler.Queries.UpsertPushSubscription(context.Background(), db.UpsertPushSubscriptionParams{
		ID: dbid.NewV7(), UserID: parseUUID(testUserID), Endpoint: endpoint, P256dh: "p", Auth: "a",
		SessionID: "sid-handler-busy", SessionValidUntil: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM push_subscription WHERE endpoint = $1`, endpoint)
	})
	holder, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(context.Background(), `SELECT 1 FROM push_subscription WHERE endpoint = $1 FOR NO KEY UPDATE`, endpoint); err != nil {
		t.Fatalf("lock device: %v", err)
	}

	rec := &pushRecordingStarter{pool: testPool}
	savedStarter := testHandler.TxStarter
	testHandler.TxStarter = rec
	t.Cleanup(func() { testHandler.TxStarter = savedStarter })

	req := newRequest(http.MethodPost, "/api/push/subscriptions", body)
	req.Header.Set("Authorization", "Bearer "+pushSessionToken(t, "sid-handler-busy"))
	w := httptest.NewRecorder()
	testHandler.UpsertPushSubscription(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy device: status %d, want 503", w.Code)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.remaining) == 0 {
		t.Fatal("no rollback was observed")
	}
	for _, left := range rec.remaining {
		if left < 0 || left > 10*time.Second {
			t.Errorf("a rollback ran with %v left; teardown needs its own short deadline, outside the request budget", left)
		}
	}
}

// drainFaultStarter lets the logout drain fail at each step it has: Begin, the
// SET LOCAL, the exclusive lock, the commit.
type drainFaultStarter struct {
	pool                                      pushTxPool
	failBegin, failSet, failDrain, failCommit error
}

type drainFaultTx struct {
	pgx.Tx
	owner *drainFaultStarter
}

func (s *drainFaultStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	if s.failBegin != nil {
		return nil, s.failBegin
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return drainFaultTx{Tx: tx, owner: s}, nil
}

func (t drainFaultTx) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	switch {
	case t.owner.failSet != nil && strings.Contains(sql, "lock_timeout"):
		return pgconn.CommandTag{}, t.owner.failSet
	case t.owner.failDrain != nil && strings.Contains(sql, "pg_advisory_xact_lock("):
		return pgconn.CommandTag{}, t.owner.failDrain
	}
	return t.Tx.Exec(ctx, sql, args...)
}

func (t drainFaultTx) Commit(ctx context.Context) error {
	if t.owner.failCommit != nil {
		_ = t.Tx.Rollback(ctx)
		return t.owner.failCommit
	}
	return t.Tx.Commit(ctx)
}

// CHE-1795: the drain is what shows
// that no admitted send is left, so any failure of it, whatever the step, is a
// failed logout: 503 with Retry-After, the cookie kept, the marker kept.
func TestLogoutFailsClosedOnEveryDrainFailure(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	pushHandlerEnv(t)
	lockTimeout := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	cases := []struct {
		name  string
		fault drainFaultStarter
	}{
		{"begin", drainFaultStarter{failBegin: errors.New("pool is exhausted")}},
		{"set local", drainFaultStarter{failSet: errors.New("cannot set lock_timeout")}},
		{"drain error", drainFaultStarter{failDrain: errors.New("lock manager unavailable")}},
		{"drain lock timeout", drainFaultStarter{failDrain: lockTimeout}},
		{"drain deadline", drainFaultStarter{failDrain: context.DeadlineExceeded}},
		{"drain cancelled", drainFaultStarter{failDrain: context.Canceled}},
		{"commit", drainFaultStarter{failCommit: errors.New("commit failed")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sid := "sid-drain-" + strings.ReplaceAll(tc.name, " ", "-")
			t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM push_session_end WHERE session_id = $1`, sid) })
			starter := tc.fault
			starter.pool = testPool
			saved := testHandler.TxStarter
			testHandler.TxStarter = &starter
			t.Cleanup(func() { testHandler.TxStarter = saved })

			req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
			req.Header.Set("Authorization", "Bearer "+pushSessionToken(t, sid))
			rec := httptest.NewRecorder()
			testHandler.Logout(rec, req)

			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
				t.Errorf("logout with a failing drain: status %d, Retry-After %q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
			}
			for _, cookie := range rec.Result().Cookies() {
				if cookie.Name == auth.AuthCookieName {
					t.Error("the auth cookie was cleared after a failed drain: the client could no longer retry")
				}
			}
			var n int
			if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM push_session_end WHERE session_id = $1`, sid).Scan(&n); err != nil || n != 1 {
				t.Errorf("marker rows = %d (err %v), want the marker kept and durable", n, err)
			}
		})
	}

	t.Run("no fault", func(t *testing.T) {
		sid := "sid-drain-ok"
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM push_session_end WHERE session_id = $1`, sid) })
		saved := testHandler.TxStarter
		testHandler.TxStarter = &drainFaultStarter{pool: testPool}
		t.Cleanup(func() { testHandler.TxStarter = saved })

		req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		req.Header.Set("Authorization", "Bearer "+pushSessionToken(t, sid))
		rec := httptest.NewRecorder()
		testHandler.Logout(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("logout with a healthy drain: status %d, want 200", rec.Code)
		}
	})
}

// With push unconfigured the session end is still required. A store that cannot
// record it (a missing table, a database that is down) is a failed logout, not a
// skipped step.
func TestLogoutFailsLoudlyWhenPushIsOffAndTheSessionEndCannotBeRecorded(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Setenv("MULTICA_VAPID_PUBLIC_KEY", "")
	t.Setenv("MULTICA_VAPID_PRIVATE_KEY", "")
	t.Setenv("MULTICA_VAPID_SUBJECT", "")
	saved := testHandler.Queries
	testHandler.Queries = db.New(pushFailingDB{})
	t.Cleanup(func() { testHandler.Queries = saved })

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+pushSessionToken(t, "sid-off-store-down"))
	rec := httptest.NewRecorder()
	testHandler.Logout(rec, req)

	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("logout with push off and the marker store down: status %d, Retry-After %q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == auth.AuthCookieName {
			t.Fatal("the auth cookie was cleared although the session end was not recorded")
		}
	}
}
