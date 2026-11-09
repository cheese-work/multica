package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-4383 B4a: the System 2 labeller writes labels through the API. The caller
// is an agent task token only; its agent and task come from the token (the
// auth middleware strips and re-stamps X-Agent-ID / X-Task-ID). A label is
// append-only: a correction is a new row that names the one it supersedes.

// system1LabelValues is the closed label set per scope. Only the test-first
// waiver scope exists; CI scopes arrive with B5/B6.
// ponytail: static map, move to a table when scopes are configured at runtime.
var system1LabelValues = map[string][]string{"test_first_waiver": {"waive", "refuse"}}

const system1NilUUID = "00000000-0000-0000-0000-000000000000"

type system1LabelRequest struct {
	ObservationID string `json:"observation_id"`
	Kind          string `json:"kind"`
	Label         string `json:"label"`
	Supersedes    string `json:"supersedes"`
}

func (h *Handler) WriteSystem1Label(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "labels are written by an agent task only")
		return
	}
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	task, terr := util.ParseUUID(r.Header.Get("X-Task-ID"))
	agent, aerr := util.ParseUUID(r.Header.Get("X-Agent-ID"))
	if terr != nil || aerr != nil {
		writeError(w, http.StatusForbidden, "labels are written by an agent task only")
		return
	}
	var req system1LabelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	obsID, err := util.ParseUUID(req.ObservationID)
	if err != nil || (req.Kind != "burn_in" && req.Kind != "eval") {
		writeError(w, http.StatusBadRequest, "observation_id and kind (burn_in or eval) are required")
		return
	}
	var supersedes pgtype.UUID
	if req.Supersedes != "" {
		if supersedes, err = util.ParseUUID(req.Supersedes); err != nil {
			writeError(w, http.StatusBadRequest, "invalid supersedes")
			return
		}
	}

	ctx := r.Context()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write label")
		return
	}
	defer tx.Rollback(ctx)

	// The caller must be a running task of a live agent in this workspace.
	var running int
	err = tx.QueryRow(ctx, `SELECT 1 FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id AND a.workspace_id=$3 AND a.archived_at IS NULL
 WHERE t.id=$1 AND t.agent_id=$2 AND t.status='running'`, task, agent, workspaceID).Scan(&running)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusForbidden, "labels are written by a running agent task only")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write label")
		return
	}

	// A foreign-workspace observation reads as missing.
	var scope, assigned string
	err = tx.QueryRow(ctx, `SELECT coalesce(o.inputs->>'topic',''), coalesce(w.settings->'system1_labellers'->>(o.inputs->>'topic'),'')
 FROM system1_observation o JOIN workspace w ON w.id=o.workspace_id WHERE o.id=$1 AND o.workspace_id=$2`, obsID, workspaceID).Scan(&scope, &assigned)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "observation not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write label")
		return
	}
	if assigned == "" || assigned != uuidToString(agent) {
		writeError(w, http.StatusForbidden, "agent is not the assigned labeller for this scope")
		return
	}
	if !slices.Contains(system1LabelValues[scope], req.Label) {
		writeError(w, http.StatusBadRequest, "label is not a value of this scope")
		return
	}

	// One writer per observation at a time: the live-row check below depends on it.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('system1_label:'||$1::text, 0))`, uuidToString(obsID)); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write label")
		return
	}

	// Idempotency key (observation, kind, task): the same body returns the same row.
	var id pgtype.UUID
	var existing string
	err = tx.QueryRow(ctx, `SELECT id, label FROM system1_label WHERE source='labeller' AND observation_id=$1 AND kind=$2 AND task_id=$3
 AND coalesce(supersedes,$5::uuid)=coalesce($4::uuid,$5::uuid)`, obsID, req.Kind, task, supersedes, system1NilUUID).Scan(&id, &existing)
	if err == nil {
		if existing != req.Label {
			writeError(w, http.StatusConflict, "this task already wrote a different label for this observation")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": uuidToString(id), "label": existing})
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to write label")
		return
	}

	// A new label needs no live labeller row; a correction must name the live one.
	var live pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM system1_label WHERE source='labeller' AND observation_id=$1 AND kind=$2 AND superseded_at IS NULL LIMIT 1 FOR UPDATE`,
		obsID, req.Kind).Scan(&live)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to write label")
		return
	}
	if live != supersedes {
		writeError(w, http.StatusConflict, "a live label exists; send its id as supersedes to correct it")
		return
	}
	if live.Valid {
		if _, err := tx.Exec(ctx, `UPDATE system1_label SET superseded_at=clock_timestamp() WHERE id=$1`, live); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to write label")
			return
		}
	}
	err = tx.QueryRow(ctx, `INSERT INTO system1_label (workspace_id, observation_id, scope, kind, source, label, task_id, agent_id, supersedes)
 VALUES ($1,$2,$3,$4,'labeller',$5,$6,$7,$8) RETURNING id`, workspaceID, obsID, scope, req.Kind, req.Label, task, agent, supersedes).Scan(&id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write label")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write label")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": uuidToString(id), "label": req.Label})
}

const system1LabellersKey = "system1_labellers"

// reconcileSystem1Labellers makes a generic settings save keep the stored
// assignment: only SetSystem1Labeller (owner, human) changes it.
func reconcileSystem1Labellers(stored, incoming map[string]any) {
	delete(incoming, system1LabellersKey)
	if v, ok := stored[system1LabellersKey]; ok {
		incoming[system1LabellersKey] = v
	}
}

// SetSystem1Labeller assigns the labeller agent for one scope (owner, human
// only). An empty agent_id clears the assignment.
func (h *Handler) SetSystem1Labeller(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace_id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceRole(w, r, uuidToString(workspaceID), "workspace not found", "owner"); !ok {
		return
	}
	var body struct {
		Scope   string `json:"scope"`
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if _, known := system1LabelValues[body.Scope]; !known {
		writeError(w, http.StatusBadRequest, "unknown scope")
		return
	}
	if body.AgentID == "" {
		if _, err := h.DB.Exec(r.Context(), `UPDATE workspace SET settings=coalesce(settings,'{}'::jsonb) #- ARRAY['system1_labellers',$2::text] WHERE id=$1`, workspaceID, body.Scope); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save setting")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"scope": body.Scope, "agent_id": ""})
		return
	}
	agentUUID, ok := parseUUIDOrBadRequest(w, body.AgentID, "agent_id")
	if !ok {
		return
	}
	tag, err := h.DB.Exec(r.Context(), `UPDATE workspace w SET settings=jsonb_set(coalesce(w.settings,'{}'::jsonb),'{system1_labellers}',
 coalesce(w.settings->'system1_labellers','{}'::jsonb) || jsonb_build_object($3::text,$4::text))
 WHERE w.id=$1 AND EXISTS (SELECT 1 FROM agent a WHERE a.id=$2 AND a.workspace_id=$1 AND a.archived_at IS NULL)`, workspaceID, agentUUID, body.Scope, uuidToString(agentUUID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save setting")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusBadRequest, "agent not found in this workspace")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope": body.Scope, "agent_id": uuidToString(agentUUID)})
}
