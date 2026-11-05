package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Workspace background agents (CHE-4098): settings.background_agents maps a
// job (dm_title, quick_actions, compact, routing) to a two-slot agent chain.
// Only a human owner/admin changes it, on every path. The dedicated route
// writes it; a generic settings save keeps the stored value, and a machine
// actor whose save would change it gets 403. An unchanged machine write is
// accepted: a client that echoes the whole settings blob must not break.
const (
	backgroundAgentsKey      = "background_agents"
	backgroundAgentsActivity = "background_agent_changed"

	codeBackgroundAgentsHumanOnly = "background_agents_human_only"
	codeAgentNotEligible          = "agent_not_eligible"
)

var errBackgroundAgentsHumanOnly = errors.New("background agents can only be changed by a human owner or admin")

// bgChange is one job whose chain effectively changed.
type bgChange struct {
	Job      string
	From, To any
}

// bgValue treats an absent key and an empty object alike: nothing is set.
func bgValue(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// reconcileBackgroundAgents makes a generic settings save keep the stored
// value. A machine actor whose incoming value differs from it is refused.
func reconcileBackgroundAgents(stored, incoming map[string]any, machine bool) error {
	if machine && !bgSame(stored[backgroundAgentsKey], incoming[backgroundAgentsKey]) {
		return errBackgroundAgentsHumanOnly
	}
	delete(incoming, backgroundAgentsKey)
	if v, ok := stored[backgroundAgentsKey]; ok {
		incoming[backgroundAgentsKey] = v
	}
	return nil
}

func bgSame(a, b any) bool {
	am, bm := bgValue(a), bgValue(b)
	return (len(am) == 0 && len(bm) == 0) || reflect.DeepEqual(am, bm)
}

func writeBackgroundAgentsRefused(w http.ResponseWriter) {
	writeErrorCode(w, http.StatusForbidden, codeBackgroundAgentsHumanOnly, errBackgroundAgentsHumanOnly.Error())
}

func (h *Handler) GetBackgroundAgents(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireWorkspaceMember(w, r, workspaceIDFromURL(r, "id"), "workspace not found")
	if !ok {
		return
	}
	row, err := h.Queries.GetWorkspace(r.Context(), member.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load background agents")
		return
	}
	var settings map[string]any
	_ = json.Unmarshal(row.Settings, &settings)
	out := bgValue(settings[backgroundAgentsKey])
	if out == nil {
		out = map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
}

// UpdateBackgroundAgents replaces the whole value with the body.
func (h *Handler) UpdateBackgroundAgents(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireWorkspaceRole(w, r, workspaceIDFromURL(r, "id"), "workspace not found", "owner", "admin")
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	next, bad := h.canonicalBackgroundAgents(r.Context(), member.WorkspaceID, body)
	if bad != nil {
		writeErrorCode(w, http.StatusBadRequest, bad.Code, bad.Msg)
		return
	}
	h.saveBackgroundAgents(w, r, member, next)
}

// ResetBackgroundAgents clears every job.
func (h *Handler) ResetBackgroundAgents(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireWorkspaceRole(w, r, workspaceIDFromURL(r, "id"), "workspace not found", "owner", "admin")
	if !ok {
		return
	}
	h.saveBackgroundAgents(w, r, member, map[string]any{})
}

func (h *Handler) saveBackgroundAgents(w http.ResponseWriter, r *http.Request, member db.Member, next map[string]any) {
	id := uuidToString(member.WorkspaceID)
	actorType, _ := h.resolveActor(r, requestUserID(r), id)
	changes, cancelled, err := h.writeBackgroundAgents(r.Context(), member, isGovernedFieldMachineActor(r, actorType), next)
	if errors.Is(err, errBackgroundAgentsHumanOnly) {
		writeBackgroundAgentsRefused(w)
		return
	}
	if err != nil {
		slog.Warn("update background agents failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", id)...)
		writeError(w, http.StatusInternalServerError, "failed to update background agents")
		return
	}
	h.TaskService.BroadcastCancelledTasks(r.Context(), id, cancelled)
	for _, c := range changes {
		h.publish(protocol.EventWorkspaceBackgroundAgentChanged, id, "member", requestUserID(r), map[string]any{
			"job": c.Job, "scope": string(bgagents.WorkspaceScope), "from": c.From, "to": c.To,
		})
	}
	writeJSON(w, http.StatusOK, next)
}

// writeBackgroundAgents writes the value and one activity row per changed job
// in one transaction under the workspace row lock. The machine check runs
// against the locked stored value, so a refused write stores nothing.
func (h *Handler) writeBackgroundAgents(ctx context.Context, member db.Member, machine bool, next map[string]any) ([]bgChange, []db.AgentTaskQueue, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	storedJSON, err := q.LockWorkspaceSettings(ctx, member.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	var stored map[string]any
	_ = json.Unmarshal(storedJSON, &stored) // stored settings that are not an object start over as one
	if stored == nil {
		stored = map[string]any{}
	}
	prev := bgValue(stored[backgroundAgentsKey])
	var changes []bgChange
	for _, job := range sortedUnionKeys(prev, next) {
		if !reflect.DeepEqual(prev[job], next[job]) {
			changes = append(changes, bgChange{job, prev[job], next[job]})
		}
	}
	if len(changes) == 0 {
		return nil, nil, nil
	}
	if machine {
		return nil, nil, errBackgroundAgentsHumanOnly
	}
	if len(next) == 0 {
		delete(stored, backgroundAgentsKey)
	} else {
		stored[backgroundAgentsKey] = next
	}
	settings, err := json.Marshal(stored)
	if err != nil {
		return nil, nil, err
	}
	if _, err := q.UpdateWorkspace(ctx, db.UpdateWorkspaceParams{ID: member.WorkspaceID, Settings: settings}); err != nil {
		return nil, nil, err
	}
	for _, c := range changes {
		details, err := json.Marshal(map[string]any{"job": c.Job, "scope": bgagents.WorkspaceScope, "from": c.From, "to": c.To})
		if err != nil {
			return nil, nil, err
		}
		if _, err := q.CreateActivity(ctx, db.CreateActivityParams{
			ID: dbid.NewV7(), WorkspaceID: member.WorkspaceID, ActorType: strToText("member"), ActorID: member.UserID,
			Action: backgroundAgentsActivity, Details: details,
		}); err != nil {
			return nil, nil, err
		}
	}
	// A changed job drops the queued work its old workspace chain produced; a
	// user-sourced task keeps running. Same transaction, so a failed cancel
	// rolls the setting back too.
	var cancelled []db.AgentTaskQueue
	for _, c := range changes {
		rows, err := service.CancelBackgroundTasksForJob(ctx, q, member.WorkspaceID, c.Job)
		if err != nil {
			return nil, nil, err
		}
		cancelled = append(cancelled, rows...)
	}
	return changes, cancelled, tx.Commit(ctx)
}

func sortedUnionKeys(a, b map[string]any) []string {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, dup := a[k]; !dup {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// canonicalBackgroundAgents validates body for workspace scope and returns the
// value as it is stored: lower-case agent ids, empty fields dropped, JSON-typed
// so it compares equal to what a read gives back. Every agent must be ENG-8
// eligible in this workspace.
func (h *Handler) canonicalBackgroundAgents(ctx context.Context, ws pgtype.UUID, body []byte) (map[string]any, *bgagents.Error) {
	cfg, err := bgagents.Parse(body, bgagents.WorkspaceScope)
	if err != nil {
		var bad *bgagents.Error
		if !errors.As(err, &bad) {
			bad = &bgagents.Error{Code: bgagents.CodeInvalidShape, Msg: err.Error()}
		}
		return nil, bad
	}
	canon := func(field, raw string) (string, *bgagents.Error) {
		id, err := util.ParseUUID(raw)
		if err != nil {
			return "", &bgagents.Error{Code: bgagents.CodeInvalidAgentID, Msg: field + " must be an agent id"}
		}
		agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: ws})
		if err != nil || !bgagents.WorkspaceEligible(agent) {
			return "", &bgagents.Error{Code: codeAgentNotEligible, Msg: field + " must be a workspace-visible agent with a runtime in this workspace: " + raw}
		}
		return uuidToString(id), nil
	}
	out := map[string]any{}
	for job, c := range cfg {
		chain := map[string]any{}
		for field, raw := range map[string]string{"agent_id": c.AgentID, "fallback_agent_id": c.FallbackAgentID} {
			if raw == "" {
				continue
			}
			id, bad := canon(string(job)+"."+field, raw)
			if bad != nil {
				return nil, bad
			}
			chain[field] = id
		}
		if len(c.Statuses) > 0 {
			chain["statuses"] = c.Statuses
		}
		if len(c.Overrides) > 0 {
			overrides := make(map[string]string, len(c.Overrides))
			for key, raw := range c.Overrides {
				id, bad := canon(string(job)+".overrides."+key, raw)
				if bad != nil {
					return nil, bad
				}
				overrides[strings.TrimSpace(key)] = id
			}
			chain["overrides"] = overrides
		}
		out[string(job)] = chain
	}
	// Round-trip so slices and maps compare equal to what a read gives back.
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, &bgagents.Error{Code: bgagents.CodeInvalidShape, Msg: err.Error()}
	}
	var typed map[string]any
	_ = json.Unmarshal(raw, &typed)
	return typed, nil
}
