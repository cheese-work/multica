package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/pushnotify"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Web Push registration (CHE-1795). A subscription belongs to a user and a
// device across workspaces, and remembers the login session that registered it.

const pushRequestMaxBytes = 8 << 10

// Bounds on registration and delete (CHE-1795). The worker holds a device for at
// most one delivery attempt (pushnotify deliveryTimeout), and a healthy push
// service answers well within pushDeviceLockWait, so normally nothing waits.
// pushRequestBudget caps the whole request, pool acquisition, lock waits and
// commit included, which is what holds when several requests queue on the same
// device (each lock wait alone restarts the timer). Past either bound the
// request fails with 503 and Retry-After and the client retries; it does not
// queue behind the send. This replaces the earlier "at most the send timeout"
// claim, which only covered the push call itself. Ending a session (logout) does
// not use the device row, so it has no such wait.
const (
	pushDeviceLockWait = 3 * time.Second
	pushRequestBudget  = 5 * time.Second
)

// withDeviceLock runs fn in a transaction whose lock waits are bounded. A wait
// that runs out is reported to the client as a retryable 503.
func (h *Handler) withDeviceLock(w http.ResponseWriter, r *http.Request, what string, fn func(context.Context, *db.Queries) error) bool {
	ctx, cancel := context.WithTimeout(r.Context(), pushRequestBudget)
	defer cancel()
	tx, err := h.TxStarter.Begin(ctx)
	if err == nil {
		defer pushnotify.RollbackBounded(ctx, tx)
		_, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", pushDeviceLockWait.Milliseconds()))
	}
	if err == nil {
		err = fn(ctx, h.Queries.WithTx(tx))
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err == nil {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &pgErr) && pgErr.Code == "55P03") {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "a push is being sent to this device; try again shortly")
		return false
	}
	slog.Warn(what+" failed", append(logger.RequestAttrs(r), "error", err)...)
	writeError(w, http.StatusInternalServerError, "failed to "+what)
	return false
}

func (h *Handler) GetPushConfig(w http.ResponseWriter, r *http.Request) {
	cfg := pushnotify.ConfigFromEnv()
	publicKey := ""
	if cfg.Enabled() {
		publicKey = cfg.PublicKey
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": cfg.Enabled(), "public_key": publicKey})
}

type pushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// pushKeyOK checks a browser key: unpadded base64url of the expected length.
func pushKeyOK(value string, length int) bool {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
	return err == nil && len(raw) == length
}

// UpsertPushSubscription registers (or takes over) the caller's device.
func (h *Handler) UpsertPushSubscription(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if !pushnotify.ConfigFromEnv().Enabled() {
		writeError(w, http.StatusServiceUnavailable, "push notifications are not configured")
		return
	}
	// The session id ties the device to this login, so it must be a browser session.
	// ponytail: session_valid_until is the token's exp at registration. A sliding
	// renewal moves exp, so the client re-POSTs on app load (N4) to extend it; a
	// device that never reopens the app stops receiving pushes when exp passes.
	session, ok := sessionFromRequest(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "push needs a signed-in browser session")
		return
	}
	// Sliding renewal keeps a session's id alive, so an ended session is refused
	// here and not only skipped by the worker.
	if ended, err := h.Queries.PushSessionEnded(r.Context(), session.id); err != nil {
		slog.Warn("PushSessionEnded failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to save subscription")
		return
	} else if ended {
		writeError(w, http.StatusUnauthorized, "this session has ended")
		return
	}
	var req pushSubscriptionRequest
	r.Body = http.MaxBytesReader(w, r.Body, pushRequestMaxBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := pushnotify.ValidEndpoint(req.Endpoint); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !pushKeyOK(req.Keys.P256dh, 65) || !pushKeyOK(req.Keys.Auth, 16) {
		writeError(w, http.StatusBadRequest, "invalid subscription keys")
		return
	}
	var sub db.PushSubscription
	if !h.withDeviceLock(w, r, "save subscription", func(ctx context.Context, q *db.Queries) (err error) {
		sub, err = q.UpsertPushSubscription(ctx, db.UpsertPushSubscriptionParams{
			ID: dbid.NewV7(), UserID: parseUUID(userID), Endpoint: req.Endpoint, P256dh: req.Keys.P256dh, Auth: req.Keys.Auth,
			SessionID: session.id, SessionValidUntil: pgtype.Timestamptz{Time: session.expires, Valid: true},
		})
		return err
	}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": uuidToString(sub.ID)})
}

// DeletePushSubscription removes the caller's own device. Deleting what is
// already gone succeeds, so logout can always call it.
func (h *Handler) DeletePushSubscription(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var req pushSubscriptionRequest
	r.Body = http.MaxBytesReader(w, r.Body, pushRequestMaxBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "endpoint is required")
		return
	}
	if !h.withDeviceLock(w, r, "delete subscription", func(ctx context.Context, q *db.Queries) error {
		return q.DeletePushSubscriptionByEndpoint(ctx, db.DeletePushSubscriptionByEndpointParams{Endpoint: req.Endpoint, UserID: parseUUID(userID)})
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type pushSession struct {
	userID  string
	id      string
	expires time.Time
}

// sessionFromRequest reads the login session out of the request's own token,
// the same token the auth middleware accepts (header first, then cookie).
func sessionFromRequest(r *http.Request) (pushSession, bool) {
	token := ""
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		token = strings.TrimPrefix(header, "Bearer ")
	} else if cookie, err := r.Cookie(auth.AuthCookieName); err == nil {
		token = cookie.Value
	}
	claims, err := auth.ParseSessionToken(token)
	if err != nil {
		return pushSession{}, false
	}
	sid, _ := claims["sid"].(string)
	sub, _ := claims["sub"].(string)
	expires := auth.SessionExpiry(claims)
	return pushSession{userID: sub, id: sid, expires: expires}, sid != "" && sub != "" && !expires.IsZero()
}

// pushLogoutDrain is how long logout waits for sends admitted before the
// session's end to finish. Healthy push services answer in well under this.
const pushLogoutDrain = 3 * time.Second

// endPushSession ends the session for push, in two steps. First a marker row,
// committed: from then on no delivery passes its check. Then the drain: an
// exclusive session lock, which waits for every delivery that took its shared
// session lock before the marker was visible. Once the drain has finished, no
// admitted delivery is left to start a send.
//
// Both steps must succeed for logout to succeed. A drain that cannot show that,
// for any reason (the pool, the SET, the lock wait running out, the deadline,
// the commit), is returned as an error and logout answers 503 with the cookie
// kept. The marker stays, and a retry repeats both steps without harm. A push
// already on the wire cannot be recalled, and nothing here promises that.
//
// The budgets are separate, not one: the marker has pushRequestBudget, the
// drain has its own pushRequestBudget with a pushLogoutDrain lock wait, and each
// rollback has pushnotify.TeardownBudget.
//
// This does not look at the VAPID configuration. Subscriptions registered while
// push was configured survive a change of keys, and the same keys may come back
// before the session's saved expiry, so an unconfigured or stopped push must
// not skip the end. A store that cannot record it (a missing table, a database
// that is down) is a failed logout, not a skipped step.
func (h *Handler) endPushSession(r *http.Request) error {
	session, ok := sessionFromRequest(r)
	if !ok {
		return nil
	}
	markerCtx, cancelMarker := context.WithTimeout(context.WithoutCancel(r.Context()), pushRequestBudget)
	defer cancelMarker()
	if err := h.Queries.EndPushSession(markerCtx, session.id); err != nil {
		return fmt.Errorf("record the session end: %w", err)
	}
	return h.drainPushSession(r, session.id)
}

func (h *Handler) drainPushSession(r *http.Request, sessionID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), pushRequestBudget)
	defer cancel()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the drain: %w", err)
	}
	defer pushnotify.RollbackBounded(ctx, tx)
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", pushLogoutDrain.Milliseconds())); err != nil {
		return fmt.Errorf("bound the drain: %w", err)
	}
	if err := h.Queries.WithTx(tx).DrainPushSession(ctx, sessionID); err != nil {
		return fmt.Errorf("drain admitted sends: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("finish the drain: %w", err)
	}
	return nil
}
