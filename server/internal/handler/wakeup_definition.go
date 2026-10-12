package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Scoped wakeup definition API (CHE-1082 L4). One set of handlers serves the
// workspace, project and issue scopes; WakeupDefinitionAPI binds the scope.
// Writes sit behind the activation gate (featureflags.WakeupDefinitionWrites,
// closed by default). Reads and previews never persist, so they are not gated.

type wakeupDefinitionAPI struct {
	h    *Handler
	kind service.WakeupScope
}

// WakeupDefinitionAPI returns the definition handlers of one scope.
func (h *Handler) WakeupDefinitionAPI(kind service.WakeupScope) wakeupDefinitionAPI {
	return wakeupDefinitionAPI{h: h, kind: kind}
}

// wireRevision is a definition revision on the wire. Workspace alias revisions
// reach 62 bits, which a JSON number cannot carry through JavaScript, so it is
// written as a decimal string. A request may send a string or, for small
// values, a bare number.
type wireRevision int64

func (r wireRevision) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(strconv.FormatInt(int64(r), 10))), nil
}

func (r *wireRevision) UnmarshalJSON(b []byte) error {
	text := string(bytes.TrimSpace(b))
	if unquoted, err := strconv.Unquote(text); err == nil {
		text = unquoted
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 0 {
		return errors.New("revision must be a non-negative integer")
	}
	*r = wireRevision(n)
	return nil
}

type wakeupDefinitionResponse struct {
	Scope     string          `json:"scope"`
	ScopeID   string          `json:"scope_id"`
	RuleKey   string          `json:"rule_key"`
	Root      bool            `json:"root"`
	Revision  wireRevision    `json:"revision"`
	Config    json.RawMessage `json:"config"`
	UpdatedAt *time.Time      `json:"updated_at"`
	// Redacted is set when the viewer may not see the target agent: the
	// target and instruction are withheld.
	Redacted bool `json:"redacted,omitempty"`
}

type wakeupCapabilities struct {
	// DefinitionWrites is the activation gate; clients hide editing when false.
	DefinitionWrites bool     `json:"definition_writes"`
	TriggerKinds     []string `json:"trigger_kinds"`
	Fields           []string `json:"fields"`
}

type wakeupDefinitionListResponse struct {
	Definitions  []wakeupDefinitionResponse `json:"definitions"`
	Capabilities wakeupCapabilities         `json:"capabilities"`
}

type wakeupAggregateCapResponse struct {
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id"`
	Limit   int    `json:"limit"`
}

type wakeupExecutionResponse struct {
	InstanceID   string  `json:"instance_id"`
	Enabled      bool    `json:"enabled"`
	PausedReason *string `json:"paused_reason"`
}

type wakeupEffectiveResponse struct {
	RuleKey string `json:"rule_key"`
	Scope   string `json:"scope"`
	// Applicable is false when the rule resolves but cannot run here (a
	// custom rule whose root is gone); InapplicableReason says why.
	Applicable         bool   `json:"applicable"`
	InapplicableReason string `json:"inapplicable_reason,omitempty"`
	// Enabled is configuration state only; Execution carries the pause state
	// of the issue's runtime instance when one exists.
	Enabled bool            `json:"enabled"`
	Config  json.RawMessage `json:"config"`
	// Sources maps each set field to the scope that supplied it; Overrides
	// lists the fields this scope sets over an inherited value.
	Sources       map[string]string            `json:"sources"`
	Overrides     []string                     `json:"overrides"`
	AggregateCaps []wakeupAggregateCapResponse `json:"aggregate_caps"`
	Fingerprint   string                       `json:"fingerprint"`
	Execution     *wakeupExecutionResponse     `json:"execution"`
	Redacted      bool                         `json:"redacted,omitempty"`
	Capabilities  wakeupCapabilities           `json:"capabilities"`
}

type wakeupDefinitionBody struct {
	RuleKey  string          `json:"rule_key"`
	Revision wireRevision    `json:"revision"`
	Config   json.RawMessage `json:"config"`
}

// wakeupDefinitionCaller is who is calling and the scope they act on.
type wakeupDefinitionCaller struct {
	ref       service.WakeupScopeRef
	actorType string
	actorID   string
	// member is the human the call acts for: the member themselves, or the
	// live run's originator for an agent. Invalid when none can be attributed.
	member pgtype.UUID
	role   string
}

// caller resolves the scope and enforces who may touch it. Reads need workspace
// membership (and issue access for an issue scope). Writes need a human: an
// owner or admin for workspace and project scope, any member with issue access
// for an issue scope. Agents never write, so they cannot broaden defaults or
// override human-managed rules.
func (a wakeupDefinitionAPI) caller(w http.ResponseWriter, r *http.Request, write bool) (wakeupDefinitionCaller, bool) {
	h := a.h
	var c wakeupDefinitionCaller
	c.ref.Kind = a.kind
	var workspaceID string
	if a.kind == service.WakeupScopeIssue {
		issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
		if !ok {
			return c, false
		}
		c.ref.ID, c.ref.WorkspaceID = issue.ID, issue.WorkspaceID
		workspaceID = uuidToString(issue.WorkspaceID)
	} else {
		if _, ok := requireUserID(w, r); !ok {
			return c, false
		}
		workspaceID = h.resolveWorkspaceID(r)
		ws, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
		if !ok {
			return c, false
		}
		c.ref.WorkspaceID, c.ref.ID = ws, ws
		if a.kind == service.WakeupScopeProject {
			projectID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
			if !ok {
				return c, false
			}
			if _, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: ws}); err != nil {
				writeError(w, http.StatusNotFound, "project not found")
				return c, false
			}
			c.ref.ID = projectID
		}
	}
	userID := requestUserID(r)
	member, err := h.getWorkspaceMember(r.Context(), userID, workspaceID)
	if err != nil {
		writeError(w, http.StatusForbidden, "wakeup permission denied")
		return c, false
	}
	c.role = member.Role
	c.actorType, c.actorID = h.resolveActor(r, userID, workspaceID)
	if originator := h.invokeOriginatorFromRequest(r, c.actorType, c.actorID); originator != "" {
		c.member = parseUUID(originator)
	}
	if !write {
		return c, true
	}
	switch {
	case isMachineCredentialActor(r):
		// A task token or cloud-node PAT carries its owner's user id and role, so
		// the checks below would otherwise let it write as that human.
		writeError(w, http.StatusForbidden, "machine credentials cannot manage wakeup definitions")
	case c.actorType == "agent":
		writeError(w, http.StatusForbidden, "agents cannot manage wakeup definitions")
	case a.kind != service.WakeupScopeIssue && !roleAllowed(member.Role, "owner", "admin"):
		writeError(w, http.StatusForbidden, "only owners and admins can manage workspace and project wakeup definitions")
	case !featureflags.WakeupDefinitionWritesEnabled(r.Context(), h.FeatureFlags):
		writeFeatureDisabled(w, "wakeup_definition_writes_closed", "wakeup definition writes are not enabled")
	default:
		return c, true
	}
	return c, false
}

func (h *Handler) wakeupCapabilities(r *http.Request) wakeupCapabilities {
	return wakeupCapabilities{
		DefinitionWrites: featureflags.WakeupDefinitionWritesEnabled(r.Context(), h.FeatureFlags),
		TriggerKinds:     service.WakeupDefinitionTriggerKinds(),
		Fields:           service.WakeupDefinitionFields(),
	}
}

func decodeWakeupDefinitionBody(w http.ResponseWriter, r *http.Request) (wakeupDefinitionBody, service.WakeupConfigPatch, bool) {
	var body wakeupDefinitionBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || len(body.Config) == 0 {
		writeError(w, http.StatusBadRequest, "invalid wakeup definition body")
		return body, service.WakeupConfigPatch{}, false
	}
	patch, err := service.DecodeWakeupConfigPatch(body.Config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return body, patch, false
	}
	return body, patch, true
}

// definitionView renders one definition for the caller, withholding a target
// and prompt they may not see.
func (h *Handler) definitionView(r *http.Request, c wakeupDefinitionCaller, v service.WakeupDefinitionView) (wakeupDefinitionResponse, error) {
	patch, redacted := h.redactWakeupPatch(r, c, v.Patch, v.Effective)
	raw, err := service.MarshalWakeupConfigPatch(patch)
	if err != nil {
		return wakeupDefinitionResponse{}, err
	}
	out := wakeupDefinitionResponse{
		Scope: string(v.Scope), ScopeID: uuidToString(v.ScopeID), RuleKey: v.RuleKey, Root: v.Root,
		Revision: wireRevision(v.Revision), Config: raw, Redacted: redacted,
	}
	if v.UpdatedAt.Valid {
		out.UpdatedAt = &v.UpdatedAt.Time
	}
	return out, nil
}

// redactWakeupPatch drops the target and instruction of p when the target the
// rule resolves to (an agent, or the leader of a squad) is one the caller may
// not view. Visibility follows the resolved target, not the patch: an
// instruction-only override inherits its target from an ancestor. A nil
// resolution fails closed.
func (h *Handler) redactWakeupPatch(r *http.Request, c wakeupDefinitionCaller, p service.WakeupConfigPatch, resolved *service.WakeupConfigPatch) (service.WakeupConfigPatch, bool) {
	if resolved != nil && h.wakeupTargetVisible(r, c, *resolved) {
		return p, false
	}
	p.Target = service.WakeupRedactedTarget()
	p.Instruction = service.WakeupRedactedInstruction()
	return p, true
}

func (h *Handler) wakeupTargetVisible(r *http.Request, c wakeupDefinitionCaller, p service.WakeupConfigPatch) bool {
	kind, id := service.WakeupPatchTarget(p)
	if kind != "agent" && kind != "squad" {
		return true
	}
	ctx, ws := r.Context(), c.ref.WorkspaceID
	if kind == "squad" {
		squad, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: id, WorkspaceID: ws})
		if err != nil {
			return false
		}
		id = squad.LeaderID
	}
	agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: ws})
	return err == nil && h.canAccessPrivateAgent(ctx, agent, c.actorType, c.actorID, uuidToString(ws))
}

func (a wakeupDefinitionAPI) service() *service.IssueWakeupService {
	return &service.IssueWakeupService{Tasks: a.h.TaskService}
}

func (a wakeupDefinitionAPI) List(w http.ResponseWriter, r *http.Request) {
	c, ok := a.caller(w, r, false)
	if !ok {
		return
	}
	rows, err := a.service().ListWakeupDefinitions(r.Context(), c.ref)
	if err != nil {
		slog.Warn("list wakeup definitions failed", "error", err, "scope", string(c.ref.Kind), "scope_id", uuidToString(c.ref.ID))
		writeError(w, http.StatusInternalServerError, "could not load wakeup definitions")
		return
	}
	out := wakeupDefinitionListResponse{Definitions: make([]wakeupDefinitionResponse, 0, len(rows)), Capabilities: a.h.wakeupCapabilities(r)}
	for _, row := range rows {
		view, err := a.h.definitionView(r, c, row)
		if err != nil {
			wakeupError(w, err)
			return
		}
		out.Definitions = append(out.Definitions, view)
	}
	writeJSON(w, http.StatusOK, out)
}

// Create makes a new custom root rule under a server-assigned key.
func (a wakeupDefinitionAPI) Create(w http.ResponseWriter, r *http.Request) {
	a.save(w, r, "", http.StatusCreated)
}

// Put creates or replaces one rule's definition at this scope.
func (a wakeupDefinitionAPI) Put(w http.ResponseWriter, r *http.Request) {
	a.save(w, r, chi.URLParam(r, "rule"), http.StatusOK)
}

func (a wakeupDefinitionAPI) save(w http.ResponseWriter, r *http.Request, ruleKey string, status int) {
	c, ok := a.caller(w, r, true)
	if !ok {
		return
	}
	body, patch, ok := decodeWakeupDefinitionBody(w, r)
	if !ok {
		return
	}
	if body.RuleKey != "" {
		writeError(w, http.StatusBadRequest, "rule_key belongs in the path")
		return
	}
	saved, err := a.service().SaveWakeupDefinition(r.Context(), c.ref, c.member, service.WakeupDefinitionWrite{RuleKey: ruleKey, Revision: int64(body.Revision), Patch: patch})
	if err != nil {
		wakeupError(w, err)
		return
	}
	view, err := a.h.definitionView(r, c, saved)
	if err != nil {
		wakeupError(w, err)
		return
	}
	writeJSON(w, status, view)
}

// Delete removes an override or custom rule at this scope, or resets it to the
// inherited settings. The observed revision is required.
func (a wakeupDefinitionAPI) Delete(w http.ResponseWriter, r *http.Request) {
	c, ok := a.caller(w, r, true)
	if !ok {
		return
	}
	revision, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || revision < 1 {
		writeError(w, http.StatusBadRequest, "revision is required")
		return
	}
	if err := a.service().DeleteWakeupDefinition(r.Context(), c.ref, chi.URLParam(r, "rule"), revision); err != nil {
		wakeupError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Effective resolves one rule as it applies at this scope.
func (a wakeupDefinitionAPI) Effective(w http.ResponseWriter, r *http.Request) {
	c, ok := a.caller(w, r, false)
	if !ok {
		return
	}
	a.effective(w, r, c, chi.URLParam(r, "rule"), nil)
}

// Preview resolves what a definition would do without saving it. An empty
// rule_key previews a new custom root rule.
func (a wakeupDefinitionAPI) Preview(w http.ResponseWriter, r *http.Request) {
	c, ok := a.caller(w, r, false)
	if !ok {
		return
	}
	// A preview probes what its owner may invoke and resolves private prompts,
	// so it is as human-only as the write it rehearses.
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "machine credentials cannot manage wakeup definitions")
		return
	}
	body, patch, ok := decodeWakeupDefinitionBody(w, r)
	if !ok {
		return
	}
	a.effective(w, r, c, body.RuleKey, &service.WakeupDefinitionWrite{RuleKey: body.RuleKey, Patch: patch})
}

func (a wakeupDefinitionAPI) effective(w http.ResponseWriter, r *http.Request, c wakeupDefinitionCaller, ruleKey string, proposal *service.WakeupDefinitionWrite) {
	eff, err := a.service().EffectiveWakeupRule(r.Context(), c.ref, c.member, ruleKey, proposal)
	if err != nil {
		wakeupError(w, err)
		return
	}
	patch, redacted := a.h.redactWakeupPatch(r, c, eff.Config, &eff.Config)
	raw, err := service.MarshalWakeupConfigPatch(patch)
	if err != nil {
		wakeupError(w, err)
		return
	}
	out := wakeupEffectiveResponse{
		RuleKey: eff.RuleKey, Scope: string(c.ref.Kind), Applicable: eff.Applicable, InapplicableReason: string(eff.Inapplicable),
		Enabled: eff.Enabled(), Config: raw, Sources: map[string]string{}, Overrides: eff.Overrides,
		AggregateCaps: []wakeupAggregateCapResponse{}, Fingerprint: eff.Fingerprint, Redacted: redacted,
		Capabilities: a.h.wakeupCapabilities(r),
	}
	for field, scope := range eff.Sources {
		out.Sources[field] = string(scope)
	}
	for _, cap := range eff.AggregateCaps {
		out.AggregateCaps = append(out.AggregateCaps, wakeupAggregateCapResponse{Scope: string(cap.Scope), ScopeID: uuidToString(cap.ScopeID), Limit: cap.Limit})
	}
	if c.ref.Kind == service.WakeupScopeIssue && proposal == nil {
		if out.Execution, err = a.h.wakeupExecution(r, c.ref, ruleKey); err != nil {
			wakeupError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// wakeupExecution reports the pause state of a platform rule's runtime
// instance on one issue; nil when it has none yet.
func (h *Handler) wakeupExecution(r *http.Request, ref service.WakeupScopeRef, ruleKey string) (*wakeupExecutionResponse, error) {
	row, err := h.Queries.GetSystemWakeup(r.Context(), db.GetSystemWakeupParams{IssueID: ref.ID, SystemRule: pgtype.Text{String: ruleKey, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := &wakeupExecutionResponse{InstanceID: uuidToString(row.ID), Enabled: row.Enabled}
	if row.PausedReason.Valid {
		out.PausedReason = &row.PausedReason.String
	}
	return out, nil
}
