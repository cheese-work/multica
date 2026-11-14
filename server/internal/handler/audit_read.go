package handler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/db/generated"
)

// Read side of the admin audit trail (CHE-5644). Owners and admins only, and
// only human credentials: an agent task token or cloud PAT is refused even
// when it carries an admin member, so an agent cannot read or lean on the trail.

const (
	auditReadDefaultLimit = 50
	auditReadMaxLimit     = 200
)

// AuditEventResponse is one audit row. Before and After hold the stored,
// already-redacted summaries (the "changed" marker, never a secret).
type AuditEventResponse struct {
	ID                string          `json:"id"`
	At                time.Time       `json:"at"`
	ActorType         string          `json:"actor_type"`
	ActorID           *string         `json:"actor_id"`
	ActorName         *string         `json:"actor_name"`
	TaskID            *string         `json:"task_id"`
	Kind              string          `json:"kind"`
	TargetType        string          `json:"target_type"`
	TargetID          *string         `json:"target_id"`
	TargetName        *string         `json:"target_name"`
	Before            json.RawMessage `json:"before"`
	After             json.RawMessage `json:"after"`
	ExternalRequested []string        `json:"external_requested"`
}

type AuditEventsPage struct {
	Events     []AuditEventResponse `json:"events"`
	NextCursor *string              `json:"next_cursor"`
}

// ListWorkspaceAuditEvents answers GET /api/workspaces/{id}/audit-events, newest
// first. Query: kind (prefix), target_id, actor_id, since (inclusive), until
// (exclusive), limit, cursor.
func (h *Handler) ListWorkspaceAuditEvents(w http.ResponseWriter, r *http.Request) {
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return
	}
	workspaceID := chi.URLParam(r, "id")
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	params, limit, ok := parseAuditReadQuery(w, r)
	if !ok {
		return
	}
	params.WorkspaceID = wsUUID
	params.RowLimit = int32(limit + 1)

	rows, err := h.Queries.ListWorkspaceAuditEvents(r.Context(), params)
	if err != nil {
		slog.Error("list audit events failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list audit events")
		return
	}
	page := AuditEventsPage{Events: []AuditEventResponse{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		c := encodeAuditCursor(last.At.Time, last.ID)
		page.NextCursor = &c
	}
	names, err := h.auditNames(r, wsUUID, rows)
	if err != nil {
		slog.Error("resolve audit names failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list audit events")
		return
	}
	for _, row := range rows {
		ev := AuditEventResponse{
			ID: uuidToString(row.ID), At: row.At.Time, ActorType: row.ActorType,
			ActorID: uuidToPtr(row.ActorID), TaskID: uuidToPtr(row.TaskID),
			Kind: row.Kind, TargetType: row.TargetType, TargetID: uuidToPtr(row.TargetID),
			Before: row.Before, After: row.After, ExternalRequested: row.ExternalRequested,
		}
		if ev.ExternalRequested == nil {
			ev.ExternalRequested = []string{}
		}
		if row.ActorID.Valid {
			actorKind := "agent"
			if row.ActorType == "member" {
				actorKind = "user"
			}
			ev.ActorName = names.get(actorKind, row.ActorID)
		}
		if row.TargetID.Valid {
			ev.TargetName = names.get(row.TargetType, row.TargetID)
		}
		page.Events = append(page.Events, ev)
	}
	writeJSON(w, http.StatusOK, page)
}

func parseAuditReadQuery(w http.ResponseWriter, r *http.Request) (db.ListWorkspaceAuditEventsParams, int, bool) {
	var p db.ListWorkspaceAuditEventsParams
	q := r.URL.Query()
	bad := func(msg string) (db.ListWorkspaceAuditEventsParams, int, bool) {
		writeError(w, http.StatusBadRequest, msg)
		return p, 0, false
	}

	limit := auditReadDefaultLimit
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return bad("invalid limit")
		}
		limit = min(n, auditReadMaxLimit)
	}
	if s := q.Get("kind"); s != "" {
		p.KindPrefix = pgtype.Text{String: s, Valid: true}
	}
	for _, f := range []struct {
		name string
		dst  *pgtype.UUID
	}{{"target_id", &p.TargetID}, {"actor_id", &p.ActorID}} {
		if s := q.Get(f.name); s != "" {
			u, err := util.ParseUUID(s)
			if err != nil {
				return bad("invalid " + f.name)
			}
			*f.dst = u
		}
	}
	for _, f := range []struct {
		name string
		dst  *pgtype.Timestamptz
	}{{"since", &p.Since}, {"until", &p.Until}} {
		if s := q.Get(f.name); s != "" {
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return bad("invalid " + f.name + ": want RFC 3339")
			}
			*f.dst = pgtype.Timestamptz{Time: t, Valid: true}
		}
	}
	if s := q.Get("cursor"); s != "" {
		at, id, err := decodeAuditCursor(s)
		if err != nil {
			return bad("invalid cursor")
		}
		p.CursorAt, p.CursorID = pgtype.Timestamptz{Time: at, Valid: true}, id
	}
	return p, limit, true
}

// The cursor is the last row's (at, id): Postgres keeps microseconds, so the
// unix-microsecond integer round-trips exactly.
func encodeAuditCursor(at time.Time, id pgtype.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d|%s", at.UnixMicro(), uuidToString(id))))
}

func decodeAuditCursor(s string) (time.Time, pgtype.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, pgtype.UUID{}, err
	}
	micro, idStr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, pgtype.UUID{}, fmt.Errorf("malformed cursor")
	}
	us, err := strconv.ParseInt(micro, 10, 64)
	if err != nil {
		return time.Time{}, pgtype.UUID{}, err
	}
	id, err := util.ParseUUID(idStr)
	if err != nil {
		return time.Time{}, pgtype.UUID{}, err
	}
	return time.UnixMicro(us), id, nil
}

// auditNameSet maps (kind, id) to a display name.
type auditNameSet map[string]string

func (n auditNameSet) get(kind string, id pgtype.UUID) *string {
	if name, ok := n[kind+":"+uuidToString(id)]; ok {
		return &name
	}
	return nil
}

// auditNames resolves every actor and target name of the page in one query. A
// deleted target or actor has no entry, so its name stays null.
func (h *Handler) auditNames(r *http.Request, workspaceID pgtype.UUID, rows []db.ListWorkspaceAuditEventsRow) (auditNameSet, error) {
	names := auditNameSet{}
	var userIDs, ids []pgtype.UUID
	for _, row := range rows {
		if row.ActorID.Valid {
			if row.ActorType == "member" {
				userIDs = append(userIDs, row.ActorID)
			} else {
				ids = append(ids, row.ActorID)
			}
		}
		if row.TargetID.Valid {
			ids = append(ids, row.TargetID)
		}
	}
	if len(userIDs)+len(ids) == 0 {
		return names, nil
	}
	found, err := h.Queries.ResolveAuditNames(r.Context(), db.ResolveAuditNamesParams{
		UserIds: userIDs, WorkspaceID: workspaceID, Ids: ids,
	})
	if err != nil {
		return nil, err
	}
	for _, f := range found {
		names[f.Kind+":"+uuidToString(f.ID)] = f.Name
	}
	return names, nil
}
