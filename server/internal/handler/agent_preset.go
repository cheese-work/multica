package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/agentpreset"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// presetResponse is one preset as the API shows it. Stale is the reason the
// preset cannot run for the agent as it is now; empty when it can.
type presetResponse struct {
	Name          string   `json:"name"`
	Model         *string  `json:"model"`
	ThinkingLevel *string  `json:"thinking_level"`
	ServiceTier   *string  `json:"service_tier"`
	ContextArgs   []string `json:"context_args"`
	IsDefault     bool     `json:"is_default"`
	Stale         string   `json:"stale,omitempty"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

// stalePreset is a preset that cannot run for its agent, named in the agent
// update response so a rebind does not strand one silently (E5).
type stalePreset struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// presetFromRow decodes a stored preset. A NULL column inherits; context_args
// NULL inherits and `[]` clears.
func presetFromRow(row db.AgentPreset) agentpreset.Preset {
	p := agentpreset.Preset{Name: row.Name, IsDefault: row.IsDefault}
	if row.Model.Valid {
		p.Model = &row.Model.String
	}
	if row.ThinkingLevel.Valid {
		p.ThinkingLevel = &row.ThinkingLevel.String
	}
	if row.ServiceTier.Valid {
		p.ServiceTier = &row.ServiceTier.String
	}
	if len(row.ContextArgs) > 0 {
		if err := json.Unmarshal(row.ContextArgs, &p.ContextArgs); err != nil {
			slog.Warn("unreadable preset context_args", "preset", row.Name, "error", err)
		}
	}
	return p
}

func textOf(s *string) pgtype.Text {
	if s == nil || *s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func contextArgsJSON(args []string) []byte {
	if args == nil {
		return nil
	}
	b, _ := json.Marshal(args)
	return b
}

// presetLabs reads the lab lock; a malformed value means no lock, same as the claim.
func presetLabs() agentpreset.Labs {
	var labs agentpreset.Labs
	_ = json.Unmarshal([]byte(os.Getenv("MULTICA_VOTE_GATE_LABS")), &labs)
	return labs
}

// presetAgentOf is the agent as the preset checks see it.
func (h *Handler) presetAgentOf(ctx context.Context, a db.Agent) (agentpreset.Agent, bool) {
	if !a.RuntimeID.Valid {
		return agentpreset.Agent{}, false
	}
	rt, err := h.Queries.GetAgentRuntime(ctx, a.RuntimeID)
	if err != nil {
		return agentpreset.Agent{}, false
	}
	return agentpreset.Agent{Provider: rt.Provider, Model: a.Model.String, ThinkingLevel: a.ThinkingLevel.String, ServiceTier: a.ServiceTier.String}, true
}

func presetToResponse(row db.AgentPreset, stale string) presetResponse {
	p := presetFromRow(row)
	return presetResponse{
		Name: p.Name, Model: p.Model, ThinkingLevel: p.ThinkingLevel, ServiceTier: p.ServiceTier,
		ContextArgs: p.ContextArgs, IsDefault: p.IsDefault, Stale: stale,
		CreatedAt: timestampToString(row.CreatedAt), UpdatedAt: timestampToString(row.UpdatedAt),
	}
}

// listPresetsWithStale returns the agent's presets with the reason each one is
// stale ("" when usable). With no runtime nothing can be judged, so none is stale.
func (h *Handler) listPresetsWithStale(ctx context.Context, a db.Agent) ([]db.AgentPreset, []string, error) {
	rows, err := h.Queries.ListAgentPresets(ctx, db.ListAgentPresetsParams{WorkspaceID: a.WorkspaceID, AgentID: a.ID})
	if err != nil {
		return nil, nil, err
	}
	stale := make([]string, len(rows))
	if view, ok := h.presetAgentOf(ctx, a); ok {
		labs := presetLabs()
		for i, row := range rows {
			_, stale[i] = agentpreset.Effective(view, labs, presetFromRow(row))
		}
	}
	return rows, stale, nil
}

// stalePresetsOf lists the stale presets of an agent for the update response.
func (h *Handler) stalePresetsOf(ctx context.Context, a db.Agent) []stalePreset {
	rows, stale, err := h.listPresetsWithStale(ctx, a)
	if err != nil {
		slog.Warn("update agent: could not list presets for the stale check", "agent_id", uuidToString(a.ID), "error", err)
		return nil
	}
	var out []stalePreset
	for i, reason := range stale {
		if reason != "" {
			out = append(out, stalePreset{Name: rows[i].Name, Reason: reason})
		}
	}
	return out
}

// presetAgentForManage loads the agent and checks the caller may manage it.
func (h *Handler) presetAgentForManage(w http.ResponseWriter, r *http.Request) (db.Agent, bool) {
	a, ok := h.loadAgentForUser(w, r, chi.URLParam(r, "id"))
	if !ok || !h.canManageAgent(w, r, a) {
		return db.Agent{}, false
	}
	return a, true
}

// validatePreset checks a preset against the agent as it is now. It writes the 400/409 itself.
func (h *Handler) validatePreset(w http.ResponseWriter, r *http.Request, a db.Agent, p agentpreset.Preset) bool {
	view, ok := h.presetAgentOf(r.Context(), a)
	if !ok {
		writeError(w, http.StatusConflict, "agent has no runtime to validate a preset against")
		return false
	}
	if err := agentpreset.Validate(view, presetLabs(), p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

type createPresetRequest struct {
	Name          string   `json:"name"`
	Model         *string  `json:"model"`
	ThinkingLevel *string  `json:"thinking_level"`
	ServiceTier   *string  `json:"service_tier"`
	ContextArgs   []string `json:"context_args"`
	IsDefault     bool     `json:"is_default"`
}

// CreateAgentPreset adds a preset. Unknown fields are refused, so identity and
// permission fields (custom_args, instructions, mcp_config, ...) cannot ride along (I1).
func (h *Handler) CreateAgentPreset(w http.ResponseWriter, r *http.Request) {
	a, ok := h.presetAgentForManage(w, r)
	if !ok {
		return
	}
	var req createPresetRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	p := agentpreset.Preset{Name: req.Name, IsDefault: req.IsDefault, ContextArgs: req.ContextArgs}
	for _, f := range []struct {
		in  *string
		out **string
	}{{req.Model, &p.Model}, {req.ThinkingLevel, &p.ThinkingLevel}, {req.ServiceTier, &p.ServiceTier}} {
		if f.in != nil && *f.in != "" {
			*f.out = f.in
		}
	}
	if !h.validatePreset(w, r, a, p) {
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create preset")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if p.IsDefault {
		if err := qtx.ClearAgentPresetDefault(r.Context(), db.ClearAgentPresetDefaultParams{WorkspaceID: a.WorkspaceID, AgentID: a.ID}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create preset")
			return
		}
	}
	row, err := qtx.CreateAgentPreset(r.Context(), db.CreateAgentPresetParams{
		WorkspaceID: a.WorkspaceID, AgentID: a.ID, Name: p.Name,
		Model: textOf(p.Model), ThinkingLevel: textOf(p.ThinkingLevel), ServiceTier: textOf(p.ServiceTier),
		ContextArgs: contextArgsJSON(p.ContextArgs), IsDefault: p.IsDefault, CreatedBy: parseUUID(requestUserID(r)),
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a preset with that name already exists")
			return
		}
		slog.Warn("CreateAgentPreset failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to create preset")
		return
	}
	if err := h.writeAudit(r.Context(), qtx, r, auditInput{
		Kind: "agent.preset.create", WorkspaceID: a.WorkspaceID, TargetID: a.ID, After: presetAuditFields(&row),
	}); err != nil {
		writeAuditFailure(w, r, err, "agent.preset.create")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create preset")
		return
	}
	writeJSON(w, http.StatusCreated, presetToResponse(row, ""))
}

var patchablePresetFields = []string{"model", "thinking_level", "service_tier", "context_args", "is_default"}

// UpdateAgentPreset is a tri-state PATCH: an omitted field is unchanged, JSON
// null inherits the agent's value, a value sets it (S4).
func (h *Handler) UpdateAgentPreset(w http.ResponseWriter, r *http.Request) {
	a, ok := h.presetAgentForManage(w, r)
	if !ok {
		return
	}
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	for k := range fields {
		if !slices.Contains(patchablePresetFields, k) {
			writeError(w, http.StatusBadRequest, "unknown preset field: "+k)
			return
		}
	}
	name := chi.URLParam(r, "name")
	row, err := h.Queries.GetAgentPresetByName(r.Context(), db.GetAgentPresetByNameParams{WorkspaceID: a.WorkspaceID, AgentID: a.ID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "preset not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load preset")
		return
	}

	p := presetFromRow(row)
	for key, dst := range map[string]**string{"model": &p.Model, "thinking_level": &p.ThinkingLevel, "service_tier": &p.ServiceTier} {
		raw, set := fields[key]
		if !set {
			continue
		}
		var v *string
		if err := json.Unmarshal(raw, &v); err != nil {
			writeError(w, http.StatusBadRequest, key+" must be a string or null")
			return
		}
		if v != nil && *v == "" {
			v = nil
		}
		*dst = v
	}
	if raw, set := fields["context_args"]; set {
		var args []string
		if err := json.Unmarshal(raw, &args); err != nil {
			writeError(w, http.StatusBadRequest, "context_args must be an array of strings or null")
			return
		}
		p.ContextArgs = args // null leaves it nil = inherit
	}
	if raw, set := fields["is_default"]; set {
		if err := json.Unmarshal(raw, &p.IsDefault); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			writeError(w, http.StatusBadRequest, "is_default must be true or false")
			return
		}
	}
	if !h.validatePreset(w, r, a, p) {
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update preset")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if p.IsDefault && !row.IsDefault {
		if err := qtx.ClearAgentPresetDefault(r.Context(), db.ClearAgentPresetDefaultParams{WorkspaceID: a.WorkspaceID, AgentID: a.ID}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update preset")
			return
		}
	}
	updated, err := qtx.UpdateAgentPreset(r.Context(), db.UpdateAgentPresetParams{
		WorkspaceID: a.WorkspaceID, AgentID: a.ID, Name: name,
		Model: textOf(p.Model), ThinkingLevel: textOf(p.ThinkingLevel), ServiceTier: textOf(p.ServiceTier),
		ContextArgs: contextArgsJSON(p.ContextArgs), IsDefault: p.IsDefault,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update preset")
		return
	}
	if err := h.writeAudit(r.Context(), qtx, r, auditInput{
		Kind: "agent.preset.update", WorkspaceID: a.WorkspaceID, TargetID: a.ID,
		Before: presetAuditFields(&row), After: presetAuditFields(&updated),
	}); err != nil {
		writeAuditFailure(w, r, err, "agent.preset.update")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update preset")
		return
	}
	writeJSON(w, http.StatusOK, presetToResponse(updated, ""))
}

func (h *Handler) DeleteAgentPreset(w http.ResponseWriter, r *http.Request) {
	a, ok := h.presetAgentForManage(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete preset")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	row, err := qtx.GetAgentPresetByName(r.Context(), db.GetAgentPresetByNameParams{WorkspaceID: a.WorkspaceID, AgentID: a.ID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "preset not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete preset")
		return
	}
	if _, err := qtx.DeleteAgentPreset(r.Context(), db.DeleteAgentPresetParams{WorkspaceID: a.WorkspaceID, AgentID: a.ID, Name: name}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete preset")
		return
	}
	if err := h.writeAudit(r.Context(), qtx, r, auditInput{
		Kind: "agent.preset.delete", WorkspaceID: a.WorkspaceID, TargetID: a.ID, Before: presetAuditFields(&row),
	}); err != nil {
		writeAuditFailure(w, r, err, "agent.preset.delete")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete preset")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListAgentPresets lists the agent's presets, each with its live stale reason.
func (h *Handler) ListAgentPresets(w http.ResponseWriter, r *http.Request) {
	a, ok := h.loadAgentForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	rows, stale, err := h.listPresetsWithStale(r.Context(), a)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list presets")
		return
	}
	out := make([]presetResponse, len(rows))
	for i, row := range rows {
		out[i] = presetToResponse(row, stale[i])
	}
	writeJSON(w, http.StatusOK, out)
}

// SetAgentDefaultPreset makes one preset the agent's default; `base` leaves none (S5).
func (h *Handler) SetAgentDefaultPreset(w http.ResponseWriter, r *http.Request) {
	a, ok := h.presetAgentForManage(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	var target db.AgentPreset
	if req.Name != agentpreset.BaseName {
		var err error
		target, err = h.Queries.GetAgentPresetByName(r.Context(), db.GetAgentPresetByNameParams{WorkspaceID: a.WorkspaceID, AgentID: a.ID, Name: req.Name})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "preset not found")
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load preset")
			return
		}
		if !h.validatePreset(w, r, a, presetFromRow(target)) {
			return
		}
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set default preset")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	snapshot := func() (map[string]any, error) {
		rows, err := qtx.ListAgentPresets(r.Context(), db.ListAgentPresetsParams{WorkspaceID: a.WorkspaceID, AgentID: a.ID})
		if err != nil {
			return nil, err
		}
		current := agentpreset.BaseName
		for _, row := range rows {
			if row.IsDefault {
				current = row.Name
			}
		}
		return map[string]any{"default_preset": current}, nil
	}
	before, err := snapshot()
	if err == nil {
		err = qtx.ClearAgentPresetDefault(r.Context(), db.ClearAgentPresetDefaultParams{WorkspaceID: a.WorkspaceID, AgentID: a.ID})
	}
	if err == nil && req.Name != agentpreset.BaseName {
		_, err = qtx.UpdateAgentPreset(r.Context(), db.UpdateAgentPresetParams{
			WorkspaceID: a.WorkspaceID, AgentID: a.ID, Name: target.Name,
			Model: target.Model, ThinkingLevel: target.ThinkingLevel, ServiceTier: target.ServiceTier,
			ContextArgs: target.ContextArgs, IsDefault: true,
		})
	}
	var after map[string]any
	if err == nil {
		after, err = snapshot()
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set default preset")
		return
	}
	if err := h.writeAudit(r.Context(), qtx, r, auditInput{
		Kind: "agent.preset.default", WorkspaceID: a.WorkspaceID, TargetID: a.ID, Before: before, After: after,
	}); err != nil {
		writeAuditFailure(w, r, err, "agent.preset.default")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set default preset")
		return
	}
	h.ListAgentPresets(w, r)
}

// AgentPresetCompletions is the data source for `/preset <TAB>` and the CLI
// completion: base, reset, then the agent's presets that can run now. It needs
// the same permission as invoking the agent, so a caller who cannot use the
// agent learns none of its preset names.
func (h *Handler) AgentPresetCompletions(w http.ResponseWriter, r *http.Request) {
	a, ok := h.loadAgentForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	wsID := uuidToString(a.WorkspaceID)
	actorType, actorID := h.resolveActor(r, requestUserID(r), wsID)
	if !h.canInvokeAgent(r.Context(), a, actorType, actorID, h.invokeOriginatorFromRequest(r, actorType, actorID), wsID) {
		writeError(w, http.StatusForbidden, "you cannot use this agent")
		return
	}
	names, err := h.usablePresetNames(r.Context(), a)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list presets")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"names": append([]string{agentpreset.BaseName, "reset"}, names...)})
}

// usablePresetNames are the agent's presets that are not stale, by name.
func (h *Handler) usablePresetNames(ctx context.Context, a db.Agent) ([]string, error) {
	rows, stale, err := h.listPresetsWithStale(ctx, a)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for i, row := range rows {
		if stale[i] == "" {
			names = append(names, row.Name)
		}
	}
	return names, nil
}

// AgentPresetUsage sums tokens and cost per preset the agent's runs were claimed under (R7).
func (h *Handler) AgentPresetUsage(w http.ResponseWriter, r *http.Request) {
	a, ok := h.loadAgentForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	rows, err := h.Queries.AgentPresetUsage(r.Context(), a.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load preset usage")
		return
	}
	if rows == nil {
		rows = []db.AgentPresetUsageRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// presetNameOK reports whether name can be selected: a valid preset name, or base.
func presetNameOK(name string) bool {
	return name == agentpreset.BaseName || agentpreset.ValidateName(name) == nil
}
