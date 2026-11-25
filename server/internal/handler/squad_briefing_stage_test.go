package handler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// reviewPR is an agent_review issue assigned to owner with a linked open PR.
func reviewPR(t *testing.T, owner, title string) string {
	t.Helper()
	id := dbfx.Issue(t, title, testutil.Cols{"status": "agent_review", "assignee_type": "agent", "assignee_id": owner})
	pr := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "o", "repo_name": "r",
		"pr_number": int(time.Now().UnixNano() % 1000000000), "title": "t", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": time.Now(), "pr_updated_at": time.Now(),
	})
	dbfx.InsertNoID(t, "issue_pull_request", testutil.Cols{"issue_id": id, "pull_request_id": pr},
		"issue_id = $1 AND pull_request_id = $2", id, pr)
	return id
}

// squadOwnedPR is a review PR under an epic assigned to the squad.
func squadOwnedPR(t *testing.T, owner, title string, squadID pgtype.UUID) string {
	t.Helper()
	epic := dbfx.Issue(t, title+" epic", testutil.Cols{"assignee_type": "squad", "assignee_id": util.UUIDToString(squadID)})
	pr := reviewPR(t, owner, title)
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET parent_issue_id = $2 WHERE id = $1`, pr, epic); err != nil {
		t.Fatal(err)
	}
	return pr
}

// A leader run on a full stage is told to dispatch an idle member to the queued
// PR, not to open new PR work; a stage with room says nothing.
func TestSquadLeaderBriefingRoutesIdleMemberToQueuedPR(t *testing.T) {
	ctx := context.Background()
	leaderID, _ := seededLeaderAgent(t)
	squad := seedSquadForBriefing(t, leaderID, fmt.Sprintf("Stage squad %d", time.Now().UnixNano()), "")
	rt := dbfx.Runtime(t, "Stage briefing runtime")
	idle := dbfx.Agent(t, "Idle author", rt)
	busy := dbfx.Agent(t, "Busy author", rt)
	addAgentMember(t, squad.ID, idle, "dev")
	addAgentMember(t, squad.ID, busy, "dev")
	dbfx.Task(t, busy, testutil.Cols{"status": "running", "runtime_id": rt})

	owner := dbfx.Agent(t, "Reviewer", rt)
	if got := buildSquadLeaderBriefing(ctx, testHandler.Queries, squad, true, leaderNoWorkflow, false); strings.Contains(got, "## Stage Queue") {
		t.Fatalf("no stage is full, briefing has a Stage Queue section:\n%s", got)
	}

	// Review stage, N = 2: two PRs run, the third was refused and waits.
	for _, title := range []string{"PR A", "PR B"} {
		dbfx.Task(t, owner, testutil.Cols{"issue_id": reviewPR(t, owner, title), "status": "running", "runtime_id": rt})
	}
	// PR C sits under an epic assigned to this squad. PR D is another team's:
	// a member of this squad only helped on it as a guest.
	queued := squadOwnedPR(t, owner, "PR C", squad.ID)
	foreign := reviewPR(t, owner, "PR D")
	dbfx.Task(t, idle, testutil.Cols{"issue_id": foreign, "status": "completed", "runtime_id": rt})
	for _, id := range []string{queued, foreign} {
		if err := testHandler.Queries.MarkStageWaiting(ctx, db.MarkStageWaitingParams{
			IssueID: util.MustParseUUID(id), AgentID: util.MustParseUUID(owner), TaintIds: []string{},
		}); err != nil {
			t.Fatal(err)
		}
	}

	got := buildSquadLeaderBriefing(ctx, testHandler.Queries, squad, true, leaderNoWorkflow, false)
	for _, want := range []string{
		"## Stage Queue",
		queued,
		"mention://agent/" + idle,
		"update the branch from `main`",
		"New PR work waits",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("briefing lacks %q\n%s", want, got)
		}
	}
	if i := strings.Index(got, "Idle members"); i < 0 || strings.Contains(got[i:], "mention://agent/"+busy) {
		t.Errorf("busy member listed as idle, or no idle list:\n%s", got)
	}
	if strings.Contains(got, foreign) {
		t.Errorf("briefing names a PR this squad does not own:\n%s", got)
	}
	// A squad that owns no queued PR gets no directive.
	otherLeader := dbfx.Agent(t, "Other leader", rt)
	otherSquad := seedSquadForBriefing(t, otherLeader, fmt.Sprintf("Other squad %d", time.Now().UnixNano()), "")
	if got := buildSquadLeaderBriefing(ctx, testHandler.Queries, otherSquad, true, leaderNoWorkflow, false); strings.Contains(got, "## Stage Queue") {
		t.Errorf("unrelated squad got a Stage Queue directive:\n%s", got)
	}
	if strings.Contains(got, "No push task is open") == false {
		t.Errorf("queued PR with no open push task is not marked:\n%s", got)
	}

	// Once an author's push task is open on the queued PR, say so.
	dbfx.Task(t, idle, testutil.Cols{"issue_id": queued, "status": "queued", "runtime_id": rt})
	if got := buildSquadLeaderBriefing(ctx, testHandler.Queries, squad, true, leaderNoWorkflow, false); !strings.Contains(got, "push task open") {
		t.Errorf("open push task not shown:\n%s", got)
	}
}

// The claim path hands the squad leader the directive: a leader task on a squad
// with a queued PR carries the dispatch instruction in its agent instructions.
func TestClaim_LeaderTaskOnFullStage_GetsQueuedPRDispatch(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	fx := newSquadBriefingClaimFixture(t, ctx, "Stage claim")
	rt := dbfx.Runtime(t, "Stage claim owner runtime")
	owner := dbfx.Agent(t, "Stage claim reviewer", rt)
	member := dbfx.Agent(t, "Stage claim author", rt)
	addAgentMember(t, util.MustParseUUID(fx.SquadID), member, "dev")
	for _, title := range []string{"PR A", "PR B"} {
		dbfx.Task(t, owner, testutil.Cols{"issue_id": reviewPR(t, owner, title), "status": "running", "runtime_id": rt})
	}
	queued := squadOwnedPR(t, owner, "PR C", util.MustParseUUID(fx.SquadID))
	if err := testHandler.Queries.MarkStageWaiting(ctx, db.MarkStageWaitingParams{
		IssueID: util.MustParseUUID(queued), AgentID: util.MustParseUUID(owner), TaintIds: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	want := enqueueClaimTask(t, ctx, fx, true, true)

	got, instr, _, raw := claimAgentInstructionsForTest(t, fx.RuntimeID)
	if got != want {
		t.Fatalf("claimed task id = %q, want %q: %s", got, want, raw)
	}
	for _, s := range []string{"## Stage Queue", queued, "mention://agent/" + member, "update the branch from `main`", "New PR work waits"} {
		if !strings.Contains(instr, s) {
			t.Errorf("leader instructions lack %q:\n%s", s, instr)
		}
	}
}
