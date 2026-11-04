package service

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgtype"
	"log/slog"
	"slices"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
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

// WorkflowScope is the override-key prefix of the workflow a project follows,
// or "" when it has none (then only status-wide overrides apply).
func WorkflowScope(ctx context.Context, q issueworkflow.Querier, project db.Project) string {
	if !project.ID.Valid {
		return ""
	}
	def, err := issueworkflow.ForProjectRow(ctx, q, project)
	if err != nil || def == nil || !def.ID.Valid {
		return ""
	}
	return util.UUIDToString(def.ID)
}

// OverrideKey is the Overrides key for one step of one workflow.
func OverrideKey(workflowScope, status string) string { return workflowScope + ":" + status }

// AgentFor is the configured agent for a step entering status in the workflow
// with this scope, or "" when the step keeps its own handler. An override for
// that workflow's step wins, then one for the status in every workflow, then
// the primary agent.
func (c RoutingAgentConfig) AgentFor(workflowScope, status string) string {
	if c.AgentID == "" {
		return ""
	}
	if workflowScope != "" {
		if override, ok := c.Overrides[OverrideKey(workflowScope, status)]; ok {
			return override
		}
	}
	if override, ok := c.Overrides[status]; ok {
		return override
	}
	if slices.Contains(c.Statuses, status) {
		return c.AgentID
	}
	return ""
}

// FailoverFor is the fallback to run when failed, the configured agent for the
// step, could not finish its run. "" when there is none to use.
func (c RoutingAgentConfig) FailoverFor(failed, workflowScope, status string) string {
	if c.FallbackAgentID == "" || c.FallbackAgentID == failed || c.AgentFor(workflowScope, status) != failed {
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
	var project db.Project
	if issue.ProjectID.Valid {
		project, _ = s.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: issue.ProjectID, WorkspaceID: issue.WorkspaceID})
	}
	fallback, err := util.ParseUUID(RoutingAgentFromSettings(settings).FailoverFor(util.UUIDToString(failed.AgentID), WorkflowScope(ctx, s.Queries, project), issue.Status))
	if err != nil {
		return
	}
	agent, err := s.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: fallback, WorkspaceID: issue.WorkspaceID})
	if err != nil || agent.ArchivedAt.Valid {
		return
	}
	// The fallback takes the step over as its assignee. A mention would queue it
	// as an observer: the workflow brief gives step instructions and status
	// commands only to the issue's assignee. Every other field is written back
	// unchanged, as the public edit path does.
	owned, err := s.Queries.UpdateIssue(ctx, db.UpdateIssueParams{
		ID:                 issue.ID,
		SourceTaskID:       failed.ID,
		AssigneeType:       pgtype.Text{String: "agent", Valid: true},
		AssigneeID:         fallback,
		StartDate:          issue.StartDate,
		DueDate:            issue.DueDate,
		ParentIssueID:      issue.ParentIssueID,
		PreserveProject:    pgtype.Bool{Bool: true, Valid: true},
		Stage:              issue.Stage,
		DuplicateOfIssueID: issue.DuplicateOfIssueID,
	})
	if err != nil {
		slog.Warn("routing failover: hand the step to the fallback agent failed",
			"issue_id", util.UUIDToString(issue.ID), "fallback_agent_id", util.UUIDToString(fallback), "error", err)
		return
	}
	note := "Routing failover: the routing agent's run failed with a provider error (" + failureReason + "). You are the fallback agent and now own this step."
	if _, err := s.EnqueueTaskForIssueWithHandoff(ctx, owned, note, pgtype.UUID{}); err != nil {
		slog.Warn("routing failover: enqueue fallback run failed",
			"issue_id", util.UUIDToString(issue.ID), "fallback_agent_id", util.UUIDToString(fallback), "error", err)
		return
	}
	slog.Info("routing failover: fallback agent queued",
		"issue_id", util.UUIDToString(issue.ID), "failed_agent_id", util.UUIDToString(failed.AgentID),
		"fallback_agent_id", util.UUIDToString(fallback), "reason", failureReason)
}
