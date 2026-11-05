package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"reflect"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Personal background-agents writes (CHE-4113). A human member changes only
// their own member_setting row. A machine actor (task token, cloud PAT) gets
// 403 when the write would change the value; an unchanged machine write is
// accepted, as on the workspace route. Every effective write cancels, in the
// same transaction, the member's non-terminal tasks of each changed job, from
// either chain source (CHE-4114).

// UpdateMyBackgroundAgents merges the body into the caller's value, per job and
// per field: {"dm_title": {"agent_id": "<id>", "fallback_agent_id": null}} sets
// the primary and clears the fallback.
func (h *Handler) UpdateMyBackgroundAgents(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireWorkspaceMember(w, r, workspaceIDFromURL(r, "id"), "workspace not found")
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	patch, bad := h.parseMemberPatch(r.Context(), member, body)
	if bad != nil {
		writeErrorCode(w, http.StatusBadRequest, bad.Code, bad.Msg)
		return
	}
	h.saveMemberBackgroundAgents(w, r, member, func(prev map[string]any) map[string]any {
		next := maps.Clone(prev)
		for job, fields := range patch {
			chain := maps.Clone(bgValue(prev[job]))
			if chain == nil {
				chain = map[string]any{}
			}
			for field, id := range fields {
				if id == "" {
					delete(chain, field)
				} else {
					chain[field] = id
				}
			}
			if len(chain) == 0 {
				delete(next, job)
			} else {
				next[job] = chain
			}
		}
		return next
	})
}

// ResetMyBackgroundAgent drops one job from the caller's value.
func (h *Handler) ResetMyBackgroundAgent(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireWorkspaceMember(w, r, workspaceIDFromURL(r, "id"), "workspace not found")
	if !ok {
		return
	}
	job := chi.URLParam(r, "job")
	if bad := userScopedJob(bgagents.Job(job)); bad != nil {
		writeErrorCode(w, http.StatusBadRequest, bad.Code, bad.Msg)
		return
	}
	h.saveMemberBackgroundAgents(w, r, member, func(prev map[string]any) map[string]any {
		next := maps.Clone(prev)
		delete(next, job)
		return next
	})
}

// memberPatch maps job -> field -> agent id; an empty id clears the field.
type memberPatch map[string]map[string]string

// parseMemberPatch validates the body and every agent it sets: live in this
// workspace, on a runtime, and invocable by the caller.
func (h *Handler) parseMemberPatch(ctx context.Context, member db.Member, body []byte) (memberPatch, *bgagents.Error) {
	var raw map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return nil, &bgagents.Error{Code: bgagents.CodeInvalidShape, Msg: "body must map each job to an object of fields"}
	}
	patch := memberPatch{}
	for job, fields := range raw {
		if bad := userScopedJob(bgagents.Job(job)); bad != nil {
			return nil, bad
		}
		if fields == nil {
			return nil, &bgagents.Error{Code: bgagents.CodeInvalidShape, Msg: "job " + job + " must be an object"}
		}
		patch[job] = map[string]string{}
		for field, value := range fields {
			if field != "agent_id" && field != "fallback_agent_id" {
				return nil, &bgagents.Error{Code: bgagents.CodeUnknownField, Msg: "job " + job + " has no field " + field}
			}
			var id *string
			if err := json.Unmarshal(value, &id); err != nil {
				return nil, &bgagents.Error{Code: bgagents.CodeInvalidShape, Msg: job + "." + field + " must be an agent id or null"}
			}
			if id == nil {
				patch[job][field] = ""
				continue
			}
			canon, bad := h.canonicalMemberAgent(ctx, member, job+"."+field, *id)
			if bad != nil {
				return nil, bad
			}
			patch[job][field] = canon
		}
	}
	return patch, nil
}

func (h *Handler) canonicalMemberAgent(ctx context.Context, member db.Member, field, raw string) (string, *bgagents.Error) {
	id, err := util.ParseUUID(raw)
	if err != nil {
		return "", &bgagents.Error{Code: bgagents.CodeInvalidAgentID, Msg: field + " must be an agent id"}
	}
	agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: member.WorkspaceID})
	if err != nil || agent.ArchivedAt.Valid || !agent.RuntimeID.Valid ||
		!h.canInvokeAgent(ctx, agent, "member", uuidToString(member.UserID), "", uuidToString(member.WorkspaceID)) {
		return "", &bgagents.Error{Code: codeAgentNotEligible, Msg: field + " must be an agent you can invoke, with a runtime in this workspace: " + raw}
	}
	return uuidToString(id), nil
}

func (h *Handler) saveMemberBackgroundAgents(w http.ResponseWriter, r *http.Request, member db.Member, apply func(map[string]any) map[string]any) {
	id := uuidToString(member.WorkspaceID)
	actorType, _ := h.resolveActor(r, requestUserID(r), id)
	next, cancelled, err := h.writeMemberBackgroundAgents(r.Context(), member, isGovernedFieldMachineActor(r, actorType), apply)
	switch {
	case errors.Is(err, errBackgroundAgentsHumanOnly):
		writeBackgroundAgentsRefused(w)
	case err != nil:
		slog.Warn("update personal background agents failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", id)...)
		writeError(w, http.StatusInternalServerError, "failed to update background agents")
	default:
		h.TaskService.BroadcastCancelledTasks(r.Context(), id, cancelled)
		writeJSON(w, http.StatusOK, next)
	}
}

// writeMemberBackgroundAgents applies the change and writes one activity row
// per changed job in one transaction, under the lock that enqueue also takes.
// The same transaction cancels the member's tasks of each changed job; the
// caller broadcasts the returned rows after commit. The machine check runs on
// the locked stored value, so a refused write stores nothing and an unchanged
// write passes.
func (h *Handler) writeMemberBackgroundAgents(ctx context.Context, member db.Member, machine bool, apply func(map[string]any) map[string]any) (map[string]any, []db.AgentTaskQueue, error) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	if err := service.LockMemberBackgroundAgents(ctx, tx, member.WorkspaceID, member.UserID); err != nil {
		return nil, nil, err
	}
	q := h.Queries.WithTx(tx)
	storedJSON, err := h.memberBackgroundAgentsWith(ctx, q, member.WorkspaceID, member.UserID)
	if err != nil {
		return nil, nil, err
	}
	var prev map[string]any
	_ = json.Unmarshal(storedJSON, &prev) // an unset or non-object value starts over as empty
	if prev == nil {
		prev = map[string]any{}
	}
	next := apply(prev)
	var changes []bgChange
	for _, job := range sortedUnionKeys(prev, next) {
		if !reflect.DeepEqual(prev[job], next[job]) {
			changes = append(changes, bgChange{job, prev[job], next[job]})
		}
	}
	if len(changes) == 0 {
		return prev, nil, nil
	}
	if machine {
		return nil, nil, errBackgroundAgentsHumanOnly
	}
	if len(next) == 0 {
		err = q.DeleteMemberSetting(ctx, db.DeleteMemberSettingParams{WorkspaceID: member.WorkspaceID, UserID: member.UserID, Key: memberBackgroundAgentsKey})
	} else {
		var value []byte
		if value, err = json.Marshal(next); err == nil {
			err = q.UpsertMemberSetting(ctx, db.UpsertMemberSettingParams{WorkspaceID: member.WorkspaceID, UserID: member.UserID, Key: memberBackgroundAgentsKey, Value: value})
		}
	}
	if err != nil {
		return nil, nil, err
	}
	var cancelled []db.AgentTaskQueue
	for _, c := range changes {
		details, err := json.Marshal(map[string]any{"job": c.Job, "scope": bgagents.UserScope, "from": c.From, "to": c.To})
		if err != nil {
			return nil, nil, err
		}
		if _, err := q.CreateActivity(ctx, db.CreateActivityParams{
			ID: dbid.NewV7(), WorkspaceID: member.WorkspaceID, ActorType: strToText("member"), ActorID: member.UserID,
			Action: backgroundAgentsActivity, Details: details,
		}); err != nil {
			return nil, nil, err
		}
		rows, err := service.CancelBackgroundTasksForMember(ctx, q, member.WorkspaceID, member.UserID, c.Job)
		if err != nil {
			return nil, nil, err
		}
		cancelled = append(cancelled, rows...)
	}
	return next, cancelled, tx.Commit(ctx)
}
