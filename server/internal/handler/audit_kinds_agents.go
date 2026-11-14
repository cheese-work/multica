package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds for the remaining agent admin routes (CHE-5646). Labels, MCP
// servers, skills, presets, lifecycle and scheduling pause. agent.create and
// agent.skills.update are reused where the route already has a kind.
func init() {
	agent := func(allow []string, text ...string) auditKindSpec {
		return auditKindSpec{TargetType: "agent", Allow: allow, Text: text}
	}
	// Ids are not secret. Preset context_args and the pause reason are free
	// text, so neither is allowlisted: context_args stores the marker and
	// reason a digest.
	preset := agent([]string{"name", "model", "thinking_level", "service_tier", "is_default"})
	for _, kind := range []string{"agent.preset.create", "agent.preset.update", "agent.preset.delete"} {
		auditKinds[kind] = preset
	}
	auditKinds["agent.preset.default"] = agent([]string{"default_preset"})
	for _, kind := range []string{"agent.label.attach", "agent.label.detach"} {
		auditKinds[kind] = agent([]string{"label_ids"})
	}
	auditKinds["agent.mcp_server.remove"] = agent([]string{"mcp_server_ids"})
	auditKinds["agent.mcp_server.enable"] = agent([]string{"mcp_server_states"})
	auditKinds["agent.skill.enable"] = agent([]string{"skill_states"})
	for _, kind := range []string{"agent.archive", "agent.restore"} {
		auditKinds[kind] = agent([]string{"archived", "status"})
	}
	for _, kind := range []string{"agent.pause", "agent.resume"} {
		auditKinds[kind] = agent([]string{"scheduling_paused"}, "pause_reason")
	}
	auditKinds["agent.tasks.cancel"] = agent([]string{"active_tasks"})
}

// errAgentChangeNotFound is returned by an apply func when its target row is
// not there; the handler answers 404.
var errAgentChangeNotFound = errors.New("agent change target not found")

// applyAgentChange runs apply and the audit row in one transaction. snapshot
// reads the audited fields before and after apply, through the same tx.
func (h *Handler) applyAgentChange(
	r *http.Request, agent db.Agent, kind string,
	snapshot func(context.Context, *db.Queries) (map[string]any, error),
	apply func(context.Context, *db.Queries) error,
) error {
	ctx := r.Context()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := h.Queries.WithTx(tx)
	before, err := snapshot(ctx, q)
	if err != nil {
		return err
	}
	if err := apply(ctx, q); err != nil {
		return err
	}
	after, err := snapshot(ctx, q)
	if err != nil {
		return err
	}
	if err := h.writeAudit(ctx, q, r, auditInput{
		Kind: kind, WorkspaceID: agent.WorkspaceID, TargetID: agent.ID, Before: before, After: after,
	}); err != nil {
		return auditWriteError{err}
	}
	return tx.Commit(ctx)
}

// writeAgentChangeError answers a failed applyAgentChange: an audit failure is
// 500 + Retry-After, a missing target 404 with notFound, anything else 500 with
// failed.
func writeAgentChangeError(w http.ResponseWriter, r *http.Request, err error, kind, notFound, failed string) {
	switch {
	case failedAudit(w, r, err, kind):
	case errors.Is(err, errAgentChangeNotFound):
		writeError(w, http.StatusNotFound, notFound)
	default:
		writeError(w, http.StatusInternalServerError, failed)
	}
}

// agentLabelsAuditSnapshot is the label_ids value: the agent's labels, sorted.
func agentLabelsAuditSnapshot(agent db.Agent) func(context.Context, *db.Queries) (map[string]any, error) {
	return func(ctx context.Context, q *db.Queries) (map[string]any, error) {
		labels, err := q.ListLabelsByAgent(ctx, db.ListLabelsByAgentParams{AgentID: agent.ID, WorkspaceID: agent.WorkspaceID})
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(labels))
		for i, l := range labels {
			ids[i] = uuidToString(l.ID)
		}
		return map[string]any{"label_ids": idsAuditField(ids)}, nil
	}
}

// agentMcpServersAuditSnapshot is the mcp_server_ids value (and, with states,
// "id:enabled" pairs). The server config is never read.
func agentMcpServersAuditSnapshot(agent db.Agent, states bool) func(context.Context, *db.Queries) (map[string]any, error) {
	return func(ctx context.Context, q *db.Queries) (map[string]any, error) {
		servers, err := q.ListAgentMcpServers(ctx, agent.ID)
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(servers))
		for i, sv := range servers {
			ids[i] = uuidToString(sv.ID)
			if states {
				ids[i] += ":" + strconv.FormatBool(sv.Enabled)
			}
		}
		if states {
			return map[string]any{"mcp_server_states": idsAuditField(ids)}, nil
		}
		return map[string]any{"mcp_server_ids": idsAuditField(ids)}, nil
	}
}

// agentSkillsAuditSnapshot is the skill_ids value of the enabled skills.
func agentSkillsAuditSnapshot(agent db.Agent) func(context.Context, *db.Queries) (map[string]any, error) {
	return func(ctx context.Context, q *db.Queries) (map[string]any, error) {
		ids, err := agentSkillsAuditField(ctx, q, agent.ID)
		return map[string]any{"skill_ids": ids}, err
	}
}

// agentSkillStatesAuditSnapshot lists every bound skill as "id:enabled".
func agentSkillStatesAuditSnapshot(agent db.Agent) func(context.Context, *db.Queries) (map[string]any, error) {
	return func(ctx context.Context, q *db.Queries) (map[string]any, error) {
		skills, err := q.ListAgentSkillSummaries(ctx, agent.ID)
		if err != nil {
			return nil, err
		}
		states := make([]string, len(skills))
		for i, sk := range skills {
			states[i] = uuidToString(sk.ID) + ":" + strconv.FormatBool(sk.Enabled)
		}
		return map[string]any{"skill_states": idsAuditField(states)}, nil
	}
}

// presetAuditFields snapshots one preset; nil means the preset is absent.
func presetAuditFields(p *db.AgentPreset) map[string]any {
	if p == nil {
		return nil
	}
	return map[string]any{
		"name":           p.Name,
		"model":          auditText(p.Model),
		"thinking_level": auditText(p.ThinkingLevel),
		"service_tier":   auditText(p.ServiceTier),
		"context_args":   string(p.ContextArgs),
		"is_default":     p.IsDefault,
	}
}

// agentLifecycleAuditSnapshot is the archived + status pair of archive/restore.
func agentLifecycleAuditSnapshot(agent db.Agent) func(context.Context, *db.Queries) (map[string]any, error) {
	return func(ctx context.Context, q *db.Queries) (map[string]any, error) {
		a, err := q.GetAgent(ctx, agent.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"archived": a.ArchivedAt.Valid, "status": a.Status}, nil
	}
}

// schedulingAuditFields snapshots the pause flag; the reason is a digest-only
// text field.
func schedulingAuditFields(a db.Agent) map[string]any {
	return map[string]any{
		"scheduling_paused": a.SchedulingPausedAt.Valid,
		"pause_reason":      auditText(a.SchedulingPausedReason),
	}
}
