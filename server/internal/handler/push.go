package handler

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

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
	sub, err := h.Queries.UpsertPushSubscription(r.Context(), db.UpsertPushSubscriptionParams{
		ID: dbid.NewV7(), UserID: parseUUID(userID), Endpoint: req.Endpoint, P256dh: req.Keys.P256dh, Auth: req.Keys.Auth,
		SessionID: session.id, SessionValidUntil: pgtype.Timestamptz{Time: session.expires, Valid: true},
	})
	if err != nil {
		slog.Warn("UpsertPushSubscription failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to save subscription")
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
	if err := h.Queries.DeletePushSubscriptionByEndpoint(r.Context(), db.DeletePushSubscriptionByEndpointParams{Endpoint: req.Endpoint, UserID: parseUUID(userID)}); err != nil {
		slog.Warn("DeletePushSubscriptionByEndpoint failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to delete subscription")
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

// endPushSession stops pushes for the devices a logging-out session registered.
func (h *Handler) endPushSession(r *http.Request) {
	session, ok := sessionFromRequest(r)
	if !ok {
		return
	}
	if err := h.Queries.EndPushSession(r.Context(), db.EndPushSessionParams{UserID: parseUUID(session.userID), SessionID: session.id}); err != nil {
		slog.Warn("EndPushSession failed", append(logger.RequestAttrs(r), "error", err)...)
	}
}
