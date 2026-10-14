package handler

import (
	"context"
	"slices"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1418: one comment produces at most one run per agent. A comment in a new
// thread folds into the agent's queued run on the same issue instead of
// queueing a second run beside it.

type issueFoldFixture struct {
	issue   db.Issue
	agent   db.Agent
	issueID string
	agentID string
	runtime string
}

func newIssueFoldFixture(t *testing.T, name string) issueFoldFixture {
	t.Helper()
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, name+" runtime")
	agentID := dbfx.Agent(t, name+" agent", runtimeID)
	issueID := dbfx.Issue(t, name+" issue", testutil.Cols{"assignee_type": "agent", "assignee_id": agentID})
	t.Cleanup(func() { dbfx.Exec(t, "DELETE FROM agent_task_queue WHERE issue_id = $1", issueID) })
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := testHandler.Queries.GetAgent(ctx, parseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	return issueFoldFixture{issue: issue, agent: agent, issueID: issueID, agentID: agentID, runtime: runtimeID}
}

func (fx issueFoldFixture) enqueue(t *testing.T, commentID string, trigger commentAgentTrigger) DispatchStatus {
	t.Helper()
	trigger.Agent = fx.agent
	result := testHandler.enqueueCommentAgentTriggers(context.Background(), fx.issue, parseUUID(commentID), []commentAgentTrigger{trigger})
	return result[fx.agentID].status
}

func (fx issueFoldFixture) tasks(t *testing.T, status string) []db.AgentTaskQueue {
	t.Helper()
	all, err := testHandler.Queries.ListTasksByIssue(context.Background(), parseUUID(fx.issueID))
	if err != nil {
		t.Fatal(err)
	}
	var out []db.AgentTaskQueue
	for _, task := range all {
		if task.Status == status {
			out = append(out, task)
		}
	}
	return out
}

func coalescedIDs(task db.AgentTaskQueue) []string {
	return uuidsToStrings(task.CoalescedCommentIds)
}

func TestCommentInNewThreadFoldsIntoQueuedRun_CHE1418(t *testing.T) {
	fx := newIssueFoldFixture(t, "fold other thread")
	rootA := dbfx.Comment(t, fx.issueID, "thread A")
	rootB := dbfx.Comment(t, fx.issueID, "thread B")
	mention := commentAgentTrigger{Source: commentTriggerSourceMentionAgent}

	if got := fx.enqueue(t, rootA, mention); got != DispatchQueued {
		t.Fatalf("thread A: got %s, want queued", got)
	}
	if got := fx.enqueue(t, rootB, mention); got != DispatchCoalesced {
		t.Fatalf("thread B: got %s, want coalesced into the queued run", got)
	}
	queued := fx.tasks(t, "queued")
	if len(queued) != 1 {
		t.Fatalf("got %d queued runs, want 1", len(queued))
	}
	if uuidToString(queued[0].TriggerCommentID) != rootB || !slices.Contains(coalescedIDs(queued[0]), rootA) {
		t.Fatalf("run must carry both threads: trigger=%s coalesced=%v", uuidToString(queued[0].TriggerCommentID), coalescedIDs(queued[0]))
	}
}

// Seed 6 / CHE-955 shape: an assignment run is queued, then the dispatch
// comment mentions the same agent.
func TestCommentFoldsIntoQueuedAssignmentRun_CHE1418(t *testing.T) {
	fx := newIssueFoldFixture(t, "fold assignment")
	dbfx.Task(t, fx.agentID, testutil.Cols{"runtime_id": fx.runtime, "issue_id": fx.issueID, "status": "queued"})
	dispatch := dbfx.Comment(t, fx.issueID, "dispatch: please implement")

	if got := fx.enqueue(t, dispatch, commentAgentTrigger{Source: commentTriggerSourceMentionAgent}); got != DispatchCoalesced {
		t.Fatalf("got %s, want coalesced into the assignment run", got)
	}
	queued := fx.tasks(t, "queued")
	if len(queued) != 1 || uuidToString(queued[0].TriggerCommentID) != dispatch {
		t.Fatalf("want one queued run carrying the dispatch comment, got %d", len(queued))
	}
}

// CHE-982 / CHE-1047 / CHE-1048 shape: a burst of top-level comments while the
// agent is already running. One successor run collects them all.
func TestCommentBurstDuringActiveRunQueuesOneSuccessor_CHE1418(t *testing.T) {
	fx := newIssueFoldFixture(t, "fold burst")
	rootA := dbfx.Comment(t, fx.issueID, "running thread")
	dbfx.Task(t, fx.agentID, testutil.Cols{"runtime_id": fx.runtime, "issue_id": fx.issueID, "trigger_comment_id": rootA, "status": "running", "started_at": testutil.Raw("now()")})
	assignee := commentAgentTrigger{Source: commentTriggerSourceIssueAssignee}

	want := []DispatchStatus{DispatchQueued, DispatchCoalesced, DispatchCoalesced}
	var roots []string
	for i, w := range want {
		root := dbfx.Comment(t, fx.issueID, "collection comment")
		roots = append(roots, root)
		if got := fx.enqueue(t, root, assignee); got != w {
			t.Fatalf("comment %d: got %s, want %s", i+1, got, w)
		}
	}
	queued := fx.tasks(t, "queued")
	if len(queued) != 1 {
		t.Fatalf("got %d queued successors, want 1", len(queued))
	}
	covered := append(coalescedIDs(queued[0]), uuidToString(queued[0].TriggerCommentID))
	for _, root := range roots {
		if !slices.Contains(covered, root) {
			t.Fatalf("successor does not cover comment %s: %v", root, covered)
		}
	}
}

// A leader-role trigger never folds into the same agent's worker run: the
// briefing and squad context differ.
func TestCommentDoesNotFoldAcrossSquadRole_CHE1418(t *testing.T) {
	fx := newIssueFoldFixture(t, "fold role")
	squadID := dbfx.Squad(t, "fold role squad", fx.agentID)
	t.Cleanup(func() { dbfx.Exec(t, "DELETE FROM squad WHERE id = $1", squadID) })
	squad, err := testHandler.Queries.GetSquad(context.Background(), parseUUID(squadID))
	if err != nil {
		t.Fatal(err)
	}
	rootA := dbfx.Comment(t, fx.issueID, "worker thread")
	rootB := dbfx.Comment(t, fx.issueID, "leader thread")

	if got := fx.enqueue(t, rootA, commentAgentTrigger{Source: commentTriggerSourceMentionAgent}); got != DispatchQueued {
		t.Fatalf("worker run: got %s, want queued", got)
	}
	if got := fx.enqueue(t, rootB, commentAgentTrigger{Source: commentTriggerSourceMentionSquadLeader, Squad: &squad}); got != DispatchQueued {
		t.Fatalf("leader run: got %s, want its own queued run", got)
	}
	if queued := fx.tasks(t, "queued"); len(queued) != 2 {
		t.Fatalf("got %d queued runs, want worker and leader runs", len(queued))
	}
}
