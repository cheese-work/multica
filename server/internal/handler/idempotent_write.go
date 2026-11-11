package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

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
		ctx := r.Context()

		_, err = h.Queries.ClaimWriteEvent(ctx, db.ClaimWriteEventParams{WorkspaceID: workspaceID, EventKey: key})
		if errors.Is(err, pgx.ErrNoRows) {
			if replayed := h.replayWriteEvent(w, r, workspaceID, key); replayed {
				return
			}
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to record event")
			return
		}

		rec := &recordingWriter{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status >= 200 && rec.status < 300 {
			_ = h.Queries.CompleteWriteEvent(ctx, db.CompleteWriteEventParams{
				WorkspaceID: workspaceID, EventKey: key, StatusCode: pgtype.Int4{Int32: int32(rec.status), Valid: true}, ResponseBody: rec.body.Bytes(),
			})
			return
		}
		_ = h.Queries.ReleaseWriteEvent(ctx, db.ReleaseWriteEventParams{WorkspaceID: workspaceID, EventKey: key})
	})
}

// replayWriteEvent answers a redelivery. It returns false when the earlier claim
// went stale and this request now owns the write.
func (h *Handler) replayWriteEvent(w http.ResponseWriter, r *http.Request, workspaceID pgtype.UUID, key string) bool {
	ctx := r.Context()
	ev, err := h.Queries.GetWriteEvent(ctx, db.GetWriteEventParams{WorkspaceID: workspaceID, EventKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		// Released between the claim and the read: the write failed, so go on.
		return false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read event")
		return true
	}
	if ev.State == "done" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(IdempotentReplayHeader, "true")
		w.WriteHeader(int(ev.StatusCode.Int32))
		_, _ = w.Write(ev.ResponseBody)
		return true
	}
	_, err = h.Queries.RetakeStaleWriteEvent(ctx, db.RetakeStaleWriteEventParams{WorkspaceID: workspaceID, EventKey: key, StaleSecs: writeEventStale.Seconds()})
	if err == nil {
		return false
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "an earlier write with this event id is still in progress")
		return true
	}
	writeError(w, http.StatusInternalServerError, "failed to read event")
	return true
}
