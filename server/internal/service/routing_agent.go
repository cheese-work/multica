package service

import (
	"context"
	"encoding/json"
	"errors"
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

// RoutingAgentSettingsKey is the retired key inside workspace.settings. The
// setting now lives at background_agents.routing (CHE-4116); the key is kept
// only so the startup copy and a generic save can recognise the old value.
const RoutingAgentSettingsKey = "routing_agent"

// BackgroundAgentsSettingsKey and RoutingJobKey locate the setting.
const (
	BackgroundAgentsSettingsKey = "background_agents"
	RoutingJobKey               = "routing"
)

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
	if jobs, ok := settings[BackgroundAgentsSettingsKey].(map[string]any); ok {
		if b, err := json.Marshal(jobs[RoutingJobKey]); err == nil {
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
// effort: on any doubt the failure stays visible on the issue as before. It
// reports whether the fallback agent now owns the step.
func (s *TaskService) failOverRoutingTask(ctx context.Context, failed db.AgentTaskQueue, failureReason string) bool {
	if !providerFailureReasons[failureReason] || !failed.IssueID.Valid {
		return false
	}
	issue, err := s.Queries.GetIssue(ctx, failed.IssueID)
	if err != nil || !issue.AssigneeID.Valid || issue.AssigneeID != failed.AgentID {
		return false
	}
	ws, err := s.Queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return false
	}
	var settings map[string]any
	_ = json.Unmarshal(ws.Settings, &settings)
	var project db.Project
	if issue.ProjectID.Valid {
		project, _ = s.Queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: issue.ProjectID, WorkspaceID: issue.WorkspaceID})
	}
	fallback, err := util.ParseUUID(RoutingAgentFromSettings(settings).FailoverFor(util.UUIDToString(failed.AgentID), WorkflowScope(ctx, s.Queries, project), issue.Status))
	if err != nil {
		return false
	}
	agent, err := s.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: fallback, WorkspaceID: issue.WorkspaceID})
	if err != nil || agent.ArchivedAt.Valid {
		return false
	}
	// The fallback brings its owner's connected apps. Judge it by the invocation
	// policy every other trigger uses, against the human behind the run that
	// failed; a run with no human originator fails closed.
	if !CanMemberInvokeAgent(ctx, s.Queries, agent, failed.OriginatorUserID, issue.WorkspaceID) {
		slog.Warn("routing failover: the failed run's originator may not invoke the fallback agent; the issue keeps its assignee",
			"issue_id", util.UUIDToString(issue.ID), "fallback_agent_id", util.UUIDToString(fallback))
		return false
	}
	note := "Routing failover: the routing agent's run failed with a provider error (" + failureReason + "). You are the fallback agent and now own this step."
	if err := s.handStepToFallback(ctx, issue, failed, fallback, note); err != nil {
		slog.Warn("routing failover: hand the step to the fallback agent failed; the issue keeps its assignee",
			"issue_id", util.UUIDToString(issue.ID), "fallback_agent_id", util.UUIDToString(fallback), "error", err)
		return false
	}
	slog.Info("routing failover: fallback agent queued",
		"issue_id", util.UUIDToString(issue.ID), "failed_agent_id", util.UUIDToString(failed.AgentID),
		"fallback_agent_id", util.UUIDToString(fallback), "reason", failureReason)
	return true
}

var errRoutingStepMoved = errors.New("the issue changed since the routing run failed")

// handStepToFallback makes fallback the issue's assignee and queues its run in
// one transaction. The fallback must own the step: the workflow brief gives step
// instructions and status commands only to the assignee, and a mention would
// queue an observer. snapshot is the issue as read before the transaction; the
// locked row is re-checked against it, so a status, assignee or project change
// in between skips the failover, and the write carries the locked row's other
// fields. A refused or failed enqueue rolls the assignee back.
func (s *TaskService) handStepToFallback(ctx context.Context, snapshot db.Issue, failed db.AgentTaskQueue, fallback pgtype.UUID, note string) error {
	next := snapshot
	next.AssigneeType = pgtype.Text{String: "agent", Valid: true}
	next.AssigneeID = fallback
	params, _, err := s.prepareIssueTaskWithQueriesAndOverlay(readOnlyRunBudget(ctx), s.Queries, next, pgtype.UUID{}, nil, false, note, failed.OriginatorUserID, pgtype.UUID{}, pgtype.Timestamptz{}, OriginDerived, nil)
	if err != nil {
		return err
	}
	params.FreshRerun = pgtype.Bool{Bool: false, Valid: true}
	var owned db.Issue
	var task db.AgentTaskQueue
	txCtx, notifyAfterCommit := deferEnqueuedTaskHook(ctx)
	err = s.runInTx(ctx, func(q *db.Queries) error {
		locked, err := q.LockIssueForDescriptionUpdate(txCtx, db.LockIssueForDescriptionUpdateParams{ID: snapshot.ID, WorkspaceID: snapshot.WorkspaceID})
		if err != nil {
			return err
		}
		if locked.AssigneeID != failed.AgentID || locked.Status != snapshot.Status || locked.ProjectID != snapshot.ProjectID {
			return errRoutingStepMoved
		}
		owned, err = q.UpdateIssue(txCtx, db.UpdateIssueParams{
			ID:                 locked.ID,
			SourceTaskID:       failed.ID,
			AssigneeType:       next.AssigneeType,
			AssigneeID:         fallback,
			StartDate:          locked.StartDate,
			DueDate:            locked.DueDate,
			ParentIssueID:      locked.ParentIssueID,
			PreserveProject:    pgtype.Bool{Bool: true, Valid: true},
			Stage:              locked.Stage,
			DuplicateOfIssueID: locked.DuplicateOfIssueID,
		})
		if err != nil {
			return err
		}
		task, err = s.insertPreparedIssueTask(txCtx, q, owned, params, pgtype.Timestamptz{}, OriginDerived)
		return err
	})
	if err != nil {
		return err
	}
	notifyAfterCommit()
	s.PublishTaskEnqueued(ctx, task)
	if s.Bus != nil {
		s.broadcastIssueUpdated(ctx, owned, snapshot.Status)
	}
	return nil
}
