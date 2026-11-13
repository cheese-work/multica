package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RunEnqueueSource identifies which kind of issue write would start an agent
// run. It is surfaced in preview responses so the UI can explain each trigger.
type RunEnqueueSource string

const (
	// RunSourceAssign covers issue creation and assignee changes — the issue
	// is being handed to an agent/squad. Parks silently on backlog.
	RunSourceAssign RunEnqueueSource = "assign"
	// RunSourceStatus covers backlog promotion and member status input on a
	// blocked issue, when the destination remains actionable.
	RunSourceStatus RunEnqueueSource = "status"
)

// IssueTriggerProbe carries the request-scoped checks WillEnqueueRun cannot
// resolve from issue state alone.
//
// CanAccessAgent is the private-agent gate. The write paths enforce it at the
// HTTP boundary (validateAssigneePair on assign, canEnqueueSquadLeader inside
// the squad enqueue helper) and therefore pass an allow-all probe so the gate
// is never duplicated or sunk into the service layer. Preview passes the real
// gate so it never leaks a private agent's readiness to a member who cannot
// see it. A nil func is treated as allow-all.
//
// IsSelfLoop reports whether promoting this issue out of backlog would be the
// calling agent re-triggering its own running task. Only the status source
// consults it. A nil func means "not a self-loop".
//
// SuppressActiveSelfAssignment reports whether a direct agent assignment is a
// trusted agent claiming ownership for itself while the target (issue, agent)
// pair already has a non-terminal task. The ownership update still succeeds;
// only the duplicate enqueue is suppressed. Cross-issue handoffs to a fresh
// target remain runnable. A nil func means "do not suppress".
type IssueTriggerProbe struct {
	CanAccessAgent               func(agent db.Agent) bool
	IsSelfLoop                   func() bool
	SuppressActiveSelfAssignment func(agentID pgtype.UUID) bool
	// CanResumeBlockedIssue permits member status input to wake the assignee.
	// Nil disables this source; callers retain invocation permission checks.
	CanResumeBlockedIssue func(agent db.Agent) bool
}

// IssueTriggerInput describes one prospective issue write in its post-write
// shape. AssigneeChanged / StatusChanged mark which fields the write touches;
// IsCreate marks a brand-new issue (no prior task to cancel, no self-loop).
type IssueTriggerInput struct {
	Issue           db.Issue
	PrevStatus      string
	IsCreate        bool
	AssigneeChanged bool
	StatusChanged   bool
	// EnteredHandoffStep marks a write that moved the issue into a project
	// workflow column with a handler (MUL-7420). The step is new work even
	// when its handler is the agent whose run made the move, so the
	// self-assignment suppression does not apply.
	EnteredHandoffStep bool
	// RecordCoalesced marks the write path: a trigger a queued run absorbs is
	// stored as a run input. The preview endpoint shares the predicate and
	// leaves it unset, so a dry run writes nothing.
	RecordCoalesced bool
}

// IssueRunTrigger is the resolved decision shared by preview and the write
// paths. AgentID is the agent that will actually run — the assignee for an
// agent issue, the squad leader for a squad issue.
type IssueRunTrigger struct {
	IssueID      pgtype.UUID
	AgentID      pgtype.UUID
	AssigneeType string
	Source       RunEnqueueSource
	BlockedEntry bool
}

func allowAllAgents(db.Agent) bool { return true }

// WillEnqueueRun is the single predicate answering "will this issue write
// start an agent run, and for whom". It is the one source of truth shared by
// the issue update / batch-update write paths and the preview endpoint,
// replacing the per-site copies that drifted (squad omitted, self-loop
// omitted, four entry points inconsistent — see MUL-3375).
//
// It is intentionally a distinct predicate from the comment trigger
// (assignee fallback comment routing): issue writes park on backlog while comments fire
// in any status. The two only share leaf readiness checks (AgentReadiness,
// the pending-task dedup), not the top-level decision.
//
// The decision must equal the real enqueue conditions so preview never claims
// a net-new run that the write path then drops. The write enqueues through
// CreateAgentTask, guarded by the (issue_id, agent_id) partial unique index
// over pending (queued/dispatched) tasks; the pending check below mirrors that
// guard, and only the status source needs it:
//   - status source (backlog → active) can re-fire against an assignee that
//     already holds a pending task (e.g. one a @mention raised while the issue
//     sat in backlog); the check keeps preview from promising a run the unique
//     index would coalesce away.
//   - assign source (create / assignee change) skips the check: a create
//     targets a fresh issue with no prior task, and a reassignment no longer
//     cancels existing tasks (#4963 / MUL-4113) — in the rare case the new
//     assignee already holds a pending task the insert is rejected by the
//     same unique index and mapped to the typed ErrDuplicatePendingTask
//     sentinel (CHE-486), so the assignee still ends up with one pending run
//     instead of a swallowed error.
func (s *IssueService) WillEnqueueRun(ctx context.Context, in IssueTriggerInput, probe IssueTriggerProbe) (IssueRunTrigger, bool) {
	issue := in.Issue
	if err := RequireActiveProject(ctx, s.Queries, issue.WorkspaceID, issue.ProjectID); err != nil {
		return IssueRunTrigger{}, false
	}
	if !issue.AssigneeType.Valid || !issue.AssigneeID.Valid {
		return IssueRunTrigger{}, false
	}
	canAccess := probe.CanAccessAgent
	if canAccess == nil {
		canAccess = allowAllAgents
	}

	// Only the fixed backlog key parks work. Leaving it for another unstarted
	// or started status can enqueue a run; custom statuses do not inherit parking.
	// Effective preserves built-in behavior and resolves custom terminal categories
	// so moving to done/closed cannot start work.
	currentStatus := issuestatus.Effective(ctx, s.Queries, issue.WorkspaceID, issue.Status)
	prevStatus := issuestatus.Effective(ctx, s.Queries, issue.WorkspaceID, in.PrevStatus)

	// Triage is stricter than the backlog parking lot: backlog defers a run,
	// Triage refuses one outright (MUL-7189 §2.3). Deciding it here is what
	// keeps the trigger PREVIEW honest — the queue door would refuse the insert
	// either way, but silently, and the preview would have promised a run.
	//
	// Leaving Triage needs no case of its own. Triage is not a status, so accept
	// clears this field and then takes the ordinary create / assign path; there
	// is no "was in triage" transition for this predicate to recognise.
	if issue.TriageState.Valid {
		return IssueRunTrigger{}, false
	}

	var source RunEnqueueSource
	blockedResume := false
	switch {
	case in.IsCreate || in.AssigneeChanged:
		// Backlog is the parking lot: assigning into backlog never starts a run.
		if currentStatus == "backlog" {
			return IssueRunTrigger{}, false
		}
		source = RunSourceAssign
	case in.StatusChanged && prevStatus == "backlog" &&
		currentStatus != "backlog" &&
		currentStatus != "done" && currentStatus != "cancelled":
		if probe.IsSelfLoop != nil && probe.IsSelfLoop() {
			return IssueRunTrigger{}, false
		}
		source = RunSourceStatus
	case in.StatusChanged && prevStatus == "blocked" &&
		currentStatus != "backlog" && currentStatus != "done" && currentStatus != "cancelled" &&
		probe.CanResumeBlockedIssue != nil:
		source = RunSourceStatus
		blockedResume = true
	default:
		return IssueRunTrigger{}, false
	}

	switch issue.AssigneeType.String {
	case "agent":
		agent, err := s.Queries.GetAgent(ctx, issue.AssigneeID)
		if err != nil || !agent.RuntimeID.Valid || agent.ArchivedAt.Valid {
			return IssueRunTrigger{}, false
		}
		if !canAccess(agent) || (blockedResume && !probe.CanResumeBlockedIssue(agent)) {
			return IssueRunTrigger{}, false
		}
		if source == RunSourceAssign && !in.IsCreate && !in.EnteredHandoffStep && probe.SuppressActiveSelfAssignment != nil &&
			probe.SuppressActiveSelfAssignment(issue.AssigneeID) {
			return IssueRunTrigger{}, false
		}
		// A step entered under the agent's own run queues behind it. A run
		// already queued covers a new step only when it works that step: one
		// stamped for a step the issue has left would be refused the moment it
		// moved the issue, so it must not suppress the run the new step needs.
		if in.EnteredHandoffStep {
			if pending, carrier := s.hasPendingStepRun(ctx, issue, issue.AssigneeID); pending {
				s.noteCoalesced(ctx, in, issue.AssigneeID, carrier)
				return IssueRunTrigger{}, false
			}
			if s.headAlreadyReviewed(ctx, issue) || s.waitingOnOpenDecision(ctx, issue) {
				return IssueRunTrigger{}, false
			}
		} else if source == RunSourceStatus {
			if pending, carrier := s.hasPendingRun(ctx, issue.ID, issue.AssigneeID); pending {
				s.noteCoalesced(ctx, in, issue.AssigneeID, carrier)
				return IssueRunTrigger{}, false
			}
		}
		return IssueRunTrigger{
			IssueID:      issue.ID,
			AgentID:      issue.AssigneeID,
			AssigneeType: "agent",
			Source:       source,
			BlockedEntry: in.EnteredHandoffStep && in.StatusChanged && currentStatus == "blocked",
		}, true

	case "squad":
		// Pair-scoped self-assignment suppression intentionally applies only to
		// direct agent ownership. Assigning a squad changes execution context
		// (leader briefing, roles, and member routing), so even when the acting
		// agent is that squad's leader it is an intentional group handoff rather
		// than a redundant direct self-claim. The status path below still uses
		// the leader's pending-task guard.
		squad, err := s.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
			ID:          issue.AssigneeID,
			WorkspaceID: issue.WorkspaceID,
		})
		if err != nil {
			return IssueRunTrigger{}, false
		}
		leader, err := s.Queries.GetAgent(ctx, squad.LeaderID)
		if err != nil {
			return IssueRunTrigger{}, false
		}
		verdict, err := AgentReadiness(ctx, s.runtimeLookup(s.Queries), leader)
		// Only a BLOCKED leader stops the handoff, as for a direct agent: a
		// waitable one (offline, provider breaker hold) queues and runs when the
		// wait ends. Skipping it here would lose the handoff for good (CHE-4408).
		if err != nil || verdict.Blocked() {
			return IssueRunTrigger{}, false
		}
		if !canAccess(leader) || (blockedResume && !probe.CanResumeBlockedIssue(leader)) {
			return IssueRunTrigger{}, false
		}
		if source == RunSourceStatus {
			if pending, carrier := s.hasPendingRun(ctx, issue.ID, squad.LeaderID); pending {
				s.noteCoalesced(ctx, in, squad.LeaderID, carrier)
				return IssueRunTrigger{}, false
			}
		}
		return IssueRunTrigger{
			IssueID:      issue.ID,
			AgentID:      squad.LeaderID,
			AssigneeType: "squad",
			Source:       source,
			BlockedEntry: in.EnteredHandoffStep && in.StatusChanged && currentStatus == "blocked",
		}, true
	}
	return IssueRunTrigger{}, false
}

// noteCoalesced stores the trigger a pending run absorbs, on the write path
// only. carrier is the run the pending check read (zero when it read none).
// A failure is logged: the write it follows has already happened.
func (s *IssueService) noteCoalesced(ctx context.Context, in IssueTriggerInput, agentID, carrier pgtype.UUID) {
	if !in.RecordCoalesced {
		return
	}
	source := "status"
	if in.AssigneeChanged || in.IsCreate {
		source = "assignment"
	}
	if err := s.TaskService.RecordCoalescedTrigger(ctx, in.Issue, agentID, pgtype.UUID{}, source, carrier); err != nil {
		slog.Error("record coalesced trigger failed", "issue_id", util.UUIDToString(in.Issue.ID), "source", source, "error", err)
	}
}

// hasPendingRun reports whether the agent already holds a queued or dispatched
// task for the issue (the (issue_id, agent_id) unique-index slot). Errors fail
// closed to "pending" so preview never over-promises a run. The second result
// is the pending run the check read (zero on error): the carrier the
// coalescing insert must lock.
func (s *IssueService) hasPendingRun(ctx context.Context, issueID, agentID pgtype.UUID) (bool, pgtype.UUID) {
	carrier, err := s.Queries.PendingCarrierForIssueAndAgent(ctx, db.PendingCarrierForIssueAndAgentParams{
		IssueID: issueID,
		AgentID: agentID,
		// Key dedup on the reviewed head so a pending run against an old HEAD
		// does not shadow a request after HEAD advanced (TEN-356).
		HeadSha: headShaText(s.TaskService.ResolveIssueReviewSHA(ctx, issueID)),
	})
	return pendingFromCarrier(carrier, err)
}

// pendingFromCarrier maps a carrier read to (pending, carrier): no row is not
// pending, any other error fails closed to pending with no carrier.
func pendingFromCarrier(carrier pgtype.UUID, err error) (bool, pgtype.UUID) {
	if err == nil {
		return true, carrier
	}
	return !errors.Is(err, pgx.ErrNoRows), pgtype.UUID{}
}

// hasPendingStepRun is hasPendingRun for a workflow handoff: a pending run
// stamped for a step the issue has left does not count. Errors fail closed to
// "pending", like hasPendingRun.
func (s *IssueService) hasPendingStepRun(ctx context.Context, issue db.Issue, agentID pgtype.UUID) (bool, pgtype.UUID) {
	carrier, err := s.Queries.PendingCarrierForIssueAndAgentAtStep(ctx, db.PendingCarrierForIssueAndAgentAtStepParams{
		IssueID: issue.ID,
		AgentID: agentID,
		HeadSha: headShaText(s.TaskService.ResolveIssueReviewSHA(ctx, issue.ID)),
		Step:    issue.Status,
	})
	return pendingFromCarrier(carrier, err)
}

// CancelStaleStepRuns cancels the agent's pending runs on the issue that are
// stamped for a step the issue has left, before the run for the step it
// entered is queued. The pending-run unique index admits one per issue and
// agent, so the stale run would otherwise block its replacement.
func (s *IssueService) CancelStaleStepRuns(ctx context.Context, issue db.Issue, agentID pgtype.UUID) {
	stale, err := s.Queries.ListStalePendingStepTasks(ctx, db.ListStalePendingStepTasksParams{
		IssueID: issue.ID,
		AgentID: agentID,
		Step:    issue.Status,
	})
	if err != nil {
		slog.Warn("workflow: list stale step runs failed", "issue_id", util.UUIDToString(issue.ID), "error", err)
		return
	}
	for _, task := range stale {
		// Conditional at the statement: a run that started after the selection
		// is in flight and stays.
		if _, err := s.TaskService.CancelTaskWithResult(ctx, task.ID, CancelTaskOptions{
			ClientSupportsDraftRestore: true,
			StaleStep:                  issue.Status,
		}); err != nil {
			slog.Warn("workflow: cancel stale step run failed",
				"issue_id", util.UUIDToString(issue.ID), "task_id", util.UUIDToString(task.ID), "error", err)
		}
	}
}

// headAlreadyReviewed reports whether the issue entered agent_review on the PR
// head its latest review verdict was for (CHE-3243). The loop only has a new
// review to give after a push. Errors read as "not reviewed": a lookup failure
// must not swallow a review.
func (s *IssueService) headAlreadyReviewed(ctx context.Context, issue db.Issue) bool {
	if issue.Status != statusAgentReview {
		return false
	}
	head := s.TaskService.ResolveIssueReviewSHA(ctx, issue.ID)
	if head == "" {
		return false
	}
	last, err := s.Queries.LastVerdictHeadSHA(ctx, issue.ID)
	return err == nil && last == head
}

// waitingOnOpenDecision reports whether the issue entered a need_decision step
// that its workflow declares a no-op while a human decision request is open
// (CHE-4976, `waits_on_open_decision`), and such a request is open. The answer
// wakes the issue, so the run would only end at once. A step without the
// declaration may have entry work and is never skipped, whoever its handler is.
// Errors read as "not waiting": a lookup failure must not swallow a run.
func (s *IssueService) waitingOnOpenDecision(ctx context.Context, issue db.Issue) bool {
	if issue.Status != statusNeedDecision {
		return false
	}
	def, _, err := issueworkflow.ForIssue(ctx, s.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil {
		return false
	}
	if step, ok := def.Step(issue.Status); !ok || !step.WaitsOnOpenDecision {
		return false
	}
	open, err := s.Queries.HasOpenDecisionRequestForIssue(ctx, db.HasOpenDecisionRequestForIssueParams{
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
	})
	return err == nil && open
}
