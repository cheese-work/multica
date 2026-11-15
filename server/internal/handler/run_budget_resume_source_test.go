package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Slice 2 (CHE-2860 revise): a refusal of a native wakeup, system wakeup or
// retry is resumed through that same path. The tests build the durable source
// (a rule with its receipt, a failed parent run), take the refusal from the
// real guard, approve it and run the worker, the native tick and the sweep.

type nativeSource struct {
	trigger string
	// fire makes the native path run once and be refused.
	fire func(t *testing.T, c budgetCase) sourceRef
}

// sourceRef names the durable state the delivered run must come from.
type sourceRef struct {
	wakeupID, parentTaskID string
}

func (r sourceRef) pending(t *testing.T) int {
	t.Helper()
	if r.wakeupID == "" {
		return 0
	}
	return pendingEventReceipts(t, r.wakeupID)
}

// refuseNative runs fire with the exhaustion hook captured, then records the
// refusal's intent the way the hook's first transaction does.
func refuseNative(t *testing.T, installationID int64, src nativeSource) (budgetCase, pgtype.UUID, sourceRef) {
	t.Helper()
	c, e, ref := captureNative(t, installationID, src, "in_progress")
	crashAt(t, "after_intent")
	testHandler.onRunBudgetExhausted(context.Background(), e)
	return c, refusalIDs(t, c)[0], ref
}

// blockNative runs the whole hook for the refused native trigger: the issue is
// Blocked and the refusal remembers status.
func blockNative(t *testing.T, installationID int64, src nativeSource, status string) (budgetCase, pgtype.UUID, sourceRef) {
	t.Helper()
	c, e, ref := captureNative(t, installationID, src, status)
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked", got)
	}
	id := refusalIDs(t, c)[0]
	if r, err := testHandler.Queries.GetRunBudgetRefusal(context.Background(), id); err != nil || !r.Blocked || !(r.SourceWakeupID.Valid || r.SourceTaskID.Valid) {
		t.Fatalf("refusal = %+v, %v, want blocked with a native source", r, err)
	}
	return c, id, ref
}

func captureNative(t *testing.T, installationID int64, src nativeSource, status string) (budgetCase, service.RunBudgetExhausted, sourceRef) {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	c := newBudgetCase(t, installationID, recoverySteps(seededReadyAgentID(t)), 2)
	if status != "in_progress" {
		c.enter(t, status)
	}
	recordOccurrence(t, c, status)
	c.spend(t, 2)
	events := make(chan service.RunBudgetExhausted, 4)
	service.SetOnRunBudgetExhausted(func(_ context.Context, e service.RunBudgetExhausted) { events <- e })
	t.Cleanup(func() { service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted) })
	ref := src.fire(t, c)
	var e service.RunBudgetExhausted
	select {
	case e = <-events:
	case <-time.After(5 * time.Second):
		t.Fatalf("the %s was never refused by the budget", src.trigger)
	}
	if e.Target.Trigger != src.trigger {
		t.Fatalf("refused trigger = %q, want %q", e.Target.Trigger, src.trigger)
	}
	return c, e, ref
}

func fireWakeup(t *testing.T, c budgetCase) sourceRef {
	t.Helper()
	svc := service.IssueWakeupService{Tasks: testHandler.TaskService}
	w, err := svc.Create(context.Background(), parseUUID(c.issueID), parseUUID(testUserID), pgtype.UUID{}, service.WakeupInput{
		AgentID: c.agentID, Kind: "event", EventTypes: []string{"issue.status_changed"}, Instruction: "look again",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1`, w.ID)
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id = $1`, c.issueID)
	})
	// A rule a person made is a named run the budget never refuses; one an agent
	// made from a task is derived, so a spent budget stops it.
	dbfx.Exec(t, `UPDATE issue_wakeup SET source_task_id=(SELECT id FROM agent_task_queue WHERE issue_id=$2 LIMIT 1) WHERE id=$1`, w.ID, c.issueID)
	if err := svc.Trigger(context.Background(), parseUUID(c.issueID), w.ID, parseUUID(testUserID)); err != nil {
		t.Logf("trigger: %v", err)
	}
	return sourceRef{wakeupID: uuidToString(w.ID)}
}

func fireRetry(t *testing.T, c budgetCase) sourceRef {
	t.Helper()
	var runtimeID string
	dbfxQueryRuntime(t, c.agentID, &runtimeID)
	parentID := dbfxTask(t, c, runtimeID)
	parent, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(parentID))
	if err != nil {
		t.Fatal(err)
	}
	if child, err := testHandler.TaskService.MaybeRetryFailedTask(context.Background(), parent); err != nil || child != nil {
		t.Fatalf("retry = %v, %v, want it refused", child, err)
	}
	return sourceRef{parentTaskID: parentID}
}

func dbfxQueryRuntime(t *testing.T, agentID string, out *string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(), `SELECT runtime_id::text FROM agent WHERE id=$1`, agentID).Scan(out); err != nil {
		t.Fatal(err)
	}
}

func dbfxTask(t *testing.T, c budgetCase, runtimeID string) string {
	t.Helper()
	return dbfx.Task(t, c.agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": c.issueID, "status": "failed", "failure_reason": "timeout",
		"attempt": 1, "max_attempts": 2,
		"started_at":   testutil.Raw("now() - interval '2 minutes'"),
		"completed_at": testutil.Raw("now() - interval '1 minute'"),
	})
}

func fireSystemWakeup(t *testing.T, c budgetCase) sourceRef {
	t.Helper()
	// The rule holds once every sub-issue is closed, the edge case's own included.
	testPool.Exec(context.Background(), `UPDATE issue SET status='done' WHERE parent_issue_id=$1`, c.issueID)
	childID := createSourceChild(t, c.issueID)
	updateChildStatus(t, childID, "done")
	runWakeupTick(t)
	var wakeupID string
	dbfx.QueryRow(t, `SELECT id::text FROM issue_wakeup WHERE issue_id=$1 AND system_rule='child_done'`, c.issueID).Scan(&wakeupID)
	return sourceRef{wakeupID: wakeupID}
}

func createSourceChild(t *testing.T, parentID string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "source child " + time.Now().Format(time.RFC3339Nano), "status": "in_progress", "parent_issue_id": parentID,
	})
	testHandler.CreateIssue(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create child: %d %s", w.Code, w.Body.String())
	}
	var child IssueResponse
	decodeJSON(t, w.Body.String(), &child)
	t.Cleanup(func() { cleanupChildDoneIssue(child.ID) })
	return child.ID
}

var nativeSources = []nativeSource{
	{"wakeup", fireWakeup},
	{"system wakeup", fireSystemWakeup},
	{"retry", fireRetry},
}

// deliveries counts the runs queued for the issue, and the retry children of a parent.
func deliveries(t *testing.T, c budgetCase, ref sourceRef) (queued, ofSource int) {
	t.Helper()
	queued = resumeQueued(t, c)
	switch {
	case ref.parentTaskID != "":
		ofSource = dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, ref.parentTaskID)
	default:
		ofSource = dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status='queued' AND context->>'wakeup_id'=$2`, c.issueID, ref.wakeupID)
	}
	return queued, ofSource
}

func wantOneDelivery(t *testing.T, c budgetCase, id pgtype.UUID, ref sourceRef) {
	t.Helper()
	wantResume(t, id, "run_queued", "")
	if q, s := deliveries(t, c, ref); q != 1 || s != 1 {
		t.Fatalf("queued runs = %d, of the source = %d, want 1 and 1", q, s)
	}
	if n := ref.pending(t); n != 0 {
		t.Fatalf("pending receipts = %d, want 0 (consumed once)", n)
	}
	// The resume neither starts nor defers a review: the delivered run is the
	// source's own, so review admission has nothing recorded for the issue.
	if n := dbfx.Count(t, `SELECT count(*) FROM review_admission_deferral WHERE issue_id=$1`, c.issueID); n != 0 {
		t.Fatalf("review admission deferrals = %d, want 0", n)
	}
	if got := resumeRowOf(t, id).TaskID; !got.Valid {
		t.Fatalf("the refusal records no task")
	}
}

func TestRunBudgetResumeDeliversARefusedNativeTriggerOnce(t *testing.T) {
	for i, src := range nativeSources {
		t.Run(src.trigger, func(t *testing.T) {
			c, id, ref := refuseNative(t, int64(2860200+i), src)
			wantResume(t, id, "open", "")
			linkRefusal(t, id, resumeRowOf(t, id).Window)
			raiseCap(t, c)
			resume(t, id)
			resume(t, id)
			runWakeupTick(t)
			testHandler.sweepRunBudgetResumes(context.Background(), time.Now().Add(time.Minute))
			wantOneDelivery(t, c, id, ref)
		})
	}
}

// The native tick and the worker race for the same refused work: one delivers.
func TestRunBudgetResumeRacesTheNativeDispatchAndDeliversOnce(t *testing.T) {
	for i, src := range nativeSources {
		t.Run(src.trigger, func(t *testing.T) {
			c, id, ref := refuseNative(t, int64(2860210+i), src)
			linkRefusal(t, id, resumeRowOf(t, id).Window)
			raiseCap(t, c)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); resume(t, id) }()
			go func() { defer wg.Done(); runWakeupTick(t) }()
			wg.Wait()
			resume(t, id)
			runWakeupTick(t)
			wantOneDelivery(t, c, id, ref)
		})
	}
}

// A crash after the dispatch bound the refusal and before it committed leaves
// nothing behind: the refusal, the run, the receipts or retry the dispatch
// consumed and its evidence roll back together, and the sweep delivers once.
func TestRunBudgetResumeRollsBackANativeDispatchThatCrashesBeforeCommit(t *testing.T) {
	for i, src := range nativeSources {
		t.Run(src.trigger, func(t *testing.T) {
			c, id, ref := refuseNative(t, int64(2860220+i), src)
			linkRefusal(t, id, resumeRowOf(t, id).Window)
			raiseCap(t, c)
			pendingBefore := ref.pending(t)
			service.RunBudgetBindAt = func(stage string) bool { return stage == "after_bind" }
			t.Cleanup(func() { service.RunBudgetBindAt = func(string) bool { return false } })
			resume(t, id)
			wantResume(t, id, "resume_pending", "")
			if got := resumeRowOf(t, id).TaskID; got.Valid {
				t.Fatalf("the crashed dispatch left task %s on the refusal", uuidToString(got))
			}
			if q, s := deliveries(t, c, ref); q != 0 || s != 0 {
				t.Fatalf("after the crash: queued runs = %d, of the source = %d, want 0 and 0", q, s)
			}
			if got := ref.pending(t); got != pendingBefore {
				t.Fatalf("after the crash: pending receipts = %d, want %d (not consumed)", got, pendingBefore)
			}
			service.RunBudgetBindAt = func(string) bool { return false }
			testHandler.sweepRunBudgetResumes(context.Background(), time.Now().Add(time.Minute))
			testHandler.sweepRunBudgetResumes(context.Background(), time.Now().Add(2*time.Minute))
			wantOneDelivery(t, c, id, ref)
		})
	}
}

// A blocked refusal always ends in the restore transaction, whatever its source:
// the resume admits the run the restored status owes and leaves the source's
// native state (receipts, retry lineage) untouched.
func nativeOwed(t *testing.T, c budgetCase, ref sourceRef, pendingBefore int) {
	t.Helper()
	if q, s := deliveries(t, c, ref); q != 1 || s != 0 {
		t.Fatalf("queued runs = %d, of the source = %d, want 1 (the owed run) and 0", q, s)
	}
	if got := ref.pending(t); got != pendingBefore {
		t.Fatalf("pending receipts = %d, want %d (not consumed)", got, pendingBefore)
	}
}

func TestRunBudgetResumeBlockedNativeRefusalAdmitsTheRunTheStatusOwes(t *testing.T) {
	for i, src := range nativeSources {
		t.Run(src.trigger, func(t *testing.T) {
			c, id, ref := blockNative(t, int64(2860230+i), src, "in_review")
			action := linkRefusal(t, id, resumeRowOf(t, id).Window)
			raiseCap(t, c)
			pendingBefore := ref.pending(t)
			resume(t, id)
			resume(t, id)
			r := wantResume(t, id, "run_queued", "")
			nativeOwed(t, c, ref, pendingBefore)
			if got := refusedRunOf(t, c).Review; r.TaskID != got {
				t.Fatalf("refusal ends on task %s, want the owed run %s", uuidToString(r.TaskID), uuidToString(got))
			}
			if got := c.status(t); got != "in_review" || restoreRows(t, c, action) != 1 {
				t.Fatalf("issue %s, restore rows %d: want in_review, 1", got, restoreRows(t, c, action))
			}
		})
	}
}

func TestRunBudgetResumeBlockedNativeRefusalThatOwesNoRunRestoresTheStatus(t *testing.T) {
	for i, src := range nativeSources {
		t.Run(src.trigger, func(t *testing.T) {
			c, id, ref := blockNative(t, int64(2860240+i), src, "in_progress")
			action := linkRefusal(t, id, resumeRowOf(t, id).Window)
			raiseCap(t, c)
			pendingBefore := ref.pending(t)
			resume(t, id)
			resume(t, id)
			wantResume(t, id, "status_restored", "")
			if q, s := deliveries(t, c, ref); q != 0 || s != 0 {
				t.Fatalf("queued runs = %d, of the source = %d, want 0 and 0", q, s)
			}
			if got := ref.pending(t); got != pendingBefore {
				t.Fatalf("pending receipts = %d, want %d (not consumed)", got, pendingBefore)
			}
			if got := c.status(t); got != "in_progress" || restoreRows(t, c, action) != 1 {
				t.Fatalf("issue %s, restore rows %d: want in_progress, 1", got, restoreRows(t, c, action))
			}
		})
	}
}

func TestRunBudgetResumeBlockedNativeRefusalRollsBackTogetherOnACrashBeforeCommit(t *testing.T) {
	for i, src := range nativeSources {
		t.Run(src.trigger, func(t *testing.T) {
			c, id, ref := blockNative(t, int64(2860250+i), src, "in_review")
			action := linkRefusal(t, id, resumeRowOf(t, id).Window)
			raiseCap(t, c)
			pendingBefore := ref.pending(t)
			runBudgetResumeAt = func(stage string) bool {
				if stage != "before_commit" {
					return false
				}
				runBudgetResumeAt = func(string) bool { return false }
				return true
			}
			t.Cleanup(func() { runBudgetResumeAt = func(string) bool { return false } })
			resume(t, id)
			wantResume(t, id, "resume_pending", "")
			if got := resumeRowOf(t, id).TaskID; got.Valid {
				t.Fatalf("the crashed resume left task %s on the refusal", uuidToString(got))
			}
			if got := c.status(t); got != "blocked" || restoreRows(t, c, action) != 0 {
				t.Fatalf("after the crash: issue %s, restore rows %d, want blocked, 0", got, restoreRows(t, c, action))
			}
			if q, s := deliveries(t, c, ref); q != 0 || s != 0 {
				t.Fatalf("after the crash: queued runs = %d, of the source = %d, want 0 and 0", q, s)
			}
			testHandler.sweepRunBudgetResumes(context.Background(), time.Now().Add(time.Minute))
			testHandler.sweepRunBudgetResumes(context.Background(), time.Now().Add(2*time.Minute))
			wantResume(t, id, "run_queued", "")
			nativeOwed(t, c, ref, pendingBefore)
		})
	}
}
