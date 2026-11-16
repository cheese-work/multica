package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-6172: a weekly run guard refusal opens one spend card for the owner, an
// approval grants +10 runs and runs the refused trigger once, a rejection runs
// nothing.

const weeklyAllowType = "issue.run_guard.allow"

// weeklyCardCase trips the guard once and runs the incident hook, as production
// does on its own goroutine. The test member owns the workspace, so the card
// goes to them.
func weeklyCardCase(t *testing.T, installationID int64) (budgetCase, service.WeeklyRunGuardTripped) {
	t.Helper()
	return weeklyCardCaseFor(t, installationID, false)
}

// weeklyCardCaseFor with manyOwners leaves the issue without a requester, so the
// recipients are every workspace owner, and adds a second owner.
func weeklyCardCaseFor(t *testing.T, installationID int64, manyOwners bool) (budgetCase, service.WeeklyRunGuardTripped) {
	t.Helper()
	c, e := weeklyRefusalCase(t, installationID, manyOwners)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	return c, e
}

// weeklyRefusalCase is weeklyCardCaseFor before the incident hook runs.
func weeklyRefusalCase(t *testing.T, installationID int64, manyOwners bool) (budgetCase, service.WeeklyRunGuardTripped) {
	t.Helper()
	c := weeklyCase(t, installationID)
	var role string
	dbfx.QueryRow(t, `SELECT role FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, testUserID).Scan(&role)
	dbfx.Exec(t, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE member SET role = $3 WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, testUserID, role)
	})
	if manyOwners {
		second := seedSecurityTestOwner(t, "weekly-owner")
		dbfx.Exec(t, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, second)
		dbfx.Exec(t, `UPDATE issue SET requested_by_id = NULL, creator_type = 'agent', creator_id = $2 WHERE id = $1`, c.issueID, c.agentID)
	} else {
		dbfx.Exec(t, `UPDATE issue SET requested_by_id = $2 WHERE id = $1`, c.issueID, testUserID)
	}
	dbfx.Cleanup(t, `DELETE FROM decision_answer WHERE decision_request_id IN (SELECT id FROM decision_request WHERE issue_id = $1)`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM decision_request WHERE issue_id = $1`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM weekly_run_guard_refusal WHERE issue_id = $1`, c.issueID)
	dbfx.Cleanup(t, `DELETE FROM weekly_run_guard_allowance WHERE issue_id = $1`, c.issueID)
	seedRuns(t, c, c.agentID, 30, 2*24*time.Hour)
	return c, c.weeklyTripped(t)
}

func (c budgetCase) allowCards(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id
		WHERE r.issue_id = $1 AND r.status = 'open' AND a.action_type = $2`, c.issueID, weeklyAllowType)
}

func (c budgetCase) queuedRuns(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, c.issueID)
}

func (c budgetCase) pendingAllow(t *testing.T) pendingAction {
	t.Helper()
	var requestID, actionID string
	dbfx.QueryRow(t, `SELECT r.id::text, a.id::text FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id
		WHERE r.issue_id = $1 AND a.action_type = $2`, c.issueID, weeklyAllowType).Scan(&requestID, &actionID)
	action := pendingAction{requestID: requestID, actionID: actionID}
	action.humanID = testUserID
	return action
}

// AC1: a refusal opens one spend card and one system comment naming the trigger.
func TestWeeklyRunGuardRefusalOpensOneSpendCardAndOneComment(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, e := weeklyCardCase(t, 6172001)
	// A second refusal on the same issue adds no second card or comment.
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)

	if n := c.allowCards(t); n != 1 {
		t.Fatalf("open allow cards = %d, want 1", n)
	}
	var class, ask, owner string
	dbfx.QueryRow(t, `SELECT r.escalation_class, r.ask, r.human_user_id::text FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id
		WHERE r.issue_id = $1 AND a.action_type = $2`, c.issueID, weeklyAllowType).Scan(&class, &ask, &owner)
	if class != "spend" || owner != testUserID {
		t.Fatalf("card class = %q owner = %q, want spend for the workspace owner %s", class, owner, testUserID)
	}
	for _, want := range []string{"30", "assignment or status handoff"} {
		if !strings.Contains(ask, want) {
			t.Fatalf("card text %q lacks %q", ask, want)
		}
	}
	var body string
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'system' AND content LIKE '%assignment or status handoff%'`, c.issueID); n != 1 {
		t.Fatalf("system comments naming the trigger = %d, want 1", n)
	}
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE issue_id = $1 AND type = 'system'`, c.issueID).Scan(&body)
	if !strings.Contains(body, "?approve=") {
		t.Fatalf("comment does not link the card: %s", body)
	}
}

// AC2: approval grants +10 and runs the refused trigger once; the guard still
// refuses after the allowance is used (AC4).
func TestWeeklyRunGuardApprovalRunsTheRefusedTriggerOnceAndStaysBounded(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, _ := weeklyCardCase(t, 6172002)
	action := c.pendingAllow(t)
	action.approve(t, withVerifier(acceptingVerifier), action.claim(t, map[string]string{}), http.StatusOK)
	if state := action.state(t); state != "executed" {
		t.Fatalf("action state = %s, want executed", state)
	}
	if n := dbfx.Count(t, `SELECT coalesce(sum(runs),0) FROM weekly_run_guard_allowance WHERE issue_id = $1`, c.issueID); n != 10 {
		t.Fatalf("allowance = %d runs, want 10", n)
	}
	if n := c.queuedRuns(t); n != 1 {
		t.Fatalf("queued runs after approval = %d, want the refused trigger once", n)
	}
	// Approving twice must not run it twice.
	testHandler.sweepRunBudgetResumes(context.Background(), time.Now().Add(time.Hour))
	if n := c.queuedRuns(t); n != 1 {
		t.Fatalf("queued runs after a second resume = %d, want 1", n)
	}

	// 31 runs used of 40: nine more fit, the next is refused.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE issue_id = $1 AND status = 'queued'`, c.issueID)
	seedRuns(t, c, c.agentID, 9, time.Hour)
	if err := c.enqueue(t); err == nil || !strings.Contains(err.Error(), service.ErrWeeklyRunGuard.Error()) {
		t.Fatalf("run after the allowance is used = %v, want ErrWeeklyRunGuard", err)
	}
}

// AC2: rejecting dispatches nothing and grants nothing.
func TestWeeklyRunGuardRejectionDispatchesNothing(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, _ := weeklyCardCase(t, 6172003)
	action := c.pendingAllow(t)
	action.reject(t, testHandler, http.StatusOK)
	if n := c.queuedRuns(t); n != 0 {
		t.Fatalf("queued runs after rejection = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM weekly_run_guard_allowance WHERE issue_id = $1`, c.issueID); n != 0 {
		t.Fatalf("allowance rows after rejection = %d, want 0", n)
	}
	if err := c.enqueue(t); err == nil {
		t.Fatal("run after rejection was admitted, want the guard to refuse")
	}
}

// AC3: the budget read reports the guard while it is tripped.
func TestIssueBudgetGetReportsTrippedWeeklyGuard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, _ := weeklyCardCase(t, 6172004)
	var got struct {
		Blocking    []string `json:"blocking"`
		WeeklyRuns  int64    `json:"weekly_runs"`
		WeeklyLimit int      `json:"weekly_limit"`
	}
	request := withURLParam(newRequest(http.MethodGet, "/api/issues/"+c.issueID+"/run-budget", nil), "id", c.issueID)
	testutil.Call(t, testHandler.GetIssueRunBudget, request).Want(http.StatusOK).JSON(&got)
	if got.WeeklyRuns != 30 || got.WeeklyLimit != 30 {
		t.Fatalf("weekly guard = %d of %d, want 30 of 30", got.WeeklyRuns, got.WeeklyLimit)
	}
	if len(got.Blocking) != 1 || got.Blocking[0] != "weekly_run_guard" {
		t.Fatalf("blocking = %v, want [weekly_run_guard]", got.Blocking)
	}
}

// The default limit is unchanged: 30.
func TestWeeklyRunGuardDefaultLimitIsStillThirty(t *testing.T) {
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "")
	if got := service.WeeklyRunGuardMaxRuns(); got != 30 {
		t.Fatalf("default limit = %d, want 30", got)
	}
}

// CHE-6172 review P1: several owners get one card, and the allowance is granted
// once per refusal however many times an approval runs.
func TestWeeklyRunGuardWithManyOwnersOpensOneCardAndGrantsOnce(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, _ := weeklyCardCaseFor(t, 6172005, true)
	if n := c.allowCards(t); n != 1 {
		t.Fatalf("open allow cards with two owners = %d, want 1", n)
	}
	action := c.pendingAllow(t)
	action.approve(t, withVerifier(acceptingVerifier), action.claim(t, map[string]string{}), http.StatusOK)

	// A second approval of the same refusal, from any card, grants nothing more.
	var params []byte
	dbfx.QueryRow(t, `SELECT params FROM decision_action WHERE id = $1`, action.actionID).Scan(&params)
	second := db.DecisionAction{ID: dbid.NewV7(), WorkspaceID: parseUUID(testWorkspaceID), Params: params}
	if _, err := executeRunGuardAllowAction(context.Background(), decisionActionExecution{Queries: testHandler.Queries, Action: second}); err == nil {
		t.Fatal("second approval of one refusal was accepted, want it refused")
	}
	if n := dbfx.Count(t, `SELECT coalesce(sum(runs),0) FROM weekly_run_guard_allowance WHERE issue_id = $1`, c.issueID); n != 10 {
		t.Fatalf("allowance = %d runs, want 10 for one refusal", n)
	}
}

// CHE-6241: the reject comment is a system comment. A run it triggers is
// refused by the guard, and that refusal opens no card: a declined card stays
// declined.
func TestWeeklyRunGuardRejectCommentTriggerOpensNoNewCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, e := weeklyCardCase(t, 6241001)
	c.pendingAllow(t).reject(t, testHandler, http.StatusOK)
	var rejectComment pgtype.UUID
	dbfx.QueryRow(t, `SELECT id FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE 'Rejected%'`, c.issueID).Scan(&rejectComment)
	if !rejectComment.Valid {
		t.Fatal("no system reject comment")
	}
	e.Target.TriggerCommentID = rejectComment
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	if n := c.allowCards(t); n != 0 {
		t.Fatalf("open cards after the reject comment's own trigger = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM weekly_run_guard_refusal WHERE issue_id = $1`, c.issueID); n != 1 {
		t.Fatalf("refusal rows = %d, want 1", n)
	}
	if n := c.queuedRuns(t); n != 0 {
		t.Fatalf("queued runs = %d, want 0", n)
	}
}

// CHE-6241: after a reject, a refused trigger from a person opens exactly one
// new card, however often the same trigger is refused.
func TestWeeklyRunGuardRefusedMemberTriggerAfterRejectOpensOneCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, e := weeklyCardCase(t, 6241002)
	c.pendingAllow(t).reject(t, testHandler, http.StatusOK)
	var memberComment pgtype.UUID
	dbfx.QueryRow(t, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		SELECT id, workspace_id, 'member', $2, 'please continue', 'comment' FROM issue WHERE id = $1 RETURNING id`, c.issueID, testUserID).Scan(&memberComment)
	e.Target.TriggerCommentID = memberComment
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	if n := c.allowCards(t); n != 1 {
		t.Fatalf("open cards after a member trigger = %d, want 1", n)
	}
}

// CHE-6241: the declined card is remembered with the trigger it refused. The
// same trigger refused again, as a retry or a repeat would, opens no card.
func TestWeeklyRunGuardRejectedTriggerRefusedAgainOpensNoCard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, e := weeklyCardCase(t, 6241003)
	c.pendingAllow(t).reject(t, testHandler, http.StatusOK)
	for i := 0; i < 3; i++ {
		testHandler.onWeeklyRunGuardTripped(context.Background(), e)
	}
	if n := c.allowCards(t); n != 0 {
		t.Fatalf("open cards after the declined trigger was refused again = %d, want 0", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM weekly_run_guard_refusal WHERE issue_id = $1`, c.issueID); n != 1 {
		t.Fatalf("refusal rows = %d, want 1", n)
	}
}

// CHE-6241: a different trigger than the declined one opens one new card even
// when it is the same agent and kind, and the declined one stays quiet after.
func TestWeeklyRunGuardNewTriggerAfterRejectOpensOneCardAndDeclinedStaysQuiet(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, e := weeklyCardCase(t, 6241004)
	c.pendingAllow(t).reject(t, testHandler, http.StatusOK)
	fresh := e
	dbfx.QueryRow(t, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		SELECT id, workspace_id, 'member', $2, 'one more try', 'comment' FROM issue WHERE id = $1 RETURNING id`, c.issueID, testUserID).Scan(&fresh.Target.TriggerCommentID)
	fresh.Target.Trigger = "comment"
	testHandler.onWeeklyRunGuardTripped(context.Background(), fresh)
	testHandler.onWeeklyRunGuardTripped(context.Background(), fresh)
	if n := c.allowCards(t); n != 1 {
		t.Fatalf("open cards after a new trigger = %d, want 1", n)
	}
}

// CHE-6241: the real path. The reject comment goes through the comment trigger
// seam and the decision worker; no run is queued and no card reopens.
func TestWeeklyRunGuardRejectCommentDispatchesNothingThroughTheCommentSeam(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDecisionLease(t)
	c, _ := weeklyCardCase(t, 6241005)
	service.SetOnWeeklyRunGuardTripped(testHandler.onWeeklyRunGuardTripped)
	c.pendingAllow(t).reject(t, testHandler, http.StatusOK)
	_, runner := newDecisionRunnerFixture(t)
	decisionPass(t, runner)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	var rejectID pgtype.UUID
	dbfx.QueryRow(t, `SELECT id FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE 'Rejected%'`, c.issueID).Scan(&rejectID)
	reject, err := testHandler.Queries.GetComment(context.Background(), rejectID)
	if err != nil {
		t.Fatal(err)
	}
	var parent *db.Comment
	if reject.ParentID.Valid {
		p, err := testHandler.Queries.GetComment(context.Background(), reject.ParentID)
		if err != nil {
			t.Fatal(err)
		}
		parent = &p
	}
	testHandler.triggerTasksForComment(context.Background(), issue, reject, parent, "system", "", "", nil, commentSteerRequest{})
	time.Sleep(time.Second) // the incident hook runs on its own goroutine
	if n := c.queuedRuns(t); n != 0 {
		t.Fatalf("queued runs after the reject comment = %d, want 0", n)
	}
	if n := c.allowCards(t); n != 0 {
		t.Fatalf("open cards after the reject comment = %d, want 0", n)
	}
}

// CHE-6241: a system comment (the decision answer or a guard notice) never
// routes to an agent, even on a squad-assigned issue where an untagged comment
// falls back to the squad leader. A member comment on the same issue does, so
// the fixture reaches dispatch.
func TestSystemCommentNeverRoutesToSquadLeader(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	squadID, leaderID := newAuditSquad(t, "che6241-squad")
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM squad WHERE id = $1`, squadID) })
	var issueID string
	dbfx.QueryRow(t, `INSERT INTO issue (workspace_id, creator_type, creator_id, title, assignee_type, assignee_id, number)
		VALUES ($1, 'member', $2, 'CHE-6241 squad routing', 'squad', $3, $4) RETURNING id`,
		testWorkspaceID, testUserID, squadID, nextWorkspaceIssueNumber(t)).Scan(&issueID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)
	})
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	post := func(authorType, content string) db.Comment {
		var id pgtype.UUID
		authorID := testUserID
		if authorType == "system" {
			dbfx.QueryRow(t, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
				VALUES ($1, $2, 'system', gen_random_uuid(), $3, 'comment') RETURNING id`, issueID, testWorkspaceID, content).Scan(&id)
		} else {
			dbfx.QueryRow(t, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
				VALUES ($1, $2, $3, $4, $5, 'comment') RETURNING id`, issueID, testWorkspaceID, authorType, authorID, content).Scan(&id)
		}
		comment, err := testHandler.Queries.GetComment(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return comment
	}
	queued := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, leaderID)
	}
	testHandler.triggerTasksForComment(ctx, issue, post("system", "Rejected issue.run_guard.allow."), nil, "system", "", "", nil, commentSteerRequest{})
	if n := queued(); n != 0 {
		t.Fatalf("runs enqueued for the leader by a system comment = %d, want 0", n)
	}
	testHandler.triggerTasksForComment(ctx, issue, post("member", "please continue"), nil, "member", testUserID, testUserID, nil, commentSteerRequest{})
	if n := queued(); n != 1 {
		t.Fatalf("runs enqueued for the leader by a member comment = %d, want 1 (fixture must reach dispatch)", n)
	}
}

// CHE-6299: a refusal on a finished issue opens no card, no system comment and
// no inbox ping, but the incident row for the brief is still written.
func TestWeeklyRunGuardRefusalOnFinishedIssueOpensNothing(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentAccepted := createTestCustomStatus(t, "che6299_agent_accepted", "done")
	for i, status := range []string{"done", "merged", agentAccepted.Key} {
		t.Run(status, func(t *testing.T) {
			c, e := weeklyRefusalCase(t, int64(6299001+i), false)
			dbfx.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, c.issueID, status)
			testHandler.onWeeklyRunGuardTripped(context.Background(), e)
			testHandler.onWeeklyRunGuardTripped(context.Background(), e)

			if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, c.issueID); n != 0 {
				t.Fatalf("decision requests = %d, want 0", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM weekly_run_guard_refusal WHERE issue_id = $1`, c.issueID); n != 0 {
				t.Fatalf("refusal rows = %d, want 0", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'system'`, c.issueID); n != 0 {
				t.Fatalf("system comments = %d, want 0", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id = $1`, c.issueID); n != 0 {
				t.Fatalf("inbox items = %d, want 0", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'weekly_run_guard_tripped'`, c.issueID); n != 1 {
				t.Fatalf("incident rows = %d, want 1", n)
			}
			if n := c.queuedRuns(t); n != 0 {
				t.Fatalf("queued runs = %d, want 0", n)
			}
		})
	}
}
