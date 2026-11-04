package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// Routing agent (CHE-3627): the workspace setting that names the agent for
// routing-only workflow steps, a fallback, and per-status overrides. The handler
// package owns the route that changes it; this file owns reading it.

// RoutingAgentSettingsKey is the key inside workspace.settings.
const RoutingAgentSettingsKey = "routing_agent"

// DefaultRoutingStatuses are the steps that only route work to the next owner.
var DefaultRoutingStatuses = []string{"todo", "blocked", "merged", "need_decision"}

// RoutingAgentConfig is the stored setting. An empty AgentID means routing is
// off: every step keeps its own handler.
type RoutingAgentConfig struct {
	AgentID         string `json:"agent_id"`
	FallbackAgentID string `json:"fallback_agent_id"`
	// Statuses lists the routing steps; empty means DefaultRoutingStatuses.
	Statuses []string `json:"statuses"`
	// Overrides sends one routing status to another agent than AgentID. Its
	// keys are routing steps too.
	Overrides map[string]string `json:"overrides"`
}

func RoutingAgentFromSettings(settings map[string]any) RoutingAgentConfig {
	var cfg RoutingAgentConfig
	if raw, ok := settings[RoutingAgentSettingsKey]; ok {
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &cfg) // a malformed value routes nothing
		}
	}
	if cfg.Statuses == nil {
		cfg.Statuses = slices.Clone(DefaultRoutingStatuses)
	}
	if cfg.Overrides == nil {
		cfg.Overrides = map[string]string{}
	}
	return cfg
}

// AgentFor is the configured agent for a step entering status, or "" when the
// step keeps its own handler.
func (c RoutingAgentConfig) AgentFor(status string) string {
	if c.AgentID == "" {
		return ""
	}
	if override, ok := c.Overrides[status]; ok {
		return override
	}
	if slices.Contains(c.Statuses, status) {
		return c.AgentID
	}
	return ""
}

// FailoverFor is the fallback to run when failed, the configured agent for
// status, could not finish its run. "" when there is none to use.
func (c RoutingAgentConfig) FailoverFor(failed, status string) string {
	if c.FallbackAgentID == "" || c.FallbackAgentID == failed || c.AgentFor(status) != failed {
		return ""
	}
	return c.FallbackAgentID
}

// providerFailureReasons are the failures a different agent can outlast: the
// run died on its provider, not on the work.
var providerFailureReasons = map[string]bool{
	string(taskfailure.ReasonAgentProviderNetwork):             true,
	string(taskfailure.ReasonAgentProviderCapacityOrRateLimit): true,
	string(taskfailure.ReasonAgentProviderServerError):         true,
	string(taskfailure.ReasonAgentProviderQuotaLimit):          true,
	string(taskfailure.ReasonAgentProviderAuthOrAccess):        true,
}

// failOverRoutingTask hands a routing step to the fallback agent after its
// routing agent's run failed on a provider error and will not be retried. Best
// effort: on any doubt the failure stays visible on the issue as before.
func (s *TaskService) failOverRoutingTask(ctx context.Context, failed db.AgentTaskQueue, failureReason string) {
	if !providerFailureReasons[failureReason] || !failed.IssueID.Valid {
		return
	}
	issue, err := s.Queries.GetIssue(ctx, failed.IssueID)
	if err != nil || !issue.AssigneeID.Valid || issue.AssigneeID != failed.AgentID {
		return
	}
	ws, err := s.Queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return
	}
	var settings map[string]any
	_ = json.Unmarshal(ws.Settings, &settings)
	fallback, err := util.ParseUUID(RoutingAgentFromSettings(settings).FailoverFor(util.UUIDToString(failed.AgentID), issue.Status))
	if err != nil {
		return
	}
	agent, err := s.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: fallback, WorkspaceID: issue.WorkspaceID})
	if err != nil || agent.ArchivedAt.Valid {
		return
	}
	if _, err := s.EnqueueTaskForMention(ctx, issue, fallback, failed.TriggerCommentID, OriginDerived); err != nil {
		slog.Warn("routing failover: enqueue fallback run failed",
			"issue_id", util.UUIDToString(issue.ID), "fallback_agent_id", util.UUIDToString(fallback), "error", err)
		return
	}
	slog.Info("routing failover: fallback agent queued",
		"issue_id", util.UUIDToString(issue.ID), "failed_agent_id", util.UUIDToString(failed.AgentID),
		"fallback_agent_id", util.UUIDToString(fallback), "reason", failureReason)
}
