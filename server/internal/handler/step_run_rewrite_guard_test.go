package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-9605 (S1a): a queued step run (an assignment-level run, or a status
// handoff run) is never rewritten into a comment run by a side-comment merge.
// Every case claims the run through the daemon claim handler and renders the
// prompt the agent would get.

// stepRunFixture is an agent with room for several concurrent claims.
func newStepRunFixture(t *testing.T, name string) issueFoldFixture {
	t.Helper()
	fx := newIssueFoldFixture(t, name)
	dbfx.Exec(t, "UPDATE agent SET max_concurrent_tasks = 8 WHERE id = $1", fx.agentID)
	return fx
}

// seedAssignmentStepRun queues an assignment-level step run: no trigger, no wakeup.
func (fx issueFoldFixture) seedAssignmentStepRun(t *testing.T) string {
	t.Helper()
	return dbfx.Task(t, fx.agentID, testutil.Cols{"runtime_id": fx.runtime, "issue_id": fx.issueID, "status": "queued", "workflow_step": "agent_review"})
}

// seedStatusHandoffStepRun queues a status handoff step run that carries the
// handoff comment as its trigger.
func (fx issueFoldFixture) seedStatusHandoffStepRun(t *testing.T, handoffCommentID, note string) string {
	t.Helper()
	return dbfx.Task(t, fx.agentID, testutil.Cols{
		"runtime_id": fx.runtime, "issue_id": fx.issueID, "status": "queued",
		"trigger_comment_id": handoffCommentID, "trigger_kind": "status_handoff",
		"handoff_note": note, "workflow_step": "agent_review",
	})
}

func (fx issueFoldFixture) task(t *testing.T, taskID string) db.AgentTaskQueue {
	t.Helper()
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// claimAll claims every queued run of the fixture's runtime in order, completing
// each so the next can start, and returns the claimed daemon tasks by task id.
func (fx issueFoldFixture) claimAll(t *testing.T) map[string]daemon.Task {
	t.Helper()
	claimed := map[string]daemon.Task{}
	for range 8 {
		request := withURLParam(newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+fx.runtime+"/tasks/claim", nil), "runtimeId", fx.runtime)
		var response struct {
			Task *AgentTaskResponse `json:"task"`
		}
		testutil.Call(t, testHandler.ClaimTaskByRuntime, request).Want(http.StatusOK).JSON(&response)
		if response.Task == nil {
			break
		}
		raw, err := json.Marshal(response.Task)
		if err != nil {
			t.Fatal(err)
		}
		var task daemon.Task
		if err := json.Unmarshal(raw, &task); err != nil {
			t.Fatal(err)
		}
		claimed[task.ID] = task
		// The claim lets one run per (issue, agent) execute at a time.
		dbfx.Exec(t, "UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1", task.ID)
	}
	return claimed
}

// wantStepPrompt fails unless the claimed run renders the step prompt, not a comment prompt.
func wantStepPrompt(t *testing.T, task daemon.Task) {
	t.Helper()
	prompt := daemon.BuildPrompt(task, "codex")
	if !strings.Contains(prompt, "Start by running `multica issue get "+task.IssueID) || strings.Contains(prompt, "--parent "+task.TriggerCommentID) && task.TriggerKind != "status_handoff" {
		t.Fatalf("claimed step run does not render the step prompt:\n%s", prompt)
	}
	if task.TriggerCommentID != "" && task.TriggerKind != "status_handoff" {
		t.Fatalf("step run claimed with comment trigger %s and kind %q", task.TriggerCommentID, task.TriggerKind)
	}
}

// A comment in another thread while an assignment step run is queued gets its
// own run; the step run keeps its prompt (post-claim) and attribution.
func TestCrossThreadCommentKeepsQueuedAssignmentStepRun(t *testing.T) {
	fx := newStepRunFixture(t, "step cross thread")
	stepID := fx.seedAssignmentStepRun(t)
	root := dbfx.Comment(t, fx.issueID, "side question")

	if got := fx.enqueue(t, root, commentAgentTrigger{Source: commentTriggerSourceMentionAgent, AlreadyPending: true}); got != DispatchQueued {
		t.Fatalf("comment: got %s, want its own queued run", got)
	}
	if queued := fx.tasks(t, "queued"); len(queued) != 2 {
		t.Fatalf("got %d queued runs, want the step run and the comment run", len(queued))
	}
	step := fx.task(t, stepID)
	if step.TriggerCommentID.Valid || len(step.CoalescedCommentIds) != 0 {
		t.Fatalf("step run was rewritten: trigger=%v coalesced=%v", step.TriggerCommentID, coalescedIDs(step))
	}
	claimed := fx.claimAll(t)
	wantStepPrompt(t, claimed[stepID])
	var commentRuns int
	for id, task := range claimed {
		if id != stepID && task.TriggerCommentID == root {
			commentRuns++
		}
	}
	if commentRuns != 1 {
		t.Fatalf("got %d claimed comment runs for the side comment, want 1 (claimed %d runs)", commentRuns, len(claimed))
	}
}

// Same, for a status handoff step run in the handoff comment's thread.
func TestCrossThreadCommentKeepsQueuedStatusHandoffStepRun(t *testing.T) {
	fx := newStepRunFixture(t, "step cross thread handoff")
	handoff := dbfx.Comment(t, fx.issueID, "review this please")
	stepID := fx.seedStatusHandoffStepRun(t, handoff, "")
	other := dbfx.Comment(t, fx.issueID, "decision answer")

	if got := fx.enqueue(t, other, commentAgentTrigger{Source: commentTriggerSourceMentionAgent, AlreadyPending: true}); got != DispatchQueued {
		t.Fatalf("comment: got %s, want its own queued run", got)
	}
	step := fx.task(t, stepID)
	if uuidToString(step.TriggerCommentID) != handoff || step.TriggerKind.String != "status_handoff" || len(step.CoalescedCommentIds) != 0 {
		t.Fatalf("status handoff run was rewritten: trigger=%s kind=%q coalesced=%v", uuidToString(step.TriggerCommentID), step.TriggerKind.String, coalescedIDs(step))
	}
	claimed := fx.claimAll(t)
	got, ok := claimed[stepID]
	if !ok || got.TriggerKind != "status_handoff" || got.TriggerCommentID != handoff {
		t.Fatalf("claimed step run = %+v", got)
	}
	if prompt := daemon.BuildPrompt(got, "codex"); strings.Contains(prompt, other) {
		t.Fatalf("step prompt answers the side comment:\n%s", prompt)
	}
}

// A comment in the handoff thread while the status handoff step run is queued
// keeps the run intact and is replayed after the run.
func TestSameThreadCommentKeepsQueuedStatusHandoffStepRun(t *testing.T) {
	fx := newStepRunFixture(t, "step same thread")
	handoff := dbfx.Comment(t, fx.issueID, "review this please")
	stepID := fx.seedStatusHandoffStepRun(t, handoff, "scope: only the login flow")
	reply := dbfx.Comment(t, fx.issueID, "one more thing", testutil.Cols{"parent_id": handoff})

	got := fx.enqueue(t, reply, commentAgentTrigger{Source: commentTriggerSourceMentionAgent, AlreadyPending: true})
	if got != DispatchDeferred {
		t.Fatalf("same-thread comment: got %s, want deferred behind the step run", got)
	}
	step := fx.task(t, stepID)
	if uuidToString(step.TriggerCommentID) != handoff || step.TriggerKind.String != "status_handoff" || step.HandoffNote.String != "scope: only the login flow" {
		t.Fatalf("status handoff run was rewritten: trigger=%s kind=%q note=%q", uuidToString(step.TriggerCommentID), step.TriggerKind.String, step.HandoffNote.String)
	}
	if slices.Contains(coalescedIDs(step), reply) {
		t.Fatalf("step run claims to carry the reply in its coalesced set; the step prompt never renders it: %v", coalescedIDs(step))
	}
	claimed := fx.claimAll(t)
	if got := claimed[stepID]; got.TriggerKind != "status_handoff" || got.HandoffNote != "scope: only the login flow" {
		t.Fatalf("claimed step run lost its handoff: %+v", got)
	}
	// The reply stays reachable for the completion replay: it is newer than the
	// run and not in the run's delivered receipt.
	var delivered []string
	row := fx.task(t, stepID)
	delivered = uuidsToStrings(row.DeliveredCommentIds)
	if slices.Contains(delivered, reply) {
		t.Fatalf("reply was recorded as delivered by the step claim: %v", delivered)
	}
	// After the run completes, the replay queues a follow-up run for the reply.
	completed := fx.task(t, stepID)
	testHandler.reconcileCommentsOnCompletion(context.Background(), &completed)
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND (trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids))`, fx.issueID, fx.agentID, reply); n != 1 {
		t.Fatalf("got %d follow-up runs for the reply after the step run, want 1", n)
	}
}

// The post-insert sibling collapse never cancels a fresh comment run into a
// queued step run.
func TestSiblingCollapseSkipsQueuedStepRun(t *testing.T) {
	fx := newStepRunFixture(t, "step collapse")
	stepID := fx.seedAssignmentStepRun(t)
	root := dbfx.Comment(t, fx.issueID, "side question")
	commentID := dbfx.Task(t, fx.agentID, testutil.Cols{"runtime_id": fx.runtime, "issue_id": fx.issueID, "status": "queued", "trigger_comment_id": root})
	comment := fx.task(t, commentID)

	if testHandler.collapseIntoQueuedSibling(context.Background(), fx.issue, comment) {
		t.Fatal("comment run was collapsed into a queued step run")
	}
	if got := fx.task(t, commentID); got.Status != "queued" {
		t.Fatalf("comment run status = %s, want queued", got.Status)
	}
	if step := fx.task(t, stepID); step.TriggerCommentID.Valid || len(step.CoalescedCommentIds) != 0 {
		t.Fatalf("step run was rewritten: trigger=%v coalesced=%v", step.TriggerCommentID, coalescedIDs(step))
	}
}

// A squad leader's step run is protected by the same row predicate.
func TestCrossThreadCommentKeepsQueuedLeaderStepRun(t *testing.T) {
	fx := newStepRunFixture(t, "step leader")
	squadID := dbfx.Squad(t, "step leader squad", fx.agentID)
	t.Cleanup(func() { dbfx.Exec(t, "DELETE FROM squad WHERE id = $1", squadID) })
	squad, err := testHandler.Queries.GetSquad(context.Background(), parseUUID(squadID))
	if err != nil {
		t.Fatal(err)
	}
	stepID := dbfx.Task(t, fx.agentID, testutil.Cols{"runtime_id": fx.runtime, "issue_id": fx.issueID, "status": "queued", "is_leader_task": true, "squad_id": squadID, "workflow_step": "agent_review"})
	root := dbfx.Comment(t, fx.issueID, "decision answer")

	got := fx.enqueue(t, root, commentAgentTrigger{Source: commentTriggerSourceMentionSquadLeader, Squad: &squad, AlreadyPending: true})
	if got != DispatchQueued {
		t.Fatalf("leader comment: got %s, want its own queued run", got)
	}
	if step := fx.task(t, stepID); step.TriggerCommentID.Valid {
		t.Fatalf("leader step run was rewritten: trigger=%s", uuidToString(step.TriggerCommentID))
	}
}

// A decision answer gets its own comment run when the recipient's only queued
// run is a step run; a running step run still takes planned input.
func TestDecisionRecipientSkipsQueuedStepRun(t *testing.T) {
	fx := newStepRunFixture(t, "step decision")
	stepID := fx.seedAssignmentStepRun(t)
	snapshot := &decisionWakeSnapshot{issue: fx.issue}
	if err := testHandler.loadDecisionRecipient(context.Background(), testHandler.Queries, snapshot, fx.issue.AssigneeID); err != nil {
		t.Fatal(err)
	}
	if snapshot.task.ID.Valid {
		t.Fatalf("decision recipient picked queued step run %s", uuidToString(snapshot.task.ID))
	}

	dbfx.Exec(t, "UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1", stepID)
	snapshot = &decisionWakeSnapshot{issue: fx.issue}
	if err := testHandler.loadDecisionRecipient(context.Background(), testHandler.Queries, snapshot, fx.issue.AssigneeID); err != nil {
		t.Fatal(err)
	}
	if uuidToString(snapshot.task.ID) != stepID {
		t.Fatalf("decision recipient = %s, want the running step run %s", uuidToString(snapshot.task.ID), stepID)
	}
}

// Entering a step is covered only by a step run for that step; a comment run
// does not stand in for it.
func TestPendingStepCountIgnoresCommentRuns(t *testing.T) {
	fx := newStepRunFixture(t, "step count")
	root := dbfx.Comment(t, fx.issueID, "side question")
	dbfx.Task(t, fx.agentID, testutil.Cols{"runtime_id": fx.runtime, "issue_id": fx.issueID, "status": "queued", "trigger_comment_id": root})
	params := db.HasPendingTaskForIssueAndAgentAtStepParams{IssueID: fx.issue.ID, AgentID: fx.agent.ID, Step: "agent_review"}

	has, err := testHandler.Queries.HasPendingTaskForIssueAndAgentAtStep(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("a comment run was counted as the run for the entered step")
	}
	fx.seedAssignmentStepRun(t)
	has, err = testHandler.Queries.HasPendingTaskForIssueAndAgentAtStep(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("the step run was not counted")
	}
}

// The budget-resume fold keeps a queued step run and holds the refusal.
func TestResumeSurvivorKeepsQueuedStepRun(t *testing.T) {
	fx := newStepRunFixture(t, "step resume")
	stepID := fx.seedAssignmentStepRun(t)
	root := dbfx.Comment(t, fx.issueID, "refused comment")
	refusal := db.RunBudgetRefusal{TriggerCommentID: parseUUID(root), AgentID: fx.agent.ID, Trigger: "mention"}
	prep := testHandler.prepareBind(context.Background(), refusal, fx.issue)
	if prep == nil {
		t.Fatal("no bind prep for the stored comment")
	}
	var held error
	err := testHandler.underIssueLock(context.Background(), fx.issue.ID, func(q *db.Queries) error {
		_, held = testHandler.bindSurvivor(context.Background(), q, refusal, parseUUID(stepID), prep)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if held != errResumeSurvivorHeld {
		t.Fatalf("bindSurvivor = %v, want errResumeSurvivorHeld", held)
	}
	if step := fx.task(t, stepID); step.TriggerCommentID.Valid || len(step.CoalescedCommentIds) != 0 {
		t.Fatalf("step run was rewritten: trigger=%v coalesced=%v", step.TriggerCommentID, coalescedIDs(step))
	}
}

var _ = pgtype.UUID{}
