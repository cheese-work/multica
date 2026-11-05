package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/workflowlint"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Workflow lint API (CHE-3658). One engine, workflowlint; these endpoints load
// the outside state it needs (agents, runtimes, catalog, usage) and run it.

// WorkflowLintResponse is the lint result for one workflow. The counts skip
// waived findings, which are returned but do not fail a run.
type WorkflowLintResponse struct {
	WorkflowID string                 `json:"workflow_id"`
	Workflow   string                 `json:"workflow"`
	Findings   []workflowlint.Finding `json:"findings"`
	Errors     int                    `json:"errors"`
	Warnings   int                    `json:"warnings"`
	Infos      int                    `json:"infos"`
}

func lintResponse(row db.IssueWorkflow, findings []workflowlint.Finding) WorkflowLintResponse {
	resp := WorkflowLintResponse{WorkflowID: uuidToString(row.ID), Workflow: row.Name, Findings: findings}
	if resp.Findings == nil {
		resp.Findings = []workflowlint.Finding{}
	}
	for _, f := range findings {
		if f.Waived {
			continue
		}
		switch f.Level {
		case workflowlint.LevelError:
			resp.Errors++
		case workflowlint.LevelWarning:
			resp.Warnings++
		case workflowlint.LevelInfo:
			resp.Infos++
		}
	}
	return resp
}

// workflowLintEnv loads what the agent, runtime, usage and instruction rules
// look at, once for every workflow in rows.
func (h *Handler) workflowLintEnv(ctx context.Context, wsUUID pgtype.UUID, rows []db.IssueWorkflow) (workflowlint.Env, map[string][]string, error) {
	env := workflowlint.Env{Live: true}
	catalog, err := h.workflowStatusCatalog(ctx, h.Queries, wsUUID)
	if err != nil {
		return env, nil, err
	}
	env.Statuses = make(map[string]string, len(catalog))
	for key, entry := range catalog {
		env.Statuses[key] = entry.Name
	}

	agents, err := h.Queries.ListAllAgents(ctx, wsUUID)
	if err != nil {
		return env, nil, err
	}
	runtimes, err := h.Queries.ListAgentRuntimes(ctx, wsUUID)
	if err != nil {
		return env, nil, err
	}
	runtimeByID := make(map[string]db.AgentRuntime, len(runtimes))
	for _, rt := range runtimes {
		runtimeByID[uuidToString(rt.ID)] = rt
	}
	env.Agents = make(map[string]workflowlint.Agent, len(agents))
	for _, a := range agents {
		la := workflowlint.Agent{
			Model:    a.Model.String,
			Archived: a.ArchivedAt.Valid,
			Paused:   a.SchedulingPausedAt.Valid,
		}
		if rt, ok := runtimeByID[uuidToString(a.RuntimeID)]; ok {
			la.Provider, la.RuntimeID, la.RuntimeOnline = rt.Provider, uuidToString(rt.ID), rt.Status == "online"
			la.RuntimeVersion = runtimeVersion(rt.Metadata)
		}
		env.Agents[uuidToString(a.ID)] = la
	}

	byWorkflow, err := h.projectIDsByWorkflow(ctx, wsUUID, rows)
	if err != nil {
		return env, nil, err
	}
	return env, byWorkflow, nil
}

// runtimeVersion reads the agent CLI version a daemon reported in the
// runtime's metadata, for example "codex-cli 0.155.1".
func runtimeVersion(metadata []byte) string {
	var m struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(metadata, &m)
	return m.Version
}

// lintRows lints each row with env. A row that does not decode is reported as
// an error finding rather than failing the whole run.
func lintRows(rows []db.IssueWorkflow, defs map[string]issueworkflow.Definition, env workflowlint.Env, byWorkflow map[string][]string) []WorkflowLintResponse {
	referenced := map[string]bool{}
	for _, def := range defs {
		for _, s := range def.Steps {
			if s.SubWorkflow != nil {
				referenced[s.SubWorkflow.WorkflowID] = true
			}
		}
	}
	out := make([]WorkflowLintResponse, 0, len(rows))
	for _, row := range rows {
		id := uuidToString(row.ID)
		def, ok := defs[id]
		if !ok {
			out = append(out, lintResponse(row, []workflowlint.Finding{{
				Rule: "WF000", Level: workflowlint.LevelError, Workflow: row.Name,
				Message: "The stored steps do not decode.", Hint: "Rewrite the workflow steps.",
			}}))
			continue
		}
		env := env
		env.Usage = &workflowlint.Usage{Projects: len(byWorkflow[id]), ReferencedByWorkflow: referenced[id]}
		out = append(out, lintResponse(row, workflowlint.LintWith(def, env)))
	}
	return out
}

func decodeRows(rows []db.IssueWorkflow) map[string]issueworkflow.Definition {
	defs := make(map[string]issueworkflow.Definition, len(rows))
	for _, row := range rows {
		if def, err := issueworkflow.Decode(row); err == nil {
			defs[uuidToString(row.ID)] = def
		}
	}
	return defs
}

func (h *Handler) lintWorkflows(w http.ResponseWriter, r *http.Request, wsUUID pgtype.UUID, rows []db.IssueWorkflow, all []db.IssueWorkflow) ([]WorkflowLintResponse, bool) {
	// Usage and cross-references are judged against every workflow of the
	// workspace, whichever subset is linted.
	defs := decodeRows(all)
	env, byWorkflow, err := h.workflowLintEnv(r.Context(), wsUUID, all)
	if err != nil {
		slog.Warn("workflow lint env failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to lint workflows")
		return nil, false
	}
	return lintRows(rows, defs, env, byWorkflow), true
}

// LintIssueWorkflow lints one workflow. Any workspace member may read it.
func (h *Handler) LintIssueWorkflow(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}
	idUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workflow id")
	if !ok {
		return
	}
	row, err := h.Queries.GetIssueWorkflow(r.Context(), db.GetIssueWorkflowParams{ID: idUUID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusNotFound, "workflow not found")
		return
	}
	all, err := h.Queries.ListIssueWorkflows(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lint workflows")
		return
	}
	res, ok := h.lintWorkflows(w, r, wsUUID, []db.IssueWorkflow{row}, all)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, res[0])
}

// LintIssueWorkflows lints every workflow of the workspace.
func (h *Handler) LintIssueWorkflows(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}
	rows, err := h.Queries.ListIssueWorkflows(r.Context(), wsUUID)
	if err != nil {
		slog.Warn("LintIssueWorkflows list failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to lint workflows")
		return
	}
	res, ok := h.lintWorkflows(w, r, wsUUID, rows, rows)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": res, "total": len(res)})
}
