package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// EventIDHeader carries the stable id of the event behind a write.
	EventIDHeader = "X-Event-Id"
	// IdempotentReplayHeader marks a response replayed from an earlier write.
	IdempotentReplayHeader = "Idempotent-Replay"
	maxEventIDLen          = 200
	// writeEventStale is how long an unfinished claim blocks a redelivery.
	writeEventStale = 2 * time.Minute
)

// writeEventKey scopes an event id to the method, path and user it was sent
// with; the workspace is the row's own column.
func writeEventKey(r *http.Request, id string) string {
	sum := sha256.Sum256([]byte(r.Method + "|" + r.URL.Path + "|" + requestUserID(r) + "|" + id))
	return hex.EncodeToString(sum[:])
}

type recordingWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *recordingWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

// IdempotentWrite makes a write safe to redeliver: a request with an X-Event-Id
// writes once per (workspace, user, method, path, id). The first success is
// stored; a redelivery gets that response back. A failed write leaves no trace.
// A request without the header is untouched.
func (h *Handler) IdempotentWrite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(EventIDHeader))
		if id == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(id) > maxEventIDLen {
			writeError(w, http.StatusBadRequest, "event id too long")
			return
		}
		workspaceID, err := util.ParseUUID(h.resolveWorkspaceID(r))
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		key := writeEventKey(r, id)
		r = r.WithContext(service.WithEventID(r.Context(), id))
		owner := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		if !h.claimWriteEvent(w, r, workspaceID, key, owner) {
			return
		}
		r = r.WithContext(service.WithWriteEventClaim(r.Context(), workspaceID, key, owner))
		ctx := r.Context()

		rec := &recordingWriter{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status >= 200 && rec.status < 300 {
			// The write is committed. A completion that fails here leaves the
			// claim 'applied', which a redelivery answers without writing.
			done := db.CompleteWriteEventParams{
				WorkspaceID: workspaceID, EventKey: key, Owner: owner,
				StatusCode: pgtype.Int4{Int32: int32(rec.status), Valid: true}, ResponseBody: rec.body.Bytes(),
			}
			for attempt := 0; attempt < 3; attempt++ {
				_, err := h.Queries.CompleteWriteEvent(context.WithoutCancel(ctx), done)
				if err == nil {
					break
				}
				slog.Warn("write event completion failed", "error", err, "attempt", attempt+1)
			}
			return
		}
		_ = h.Queries.ReleaseWriteEvent(context.WithoutCancel(ctx), db.ReleaseWriteEventParams{WorkspaceID: workspaceID, EventKey: key, Owner: owner})
	})
}

// claimWriteEvent makes this request the owner of the event id's claim, or
// answers the request and returns false. A request runs its write only as the
// owner: a replay, an unfinished claim held by another request and a lost race
// all answer here.
func (h *Handler) claimWriteEvent(w http.ResponseWriter, r *http.Request, workspaceID pgtype.UUID, key string, owner pgtype.UUID) bool {
	ctx := r.Context()
	// A claim released between our insert and our read frees the id, so go round
	// again rather than write without owning it.
	for attempt := 0; attempt < 3; attempt++ {
		_, err := h.Queries.ClaimWriteEvent(ctx, db.ClaimWriteEventParams{WorkspaceID: workspaceID, EventKey: key, Owner: owner})
		if err == nil {
			return true
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "failed to record event")
			return false
		}
		ev, err := h.Queries.GetWriteEvent(ctx, db.GetWriteEventParams{WorkspaceID: workspaceID, EventKey: key})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read event")
			return false
		}
		switch ev.State {
		case "done":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(IdempotentReplayHeader, "true")
			w.WriteHeader(int(ev.StatusCode.Int32))
			_, _ = w.Write(ev.ResponseBody)
			return false
		case "applied":
			// Committed, but its response was never stored.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(IdempotentReplayHeader, "true")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"applied":true,"idempotent_replay":true}`))
			return false
		}
		_, err = h.Queries.RetakeStaleWriteEvent(ctx, db.RetakeStaleWriteEventParams{WorkspaceID: workspaceID, EventKey: key, Owner: owner, StaleSecs: writeEventStale.Seconds()})
		if err == nil {
			return true
		}
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, "an earlier write with this event id is still in progress")
			return false
		}
		writeError(w, http.StatusInternalServerError, "failed to read event")
		return false
	}
	writeError(w, http.StatusConflict, "the event id is contended; retry")
	return false
}
