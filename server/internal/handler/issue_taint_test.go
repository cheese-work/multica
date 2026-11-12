package handler

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Issue-level taint, the member clear action and the resolver override
// (CHE-2216 plan 3.7, 3.1, slice B5b; T9). A tainted dispatch or comment taints
// the issue; until a member clears it, no run on the issue resolves elevated,
// even a clean task holding a live grant.

func issueTaintOf(t *testing.T, issueID string) []string {
	t.Helper()
	ids := []string{}
	dbfx.QueryRow(t, `SELECT COALESCE((SELECT taint_ids FROM issue_taint WHERE issue_id = $1), '{}')`, issueID).Scan(&ids)
	return ids
}

func postClearIssueTaint(t *testing.T, issueID string, body map[string]any, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("POST", "/api/issues/"+issueID+"/taint/clear", body), "id", issueID)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, testHandler.ClearIssueTaint, req)
}

// claimCleanTask enqueues a plain task (no taint source) on the issue and
// claims it, returning the claim's profile.
func claimCleanTask(t *testing.T, issueID, agentID, runtimeID string) (profile string, taint []string) {
	t.Helper()
	// One pending task per issue and agent: finish the previous claim first.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE issue_id = $1 AND status IN ('dispatched', 'running')`, issueID)
	createDispatchedClaimFixtureTask(t, context.Background(), agentID, runtimeID, issueID, "120 seconds", false)
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
	var resp struct {
		Task *struct {
			Profile  string   `json:"profile"`
			TaintIDs []string `json:"taint_ids"`
		} `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&resp)
	if resp.Task == nil {
		t.Fatal("claim returned no task")
	}
	return resp.Task.Profile, resp.Task.TaintIDs
}

func TestTaintedDispatchTaintsTheIssueAndOverridesAGrant(t *testing.T) {
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Issue taint runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Issue taint agent")
	insertGrant(t, issueID, testWorkspaceID, []string{"repo_write:1234"}, "1 minute", "1 hour", false)

	dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "runtime_id": runtimeID, "issue_id": issueID, "taint_ids": []string{"t1"}})

	if got := issueTaintOf(t, issueID); !reflect.DeepEqual(got, []string{"t1"}) {
		t.Fatalf("issue taint = %v, want [t1]", got)
	}
	// A clean task of its own: no taint IDs, a live grant, still restricted.
	profile, taint := claimCleanTask(t, issueID, agentID, runtimeID)
	if profile != "restricted" || len(taint) != 0 {
		t.Fatalf("claim profile = %q taint = %v, want restricted with no task taint", profile, taint)
	}
}

func TestTaintedCommentTaintsTheIssue(t *testing.T) {
	agentID := seededReadyAgentID(t)
	issueID := dbfx.Issue(t, "issue taint by comment")
	tainted := taintedTask(t, agentID, "t7")
	dbfx.Comment(t, issueID, "hostile relay", testutil.Cols{"source_task_id": tainted})
	if got := issueTaintOf(t, issueID); !reflect.DeepEqual(got, []string{"t7"}) {
		t.Fatalf("issue taint = %v, want [t7]", got)
	}
	clean := dbfx.Issue(t, "issue taint clean")
	dbfx.Comment(t, clean, "member note")
	if got := issueTaintOf(t, clean); len(got) != 0 {
		t.Fatalf("clean issue taint = %v, want none", got)
	}
}

func TestMemberClearsIssueTaintAndRecordsIt(t *testing.T) {
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Issue clear runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Issue clear agent")
	insertGrant(t, issueID, testWorkspaceID, []string{"repo_write:1234"}, "1 minute", "1 hour", false)
	tainted := dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "runtime_id": runtimeID, "issue_id": issueID, "taint_ids": []string{"t1", "t2"}})

	var got struct {
		Cleared   []string `json:"cleared"`
		Remaining []string `json:"remaining"`
	}
	postClearIssueTaint(t, issueID, map[string]any{"taint_ids": []string{"t1"}}, nil).Want(http.StatusOK).JSON(&got)
	if !reflect.DeepEqual(got.Cleared, []string{"t1"}) || !reflect.DeepEqual(got.Remaining, []string{"t2"}) {
		t.Fatalf("response = %+v, want cleared [t1] remaining [t2]", got)
	}
	if profile, _ := claimCleanTask(t, issueID, agentID, runtimeID); profile != "restricted" {
		t.Fatalf("profile = %q with t2 still on the issue, want restricted", profile)
	}

	// No IDs named: clears everything left.
	postClearIssueTaint(t, issueID, map[string]any{}, nil).Want(http.StatusOK).JSON(&got)
	if !reflect.DeepEqual(got.Cleared, []string{"t2"}) || len(got.Remaining) != 0 {
		t.Fatalf("response = %+v, want cleared [t2] and nothing remaining", got)
	}

	var memberID string
	dbfx.QueryRow(t, `SELECT id FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, testUserID).Scan(&memberID)
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_taint_clear WHERE issue_id = $1 AND workspace_id = $2 AND cleared_by_member_id = $3 AND taint_ids = ARRAY['t1'] AND cleared_at IS NOT NULL`, issueID, testWorkspaceID, memberID); n != 1 {
		t.Fatalf("clear rows for [t1] = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_taint_clear WHERE issue_id = $1 AND taint_ids = ARRAY['t2']`, issueID); n != 1 {
		t.Fatalf("clear rows for [t2] = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'issue_taint_cleared' AND actor_type = 'member' AND actor_id = $2`, issueID, testUserID); n != 2 {
		t.Fatalf("issue_taint_cleared activity rows = %d, want 2", n)
	}

	// The issue is clean again: a new clean task elevates.
	if profile, _ := claimCleanTask(t, issueID, agentID, runtimeID); profile != "elevated" {
		t.Fatalf("profile = %q after the clear, want elevated", profile)
	}
	// A cleared ID that reaches a new task through inheritance taints that task,
	// never the issue again.
	child := dbfx.Task(t, agentID, testutil.Cols{"status": "completed", "runtime_id": runtimeID, "issue_id": issueID, "parent_task_id": tainted})
	if got := taintOf(t, "agent_task_queue", child); !reflect.DeepEqual(got, []string{"t1", "t2"}) {
		t.Fatalf("child task taint = %v, want [t1 t2]", got)
	}
	if got := issueTaintOf(t, issueID); len(got) != 0 {
		t.Fatalf("issue taint = %v after inheritance of cleared IDs, want none", got)
	}
}

func TestClearIssueTaintRefusals(t *testing.T) {
	agentID := seededReadyAgentID(t)
	issueID := dbfx.Issue(t, "issue clear refusals")
	dbfx.Comment(t, issueID, "relay", testutil.Cols{"source_task_id": taintedTask(t, agentID, "t1")})
	taskID := createHandlerTestTaskForAgent(t, agentID)

	agentCredentials := map[string]func(*http.Request){
		"member-owned agent with task headers": func(r *http.Request) {
			r.Header.Set("X-Agent-ID", agentID)
			r.Header.Set("X-Task-ID", taskID)
		},
		"task token": func(r *http.Request) {
			r.Header.Set("X-Actor-Source", "task_token")
			r.Header.Set("X-Agent-ID", agentID)
		},
		"cloud pat": func(r *http.Request) { r.Header.Set("X-Actor-Source", "cloud_pat") },
	}
	for name, tweak := range agentCredentials {
		postClearIssueTaint(t, issueID, map[string]any{}, tweak).Want(http.StatusForbidden)
		if got := issueTaintOf(t, issueID); !reflect.DeepEqual(got, []string{"t1"}) {
			t.Fatalf("%s: refused clear left taint %v, want [t1]", name, got)
		}
	}

	plain := dbfx.User(t, "Plain Taint Member", "plain-taint-member@example.com")
	dbfx.Member(t, testWorkspaceID, plain, "member")
	postClearIssueTaint(t, issueID, map[string]any{}, func(r *http.Request) { r.Header.Set("X-User-ID", plain) }).Want(http.StatusForbidden)

	postClearIssueTaint(t, issueID, map[string]any{"taint_ids": []string{"nope"}}, nil).Want(http.StatusBadRequest)
	postClearIssueTaint(t, issueID, map[string]any{"unknown": 1}, nil).Want(http.StatusBadRequest)
	if got := issueTaintOf(t, issueID); !reflect.DeepEqual(got, []string{"t1"}) {
		t.Fatalf("bad requests left taint %v, want [t1]", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_taint_clear WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("refused clears left %d audit rows", n)
	}

	postClearIssueTaint(t, dbfx.Issue(t, "issue never tainted"), map[string]any{}, nil).Want(http.StatusConflict)
}

func TestIssueTaintArrivingAfterElevatedClaimRefusesAndFences(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	agentID := seededReadyAgentID(t)
	dbfx.Comment(t, f.issueID, "relay", testutil.Cols{"source_task_id": taintedTask(t, agentID, "t9")})

	// Refused at once, before any sweep; the task itself carries no taint.
	f.authorize(t, 1, "repo_write:1234", hashA).Want(http.StatusForbidden)
	if n := dbfx.Count(t, `SELECT count(*) FROM elevated_op WHERE task_id = $1`, f.taskID); n != 0 {
		t.Fatalf("authorized operations = %d, want 0", n)
	}
	fenceNow(t)
	if state, epoch := f.state(t); state == "elevated" || epoch != 2 {
		t.Fatalf("after sweep state = %q epoch = %d, want fenced at epoch 2", state, epoch)
	}
}

func TestClearingIssueTaintBeforeAnySweepStillFencesTheOriginalTask(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	agentID := seededReadyAgentID(t)
	dbfx.Comment(t, f.issueID, "relay", testutil.Cols{"source_task_id": taintedTask(t, agentID, "t9")})

	// A member clears the taint before the 10-second sweep ran.
	postClearIssueTaint(t, f.issueID, map[string]any{}, nil).Want(http.StatusOK)
	if got := issueTaintOf(t, f.issueID); len(got) != 0 {
		t.Fatalf("issue taint = %v after the clear, want none", got)
	}

	// The original task never regains authority: the clear itself fenced it.
	if state, epoch := f.state(t); state == "elevated" || epoch != 2 {
		t.Fatalf("after the clear state = %q epoch = %d, want fenced at epoch 2", state, epoch)
	}
	f.authorize(t, 1, "repo_write:1234", hashA).Want(http.StatusForbidden)
	f.authorize(t, 2, "repo_write:1234", hashA).Want(http.StatusForbidden)
	fenceNow(t)
	if state, _ := f.state(t); state == "elevated" {
		t.Fatal("task is elevated again after the sweep")
	}
}

// waitForBlockedIssueShareLock polls until a backend waits on the issue row
// lock taken by a FOR SHARE read (an authorization reached its first lock).
func waitForBlockedIssueShareLock(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if dbfx.Count(t, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query ILIKE '%FROM issue%FOR SHARE%'`) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("authorization never waited on the issue row lock")
}

// A taint writer holds the issue row, updates the task row and, through its
// trigger, locks the issue again. Authorization takes the issue first too, so
// it waits before it holds any task lock and the two cannot wait on each other.
func TestTaintUpdateAndAuthorizeShareOneLockOrder(t *testing.T) {
	ctx := context.Background()
	f := claimElevated(t, "repo_write:1234")

	writer, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `SELECT 1 FROM issue WHERE id = (SELECT issue_id FROM agent_task_queue WHERE id = $1) FOR NO KEY UPDATE`, f.taskID); err != nil {
		t.Fatalf("writer locks issue: %v", err)
	}

	authorized := make(chan *testutil.Response, 1)
	go func() { authorized <- f.authorize(t, 1, "repo_write:1234", hashA) }()
	waitForBlockedIssueShareLock(t)

	if _, err := writer.Exec(ctx, `UPDATE agent_task_queue SET taint_ids = ARRAY['tz'] WHERE id = $1`, f.taskID); err != nil {
		t.Fatalf("writer update: %v", err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	(<-authorized).Want(http.StatusForbidden)
	if n := dbfx.Count(t, `SELECT count(*) FROM elevated_op WHERE task_id = $1`, f.taskID); n != 0 {
		t.Fatalf("authorized operations = %d, want 0", n)
	}
}

// A comment planned onto a claimed run adds the comment's taint to the run
// row, and the run's taint trigger locks the issue. Authorization holds the
// issue first and then asks for the run row. The registration has to take the
// issue first as well, or each waits for what the other holds and Postgres
// aborts one of them.
func TestPlannedCommentRegistrationAndAuthorizeShareOneLockOrder(t *testing.T) {
	ctx := context.Background()
	f := claimElevated(t, "repo_write:1234")
	var agentID string
	dbfx.QueryRow(t, `SELECT agent_id FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&agentID)
	commentID := dbfx.Comment(t, f.issueID, "hostile relay", testutil.Cols{"source_task_id": taintedTask(t, agentID, "tp")})
	dbfx.Exec(t, `UPDATE agent_task_queue SET comment_thread_id = $2 WHERE id = $1`, f.taskID, commentID)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(f.issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}

	// The gate holds the elevation row, so authorization stops there with the
	// issue row already locked.
	gate, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin gate: %v", err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM task_elevation WHERE task_id = $1 FOR UPDATE`, f.taskID); err != nil {
		t.Fatalf("gate locks the elevation row: %v", err)
	}
	authorized := make(chan *testutil.Response, 1)
	go func() { authorized <- f.authorize(t, 1, "repo_write:1234", hashA) }()
	waitForLockWaiters(t, 1)

	type registered struct {
		ok  bool
		err error
	}
	registration := make(chan registered, 1)
	go func() {
		ok, err := testHandler.registerPlannedCommentForActiveTask(ctx, issue, parseUUID(agentID), parseUUID(commentID), pgtype.Text{})
		registration <- registered{ok, err}
	}()
	// Authorization waits on the elevation row; the registration waits on the
	// issue row (taken first) or, unfixed, on it through the run's trigger.
	waitForLockWaiters(t, 2)
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("gate commit: %v", err)
	}

	if got := (<-authorized).Code; got != http.StatusOK && got != http.StatusForbidden {
		t.Fatalf("authorization status = %d, want 200 or 403 (a deadlock aborts it)", got)
	}
	if r := <-registration; r.err != nil || !r.ok {
		t.Fatalf("registration = %v, %v, want registered without error", r.ok, r.err)
	}
}

// Workspace teardown detaches the children of a task batch. A child's parent_task_id
// is an input of the run's inheritance trigger, so detaching it can add the taint of
// the lineage that remains (here a retry source that was tainted after the child
// inherited), and the run's taint trigger then locks the child's issue. That issue
// can belong to a neighbour workspace the teardown never locked. Authorization holds
// the issue first and then asks for the run row. The detach has to take the issue
// before it touches the run row, or each waits for what the other holds and
// Postgres aborts one of them.
func TestWorkspaceTeardownDetachAndAuthorizeShareOneLockOrder(t *testing.T) {
	ctx := context.Background()
	f := claimElevated(t, "repo_write:1234")
	victim := newWorkspaceDeletePathFixture(t, "detach-lock-order")

	var agentID string
	dbfx.QueryRow(t, `SELECT agent_id FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&agentID)
	parent := insertDeletePathTask(t, victim.victimAgent, victim.neighbourIssue, victim.neighbourRuntime)
	retrySource := taintedTask(t, agentID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET parent_task_id = $2, retry_of_task_id = $3 WHERE id = $1`, f.taskID, parent, retrySource)
	// The retry source gains taint after the child inherited from it.
	dbfx.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['tr'] WHERE id = $1`, retrySource)
	if got := taintOf(t, "agent_task_queue", f.taskID); len(got) != 0 {
		t.Fatalf("child taint before the detach = %v, want none", got)
	}

	// The gate holds the elevation row, so authorization stops there with the
	// issue row already locked.
	gate, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin gate: %v", err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM task_elevation WHERE task_id = $1 FOR UPDATE`, f.taskID); err != nil {
		t.Fatalf("gate locks the elevation row: %v", err)
	}
	authorized := make(chan *testutil.Response, 1)
	go func() { authorized <- f.authorize(t, 1, "repo_write:1234", hashA) }()
	waitForLockWaiters(t, 1)

	teardown := make(chan error, 1)
	go func() {
		tx, err := testHandler.TxStarter.Begin(ctx)
		if err != nil {
			teardown <- err
			return
		}
		defer tx.Rollback(ctx)
		qtx := testHandler.Queries.WithTx(tx)
		if err := qtx.SetWorkspaceTeardownMode(ctx); err != nil {
			teardown <- err
			return
		}
		if err := lockWorkspaceTaskOwners(ctx, qtx, parseUUID(victim.victimID)); err != nil {
			teardown <- err
			return
		}
		if err := deleteWorkspaceTasks(ctx, qtx, parseUUID(victim.victimID), nil); err != nil {
			teardown <- err
			return
		}
		teardown <- tx.Commit(ctx)
	}()
	// Authorization waits on the elevation row; the teardown waits on the issue
	// row (taken first) or, unfixed, on it through the run's trigger.
	waitForLockWaiters(t, 2)
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("gate commit: %v", err)
	}

	if got := (<-authorized).Code; got != http.StatusOK && got != http.StatusForbidden {
		t.Fatalf("authorization status = %d, want 200 or 403 (a deadlock aborts it)", got)
	}
	if err := <-teardown; err != nil {
		t.Fatalf("teardown = %v, want it to commit (a deadlock aborts it)", err)
	}
	var detached bool
	dbfx.QueryRow(t, `SELECT parent_task_id IS NULL FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&detached)
	if !detached {
		t.Fatal("the child still names the deleted parent")
	}
}

// The task page of a workspace teardown can hold a parent and its child at once.
// Locking that page FOR UPDATE before the child's issue inverts the order of an
// authorization, which holds the child's issue and then asks for the child run:
// the page holds the run and waits for the issue. The sweep has to lock the
// issues of the page before it locks the run rows.
func TestWorkspaceTeardownTaskPageAndAuthorizeShareOneLockOrder(t *testing.T) {
	ctx := context.Background()
	f := claimElevated(t, "repo_write:1234")
	victim := newWorkspaceDeletePathFixture(t, "page-lock-order")

	parent := insertDeletePathTask(t, victim.victimAgent, victim.victimIssue, victim.victimRuntime)
	pointRunAtParentWithStaleLineage(t, f, parent)
	// The child is swept in the same page as its parent; its issue stays outside
	// the workspace, so the owner-row locks do not cover it.
	dbfx.Exec(t, `UPDATE agent_task_queue SET agent_id = $2 WHERE id = $1`, f.taskID, victim.victimAgent)

	gate, authorized := holdAuthorizationAtElevationRow(t, f)
	teardown := make(chan error, 1)
	go func() {
		tx, err := testHandler.TxStarter.Begin(ctx)
		if err != nil {
			teardown <- err
			return
		}
		defer tx.Rollback(ctx)
		qtx := testHandler.Queries.WithTx(tx)
		if err := qtx.SetWorkspaceTeardownMode(ctx); err != nil {
			teardown <- err
			return
		}
		if err := lockWorkspaceTaskOwners(ctx, qtx, parseUUID(victim.victimID)); err != nil {
			teardown <- err
			return
		}
		if err := deleteWorkspaceTasks(ctx, qtx, parseUUID(victim.victimID), nil); err != nil {
			teardown <- err
			return
		}
		teardown <- tx.Commit(ctx)
	}()
	waitForLockWaiters(t, 2)
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("gate commit: %v", err)
	}
	if got := (<-authorized).Code; got != http.StatusOK && got != http.StatusForbidden {
		t.Fatalf("authorization status = %d, want 200 or 403 (a deadlock aborts it)", got)
	}
	if err := <-teardown; err != nil {
		t.Fatalf("teardown = %v, want it to commit (a deadlock aborts it)", err)
	}
	if left := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id = ANY($1::uuid[])`, []string{parent, f.taskID}); left != 0 {
		t.Fatalf("%d of the parent and child runs survived the sweep", left)
	}
}

// pointRunAtCommentWithStaleLineage makes the elevated run name commentID as its
// trigger and a retry source whose taint arrives only afterwards. Nulling the
// trigger (the FK's ON DELETE SET NULL) then recomputes the run's taint from the
// lineage that remains, and the run's taint trigger locks the run's issue.
func pointRunAtCommentWithStaleLineage(t *testing.T, f elevatedFixture, commentID string) {
	t.Helper()
	var agentID string
	dbfx.QueryRow(t, `SELECT agent_id FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&agentID)
	retrySource := taintedTask(t, agentID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET trigger_comment_id = $2, retry_of_task_id = $3 WHERE id = $1`, f.taskID, commentID, retrySource)
	dbfx.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['tr'] WHERE id = $1`, retrySource)
	if got := taintOf(t, "agent_task_queue", f.taskID); len(got) != 0 {
		t.Fatalf("run taint before the delete = %v, want none", got)
	}
}

// holdAuthorizationAtElevationRow starts an authorization that has the run's
// issue locked and waits on the elevation row the returned gate holds.
func holdAuthorizationAtElevationRow(t *testing.T, f elevatedFixture) (gate pgx.Tx, authorized chan *testutil.Response) {
	t.Helper()
	ctx := context.Background()
	gate, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin gate: %v", err)
	}
	t.Cleanup(func() { gate.Rollback(ctx) })
	if _, err := gate.Exec(ctx, `SELECT 1 FROM task_elevation WHERE task_id = $1 FOR UPDATE`, f.taskID); err != nil {
		t.Fatalf("gate locks the elevation row: %v", err)
	}
	authorized = make(chan *testutil.Response, 1)
	go func() { authorized <- f.authorize(t, 1, "repo_write:1234", hashA) }()
	waitForLockWaiters(t, 1)
	return gate, authorized
}

// A comment delete nulls trigger_comment_id on every run that names the comment,
// and that update runs the run's taint trigger. The run's issue can differ from
// the comment's. Authorization holds the run's issue and then asks for the run
// row, so the delete has to hold that issue before it deletes the comment.
func TestCommentDeleteAndAuthorizeShareOneLockOrder(t *testing.T) {
	ctx := context.Background()
	f := claimElevated(t, "repo_write:1234")
	commentIssue := dbfx.Issue(t, "comment delete lock order")
	commentID := dbfx.Comment(t, commentIssue, "trigger")
	pointRunAtCommentWithStaleLineage(t, f, commentID)

	gate, authorized := holdAuthorizationAtElevationRow(t, f)
	deleted := make(chan error, 1)
	go func() {
		_, err := testHandler.deleteComment(ctx, parseUUID(commentID), parseUUID(testWorkspaceID))
		deleted <- err
	}()
	// Authorization waits on the elevation row; the delete waits on the run's
	// issue (taken first) or, unfixed, on it through the run's trigger.
	waitForLockWaiters(t, 2)
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("gate commit: %v", err)
	}

	if got := (<-authorized).Code; got != http.StatusOK && got != http.StatusForbidden {
		t.Fatalf("authorization status = %d, want 200 or 403 (a deadlock aborts it)", got)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("comment delete = %v, want it to commit (a deadlock aborts it)", err)
	}
	var nulled bool
	dbfx.QueryRow(t, `SELECT trigger_comment_id IS NULL FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&nulled)
	if !nulled {
		t.Fatal("the run still names the deleted comment")
	}
}

// An issue delete removes the issue's comments through comment.issue_id
// ON DELETE CASCADE. Each removed comment nulls trigger_comment_id on the runs
// that name it, and a run can belong to another issue. The delete has to hold
// that issue before the cascade runs.
func TestIssueDeleteAndAuthorizeShareOneLockOrder(t *testing.T) {
	ctx := context.Background()
	f := claimElevated(t, "repo_write:1234")
	deletedIssue := dbfx.Issue(t, "issue delete lock order")
	commentID := dbfx.Comment(t, deletedIssue, "trigger")
	pointRunAtCommentWithStaleLineage(t, f, commentID)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(deletedIssue))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}

	gate, authorized := holdAuthorizationAtElevationRow(t, f)
	deleted := make(chan error, 1)
	go func() {
		_, err := testHandler.deleteIssueAndCollectAttachmentURLs(ctx, issue, nil)
		deleted <- err
	}()
	waitForLockWaiters(t, 2)
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("gate commit: %v", err)
	}

	if got := (<-authorized).Code; got != http.StatusOK && got != http.StatusForbidden {
		t.Fatalf("authorization status = %d, want 200 or 403 (a deadlock aborts it)", got)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("issue delete = %v, want it to commit (a deadlock aborts it)", err)
	}
	var nulled bool
	dbfx.QueryRow(t, `SELECT trigger_comment_id IS NULL FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&nulled)
	if !nulled {
		t.Fatal("the run still names the comment of the deleted issue")
	}
}

// pointRunAtParentWithStaleLineage makes the elevated run a child of parentID
// and gives it a retry source whose taint arrives only afterwards. Nulling
// parent_task_id (the FK's ON DELETE SET NULL) then recomputes the run's taint
// from the lineage that remains, and the run's taint trigger locks the run's issue.
func pointRunAtParentWithStaleLineage(t *testing.T, f elevatedFixture, parentID string) {
	t.Helper()
	var agentID string
	dbfx.QueryRow(t, `SELECT agent_id FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&agentID)
	retrySource := taintedTask(t, agentID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET parent_task_id = $2, retry_of_task_id = $3 WHERE id = $1`, f.taskID, parentID, retrySource)
	dbfx.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['tr'] WHERE id = $1`, retrySource)
	if got := taintOf(t, "agent_task_queue", f.taskID); len(got) != 0 {
		t.Fatalf("run taint before the delete = %v, want none", got)
	}
}

// finishDeleteAgainstHeldAuthorization runs del while authorization holds the
// run's issue and waits on the elevation row, then releases the gate. The delete
// has to hold the run's issue before it reaches the run row; a deadlock aborts
// one side.
func finishDeleteAgainstHeldAuthorization(t *testing.T, f elevatedFixture, del func() error) {
	t.Helper()
	ctx := context.Background()
	gate, authorized := holdAuthorizationAtElevationRow(t, f)
	deleted := make(chan error, 1)
	go func() { deleted <- del() }()
	waitForLockWaiters(t, 2)
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("gate commit: %v", err)
	}
	if got := (<-authorized).Code; got != http.StatusOK && got != http.StatusForbidden {
		t.Fatalf("authorization status = %d, want 200 or 403 (a deadlock aborts it)", got)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete = %v, want it to commit (a deadlock aborts it)", err)
	}
	var detached bool
	dbfx.QueryRow(t, `SELECT parent_task_id IS NULL FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&detached)
	if !detached {
		t.Fatal("the run still names the deleted parent")
	}
}

// makeSystemAgentWithRun turns a fresh agent into an Agent Builder carrier and
// gives it a finished run, which the run under test then names as its parent.
func makeSystemAgentWithRun(t *testing.T, runtimeID, issueID string) (agentID, parentTaskID string) {
	t.Helper()
	agentID = dbfx.Agent(t, "carrier "+uuid.NewString()[:8], runtimeID)
	dbfx.Exec(t, `UPDATE agent SET kind = 'system', system_key = $2 WHERE id = $1`, agentID, "agent_builder:"+uuid.NewString())
	return agentID, insertDeletePathTask(t, agentID, issueID, runtimeID)
}

// An issue delete removes the issue's runs through agent_task_queue.issue_id
// ON DELETE CASCADE, and each removed run nulls parent_task_id on its children.
// A child can belong to another issue.
func TestIssueDeleteRunCascadeAndAuthorizeShareOneLockOrder(t *testing.T) {
	ctx := context.Background()
	f := claimElevated(t, "repo_write:1234")
	deletedIssue := dbfx.Issue(t, "issue delete run cascade")
	var agentID string
	dbfx.QueryRow(t, `SELECT agent_id FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&agentID)
	parent := insertDeletePathTask(t, agentID, deletedIssue, f.runtimeID)
	pointRunAtParentWithStaleLineage(t, f, parent)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(deletedIssue))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}

	finishDeleteAgainstHeldAuthorization(t, f, func() error {
		_, err := testHandler.deleteIssueAndCollectAttachmentURLs(ctx, issue, nil)
		return err
	})
}

// Deleting an Agent Builder carrier removes its runs through
// agent_task_queue.agent_id ON DELETE CASCADE.
func TestSystemAgentDeleteAndAuthorizeShareOneLockOrder(t *testing.T) {
	ctx := context.Background()
	f := claimElevated(t, "repo_write:1234")
	carrier, parent := makeSystemAgentWithRun(t, f.runtimeID, dbfx.Issue(t, "carrier run issue"))
	pointRunAtParentWithStaleLineage(t, f, parent)

	finishDeleteAgainstHeldAuthorization(t, f, func() error {
		tx, err := testHandler.TxStarter.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		qtx := testHandler.Queries.WithTx(tx)
		if err := service.LockIssuesBeforeSystemAgentDelete(ctx, qtx, parseUUID(carrier)); err != nil {
			return err
		}
		if err := deleteSystemAgent(ctx, qtx, parseUUID(carrier)); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}

// A runtime teardown deletes the runtime's system agents the same way.
func TestRuntimeTeardownSystemAgentDeleteAndAuthorizeShareOneLockOrder(t *testing.T) {
	ctx := context.Background()
	f := claimElevated(t, "repo_write:1234")
	runtimeID := createClaimReclaimRuntime(t, ctx, "carrier runtime "+uuid.NewString()[:8])
	_, parent := makeSystemAgentWithRun(t, runtimeID, dbfx.Issue(t, "carrier run issue"))
	pointRunAtParentWithStaleLineage(t, f, parent)

	finishDeleteAgainstHeldAuthorization(t, f, func() error {
		tx, err := testHandler.TxStarter.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := service.TeardownRuntime(ctx, testHandler.Queries.WithTx(tx), parseUUID(runtimeID), service.RuntimeTeardownOptions{CancelNonTerminalTasks: true}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}

// Teardown deletes every comment of the workspace. A run in a neighbour
// workspace can name one of them, and nulling that trigger takes the run's issue,
// which the teardown never locked.
func TestWorkspaceTeardownCommentDeleteAndAuthorizeShareOneLockOrder(t *testing.T) {
	ctx := context.Background()
	f := claimElevated(t, "repo_write:1234")
	victim := newWorkspaceDeletePathFixture(t, "comment-lock-order")
	var commentID string
	if err := testPool.QueryRow(ctx, `
INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
VALUES ($1, $2, 'member', $3, 'trigger') RETURNING id`,
		victim.victimID, victim.victimIssue, testUserID).Scan(&commentID); err != nil {
		t.Fatalf("create victim comment: %v", err)
	}
	pointRunAtCommentWithStaleLineage(t, f, commentID)

	gate, authorized := holdAuthorizationAtElevationRow(t, f)
	teardown := make(chan error, 1)
	go func() {
		tx, err := testHandler.TxStarter.Begin(ctx)
		if err != nil {
			teardown <- err
			return
		}
		defer tx.Rollback(ctx)
		qtx := testHandler.Queries.WithTx(tx)
		if err := qtx.SetWorkspaceTeardownMode(ctx); err != nil {
			teardown <- err
			return
		}
		if err := lockWorkspaceTaskOwners(ctx, qtx, parseUUID(victim.victimID)); err != nil {
			teardown <- err
			return
		}
		if err := deleteWorkspaceComments(ctx, qtx, parseUUID(victim.victimID)); err != nil {
			teardown <- err
			return
		}
		teardown <- tx.Commit(ctx)
	}()
	waitForLockWaiters(t, 2)
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("gate commit: %v", err)
	}

	if got := (<-authorized).Code; got != http.StatusOK && got != http.StatusForbidden {
		t.Fatalf("authorization status = %d, want 200 or 403 (a deadlock aborts it)", got)
	}
	if err := <-teardown; err != nil {
		t.Fatalf("teardown = %v, want it to commit (a deadlock aborts it)", err)
	}
	var nulled bool
	dbfx.QueryRow(t, `SELECT trigger_comment_id IS NULL FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&nulled)
	if !nulled {
		t.Fatal("the run still names the deleted comment")
	}
}
