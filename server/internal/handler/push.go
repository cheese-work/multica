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
// committed: from then on no delivery passes its check. Then a bounded wait for
// the deliveries that took their shared session lock before the marker was
// visible; once that wait is over, none of them is left to start. A send that is
// already on the wire cannot be recalled, and a send that outlasts the drain is
// left to finish: it was admitted before the end, and the marker is recorded.
// The marker is the part that must not fail, so its error is returned and logout
// reports it (the cookie stays, so the client can retry). Nothing is recorded
// when push is not configured: there are no subscriptions to end.
func (h *Handler) endPushSession(r *http.Request) error {
	if !pushnotify.ConfigFromEnv().Enabled() {
		return nil
	}
	session, ok := sessionFromRequest(r)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), pushRequestBudget)
	defer cancel()
	if err := h.Queries.EndPushSession(ctx, session.id); err != nil {
		return err
	}
	h.drainPushSession(r, session.id)
	return nil
}

func (h *Handler) drainPushSession(r *http.Request, sessionID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), pushRequestBudget)
	defer cancel()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		slog.Warn("push session drain failed", append(logger.RequestAttrs(r), "error", err)...)
		return
	}
	defer pushnotify.RollbackBounded(ctx, tx)
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", pushLogoutDrain.Milliseconds())); err == nil {
		err = h.Queries.WithTx(tx).DrainPushSession(ctx, sessionID)
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			slog.Info("push session drain gave up; the marker is recorded", append(logger.RequestAttrs(r), "error", err)...)
		}
	}
}
