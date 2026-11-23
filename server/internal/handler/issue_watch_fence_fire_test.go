package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-8962 (plan CHE-8945 r6, step 2): the claim-to-scheduler contract. The run
// R is claimed through the real claim handler, so the stored `watch` fence is
// the one production writes; the wakeup it creates then fires through the real
// scheduler dispatch. Each case has a control: the same baseline defers, and the
// one tested change makes the fire run.

type fenceFire struct {
	runtimeID, agentID, issueID string
	run                         string
	svc                         *service.IssueWakeupService
}

// claimFenceRun queues and claims R through the claim handler, and returns its id.
func claimFenceRun(t *testing.T, runtimeID, issueID, agentID string) string {
	t.Helper()
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID})
	req := newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil)
	req = withURLParam(req, "runtimeId", runtimeID)
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK)
	return taskID
}

// newFenceFire sets the workspace to block mode and returns the fixture. The
// claim is left to the caller, so a test can hook the fence read first.
func newFenceFire(t *testing.T, label string) *fenceFire {
	t.Helper()
	runtimeID, agentID, issueID := newFenceFixture(t, label)
	ctx := context.Background()
	var before []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&before)
	dbfx.Exec(t, `UPDATE workspace SET settings = COALESCE(settings, '{}'::jsonb) || '{"time_wakeup_skip":{"mode":"block"}}'::jsonb WHERE id = $1`, testWorkspaceID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(before))
	})
	dbfx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, agentID)
	// Cleanups run last-in first-out: these go before the fixture deletes the issue.
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id = $1)`, issueID)
		testPool.Exec(ctx, `DELETE FROM issue_wakeup WHERE issue_id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM activity_log WHERE issue_id = $1`, issueID)
	})
	return &fenceFire{runtimeID: runtimeID, agentID: agentID, issueID: issueID,
		svc: &service.IssueWakeupService{Tasks: testHandler.TaskService}}
}

// say posts n comments as R: the real insert, which bumps the issue revision.
func (f *fenceFire) say(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := testHandler.Queries.CreateComment(context.Background(), db.CreateCommentParams{
			IssueID: fenceUUID(t, f.issueID), WorkspaceID: fenceUUID(t, testWorkspaceID),
			AuthorType: "agent", AuthorID: fenceUUID(t, f.agentID),
			Content: "progress", Type: "comment", SourceTaskID: fenceUUID(t, f.run),
		}); err != nil {
			t.Fatalf("R comment: %v", err)
		}
	}
}

// arm creates the one-shot `at --watch issue` wakeup from R and completes R.
func (f *fenceFire) arm(t *testing.T) pgtype.UUID {
	t.Helper()
	w, err := f.svc.Create(context.Background(), fenceUUID(t, f.issueID), fenceUUID(t, testUserID), fenceUUID(t, f.run), service.WakeupInput{
		AgentID: f.agentID, Kind: "at", AfterSeconds: 600, Instruction: "Check back", WatchInputs: []string{"issue"},
	})
	if err != nil {
		t.Fatalf("create wakeup: %v", err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, f.run)
	return w.ID
}

// fire makes the wakeup due and dispatches it through the scheduler.
func (f *fenceFire) fire(t *testing.T, id pgtype.UUID) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at = now() - interval '1 second' WHERE id = $1`, id)
	if err := f.svc.DispatchRule(context.Background(), fenceUUID(t, testWorkspaceID), id); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

func (f *fenceFire) runs(t *testing.T, id pgtype.UUID) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id' = $1`, uuidToString(id))
}

func (f *fenceFire) deferrals(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'wake_deferred'`, f.issueID)
}

func (f *fenceFire) wantDeferred(t *testing.T, id pgtype.UUID) {
	t.Helper()
	f.fire(t, id)
	if got := f.runs(t, id); got != 0 {
		t.Fatalf("runs = %d, want 0 (unchanged issue must defer)", got)
	}
	if got := f.deferrals(t); got != 1 {
		t.Fatalf("wake_deferred rows = %d, want 1", got)
	}
	var baseline, last string
	dbfx.QueryRow(t, `SELECT details->>'baseline', details->>'last_task_id' FROM activity_log WHERE issue_id = $1 AND action = 'wake_deferred'`, f.issueID).Scan(&baseline, &last)
	if baseline != "creating_run" || last != f.run {
		t.Fatalf("deferral baseline=%q last_task_id=%q, want creating_run and %s", baseline, last, f.run)
	}
}

func (f *fenceFire) wantRan(t *testing.T, id pgtype.UUID) {
	t.Helper()
	f.fire(t, id)
	if got := f.runs(t, id); got != 1 {
		t.Fatalf("runs = %d, want 1 (a change since the claim must fire)", got)
	}
	if got := f.deferrals(t); got != 0 {
		t.Fatalf("wake_deferred rows = %d, want 0", got)
	}
}

// An issue that R's claim recorded and R itself only commented on is deferred;
// the same baseline runs once another actor writes. Both use the stored fence.
func TestIssueWatchFenceFire_ClaimToScheduler(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(t *testing.T, f *fenceFire)
		want  func(f *fenceFire, t *testing.T, id pgtype.UUID)
	}{
		{"unchanged defers", func(*testing.T, *fenceFire) {}, (*fenceFire).wantDeferred},
		{"a member comment runs", func(t *testing.T, f *fenceFire) {
			dbfx.Comment(t, f.issueID, "A person wrote")
		}, (*fenceFire).wantRan},
		{"a status change by another actor runs", func(t *testing.T, f *fenceFire) {
			dbfx.Exec(t, `UPDATE issue SET status = 'in_review', revision = revision + 1 WHERE id = $1`, f.issueID)
		}, (*fenceFire).wantRan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFenceFire(t, "fire "+tc.name)
			f.run = claimFenceRun(t, f.runtimeID, f.issueID, f.agentID)
			f.say(t, 2)
			id := f.arm(t)
			tc.after(t, f)
			tc.want(f, t, id)
		})
	}
}

// F5: another actor writes after the claim's fence read and before its payload
// read. R saw the change; the fence does not hold it, so the fire must run. The
// control runs the same hook without the write and defers.
func TestIssueWatchFenceFire_WriteBetweenFenceAndPayloadReads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write bool
		want  func(f *fenceFire, t *testing.T, id pgtype.UUID)
	}{
		{"control: no write between the reads defers", false, (*fenceFire).wantDeferred},
		{"a write between the reads runs", true, (*fenceFire).wantRan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFenceFire(t, "f5 "+tc.name)
			prev := issueWatchFenceRead
			t.Cleanup(func() { issueWatchFenceRead = prev })
			reads := 0
			issueWatchFenceRead = func(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (db.IssueWatchFenceRow, error) {
				row, err := prev(ctx, q, issueID)
				reads++
				if tc.write {
					dbfx.Exec(t, `UPDATE issue SET status = 'in_review', revision = revision + 1 WHERE id = $1`, f.issueID)
				}
				return row, err
			}
			f.run = claimFenceRun(t, f.runtimeID, f.issueID, f.agentID)
			if reads != 1 {
				t.Fatalf("fence reads during the claim = %d, want 1", reads)
			}
			f.say(t, 2)
			id := f.arm(t)
			tc.want(f, t, id)
		})
	}
}
