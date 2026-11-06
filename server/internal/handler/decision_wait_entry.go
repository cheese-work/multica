package handler

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Decision episodes on the issue write path (CHE-2591 L5 design §4.2-§4.4, §4.7).
//
// A status move into a step whose handler is human_in_charge enters a
// decision_wait episode: the write binds the asking agent's open requests and
// their intents, seals the intents, records the placement it replaces as the
// restore target and gives the issue to the person in charge. Any other status
// write while an episode is open clears its restore flag, and suspends a
// waiting episode unless the new status is closed. Delivery and restore are in
// decision_wait_delivery.go; the refusal suspensions are in decision_wait_suspend.go.

// decisionEpisodeRefusal is an entry or re-attach the write refuses (409). The
// write changes nothing.
// noOpenRequestReason is the entry refusal when B has nothing open; a
// re-attach words the same refusal for an episode that already exists.
const noOpenRequestReason = "no open decision request to wait on"

type decisionEpisodeRefusal struct{ reason string }

func (e *decisionEpisodeRefusal) Error() string { return e.reason }

func refuseEpisode(format string, args ...any) error {
	return &decisionEpisodeRefusal{reason: fmt.Sprintf(format, args...)}
}

func episodeClosedStatus(status string) bool {
	switch status {
	case "done", "cancelled", "agent_accepted", "approved":
		return true
	}
	return false
}

// applyDecisionEpisode runs inside the issue write transaction, after the
// workflow decision is verified and before the update. It rewrites params for
// an entry or re-attach, and records every other status write on the open
// episode.
func (h *Handler) applyDecisionEpisode(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, wf *workflowWrite, params *db.UpdateIssueParams) error {
	if !params.Status.Valid {
		return nil
	}
	locked, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: params.ID, WorkspaceID: workspaceID})
	if err != nil {
		return err
	}
	if params.Status.String == locked.Status {
		return nil
	}
	wait, err := q.GetOpenDecisionWait(ctx, db.GetOpenDecisionWaitParams{WorkspaceID: workspaceID, IssueID: locked.ID})
	hasWait := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, workspaceID, locked.WorkflowID, params.ProjectID)
	if err != nil {
		return err
	}
	holdsPerson := func(status string) bool {
		if def == nil {
			return false
		}
		step, ok := def.Step(status)
		return ok && step.Handler.Type == issueworkflow.HandlerHumanInCharge
	}
	// A move the platform makes by itself (edge, CI, run budget) never enters.
	entering := holdsPerson(params.Status.String) && (wf == nil || !wf.system)
	actor, _ := ctx.Value(wakeupActorKey{}).(wakeupActor)
	switch {
	case entering && !hasWait:
		return h.enterDecisionEpisode(ctx, q, locked, actor, params, holdsPerson)
	case entering && wait.Phase == "suspended":
		return h.reattachDecisionEpisode(ctx, q, locked, wait, actor, params, holdsPerson)
	case entering:
		return refuseEpisode("a decision episode already holds this issue's decision")
	case hasWait:
		_, err := q.MoveDecisionWaitStatus(ctx, db.MoveDecisionWaitStatusParams{
			Suspend: !episodeClosedStatus(params.Status.String), ID: wait.ID, WorkspaceID: workspaceID,
		})
		return err
	}
	return nil
}

// askingAgentFor is the agent whose requests an entry binds: the calling agent,
// or for a member's hand move the one agent with an open asked request.
func (h *Handler) askingAgentFor(ctx context.Context, q *db.Queries, issue db.Issue, actor wakeupActor) (db.Agent, error) {
	var askingID pgtype.UUID
	if actor.kind == "agent" {
		askingID = parseUUID(actor.id)
	} else {
		askers, err := q.ListDecisionWaitOpenAskers(ctx, db.ListDecisionWaitOpenAskersParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
		if err != nil {
			return db.Agent{}, err
		}
		if len(askers) != 1 {
			return db.Agent{}, refuseEpisode("no single agent has an open decision request on this issue")
		}
		askingID = askers[0]
	}
	asking, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: askingID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (asking.ArchivedAt.Valid || !asking.RuntimeID.Valid || asking.Kind == "system")) {
		return db.Agent{}, refuseEpisode("the asking agent is archived or has no runtime")
	}
	return asking, err
}

func (h *Handler) enterDecisionEpisode(ctx context.Context, q *db.Queries, issue db.Issue, actor wakeupActor, params *db.UpdateIssueParams, holdsPerson func(string) bool) error {
	if holdsPerson(issue.Status) {
		return refuseEpisode("this issue already waits on a person")
	}
	asking, err := h.askingAgentFor(ctx, q, issue, actor)
	if err != nil {
		return err
	}
	human, err := h.resolveHumanInCharge(ctx, q, issue, asking)
	if err != nil {
		return err
	}
	if human == nil {
		return refuseEpisode("no eligible human in charge can be asked about this issue")
	}
	humanID := parseUUID(human.UserID)
	bound, err := q.ListDecisionWaitBoundRequests(ctx, db.ListDecisionWaitBoundRequestsParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AskingAgentID: asking.ID})
	if err != nil {
		return err
	}
	requestIDs, openIDs, intentIDs, err := checkBoundRequests(bound, humanID)
	if err != nil {
		return err
	}
	// The intent rows are locked first, in id order, like the delivery path, so
	// the group-wake state read below cannot change before the episode stores it.
	intents := make([]db.DecisionIntent, 0, len(intentIDs))
	groupAck := []pgtype.UUID{}
	var latestTask pgtype.UUID
	var latestOpened pgtype.Timestamptz
	for _, id := range intentIDs {
		intent, err := q.LockDecisionIntent(ctx, db.LockDecisionIntentParams{WorkspaceID: issue.WorkspaceID, ID: id})
		if err != nil {
			return err
		}
		intents = append(intents, intent)
		if !intent.GroupWokenAt.Valid {
			groupAck = append(groupAck, intent.ID)
		}
		if !latestOpened.Valid || intent.OpenedAt.Time.After(latestOpened.Time) {
			latestOpened, latestTask = intent.OpenedAt, intent.TaskID
		}
	}
	if err := h.checkEpisodeDelivery(ctx, q, issue.WorkspaceID, requestIDs, openIDs); err != nil {
		return err
	}
	for _, intent := range intents {
		if err := q.SealDecisionIntent(ctx, db.SealDecisionIntentParams{WorkspaceID: issue.WorkspaceID, ID: intent.ID}); err != nil {
			return err
		}
	}
	askingTask := latestTask
	if task := parseUUIDOrZero(actor.task); actor.kind == "agent" && task.Valid {
		askingTask = task
	}
	var squadID pgtype.UUID
	if askingTask.Valid {
		task, err := q.GetAgentTask(ctx, askingTask)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		squadID = task.SquadID
	}
	if _, err := q.CreateDecisionWait(ctx, db.CreateDecisionWaitParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AskingAgentID: asking.ID, AskingTaskID: askingTask, ResumeSquadID: squadID,
		HumanUserID: humanID, RequestIds: requestIDs, IntentIds: intentIDs, GroupAckIntentIds: groupAck,
		RestoreStatus: issue.Status, RestoreAssigneeType: issue.AssigneeType, RestoreAssigneeID: issue.AssigneeID,
	}); err != nil {
		return err
	}
	holdForPerson(params, humanID)
	return nil
}

// reattachDecisionEpisode puts a suspended episode back to waiting when the
// asking agent's task or a member moves the issue into the step again and an
// open request is still bound. The restore target stays when the flag is true
// (the latest status write was the episode's); otherwise it is the placement
// the write replaces.
func (h *Handler) reattachDecisionEpisode(ctx context.Context, q *db.Queries, issue db.Issue, wait db.DecisionWait, actor wakeupActor, params *db.UpdateIssueParams, holdsPerson func(string) bool) error {
	if actor.kind == "agent" && parseUUID(actor.id) != wait.AskingAgentID {
		return refuseEpisode("only the asking agent or a member can put this issue back to waiting")
	}
	asking, err := h.askingAgentForWait(ctx, q, issue, wait)
	if err != nil {
		return err
	}
	human, err := h.resolveHumanInCharge(ctx, q, issue, asking)
	if err != nil {
		return err
	}
	if human == nil {
		return refuseEpisode("no eligible human in charge can be asked about this issue")
	}
	humanID := parseUUID(human.UserID)
	sorted := append([]pgtype.UUID(nil), wait.RequestIds...)
	sortUUIDs(sorted)
	bound := make([]db.DecisionRequest, 0, len(sorted))
	for _, id := range sorted {
		request, err := q.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: issue.WorkspaceID, ID: id})
		if err != nil {
			return err
		}
		bound = append(bound, request)
	}
	// The §4.3 step 3 checks run again on the stored B with R recomputed.
	requestIDs, openIDs, _, err := checkBoundRequests(bound, humanID)
	if err != nil {
		var refused *decisionEpisodeRefusal
		if errors.As(err, &refused) && refused.reason == noOpenRequestReason {
			return refuseEpisode("a decision episode already holds this issue's decision and no bound request is open")
		}
		return err
	}
	if err := h.checkEpisodeDelivery(ctx, q, issue.WorkspaceID, requestIDs, openIDs); err != nil {
		return err
	}
	arg := db.ReattachDecisionWaitParams{ID: wait.ID, WorkspaceID: issue.WorkspaceID, HumanUserID: humanID}
	// The episode's own step is never a target.
	arg.RestoreStatus, arg.RestoreAssigneeType, arg.RestoreAssigneeID = captureRestoreTarget(wait, issue, !holdsPerson(issue.Status))
	if _, err := q.ReattachDecisionWait(ctx, arg); err != nil {
		return err
	}
	holdForPerson(params, humanID)
	return nil
}

// captureRestoreTarget is the capture rule (design §4.4.6), run under the issue
// lock before an episode write that replaces the placement. A true flag means
// the latest status write was the episode's, so the target stays; otherwise,
// when capturable, the current status and assignee become the target.
func captureRestoreTarget(wait db.DecisionWait, issue db.Issue, capturable bool) (status string, assigneeType pgtype.Text, assigneeID pgtype.UUID) {
	if !wait.RestoreOnResume && capturable {
		return issue.Status, issue.AssigneeType, issue.AssigneeID
	}
	return wait.RestoreStatus, wait.RestoreAssigneeType, wait.RestoreAssigneeID
}

func (h *Handler) askingAgentForWait(ctx context.Context, q *db.Queries, issue db.Issue, wait db.DecisionWait) (db.Agent, error) {
	return h.askingAgentFor(ctx, q, issue, wakeupActor{kind: "agent", id: uuidToString(wait.AskingAgentID)})
}

// holdForPerson gives the issue to the person in charge. The assignee is set
// explicitly, so the step's handoff logic starts no run.
func holdForPerson(params *db.UpdateIssueParams, humanID pgtype.UUID) {
	params.AssigneeType = pgtype.Text{String: "member", Valid: true}
	params.AssigneeID = humanID
}

// checkBoundRequests applies the §4.3 conditions that read only the bound
// requests, and returns the request ids, the open ones and the distinct
// intent ids, each in id order.
func checkBoundRequests(bound []db.DecisionRequest, human pgtype.UUID) (requestIDs, openIDs, intentIDs []pgtype.UUID, err error) {
	requestIDs, intentIDs = []pgtype.UUID{}, []pgtype.UUID{}
	seen := map[pgtype.UUID]bool{}
	for _, request := range bound {
		requestIDs = append(requestIDs, request.ID)
		if request.DecisionIntentID.Valid && !seen[request.DecisionIntentID] {
			seen[request.DecisionIntentID] = true
			intentIDs = append(intentIDs, request.DecisionIntentID)
		}
		if request.Status == "cancelled" {
			continue
		}
		if request.HumanUserID != human {
			return nil, nil, nil, refuseEpisode("the bound requests address more than one human; only the person in charge can be waited on")
		}
		if request.Status == "answered" {
			return nil, nil, nil, refuseEpisode("a bound request's answer is already in; let it be delivered first")
		}
		if request.Status == "open" {
			openIDs = append(openIDs, request.ID)
		}
	}
	if len(openIDs) == 0 {
		return nil, nil, nil, refuseEpisode(noOpenRequestReason)
	}
	sortUUIDs(intentIDs)
	return requestIDs, openIDs, intentIDs, nil
}

// checkEpisodeDelivery refuses an entry while a retraction of a bound answer is
// still owed, or a bound request's lane cannot carry the ask.
func (h *Handler) checkEpisodeDelivery(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, requestIDs, openIDs []pgtype.UUID) error {
	retractions, err := q.ListDecisionWaitPendingRetractions(ctx, requestIDs)
	if err != nil {
		return err
	}
	if len(retractions) > 0 {
		return refuseEpisode("a retraction of a bound answer is still owed; let it be delivered first")
	}
	failures, err := q.ListDecisionWaitLaneFailures(ctx, db.ListDecisionWaitLaneFailuresParams{WorkspaceID: workspaceID, RequestIds: openIDs})
	if err != nil {
		return err
	}
	if len(failures) > 0 {
		return refuseEpisode("the direct-message lane of a bound request cannot carry the ask")
	}
	return nil
}

func sortUUIDs(ids []pgtype.UUID) {
	sort.Slice(ids, func(i, j int) bool { return uuidToString(ids[i]) < uuidToString(ids[j]) })
}

func parseUUIDOrZero(s string) pgtype.UUID {
	id, err := util.ParseUUID(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return id
}
