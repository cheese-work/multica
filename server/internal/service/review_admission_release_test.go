package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/stagequeue"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-4651: a deferred reviewer run starts when the head turns admissible, once
// (CHE-4615 recovery tests 1-6). security-gate stays pending in every test that
// asserts a start: it never blocks and never triggers.

func (f *admissionFixture) release(t *testing.T, svc *TaskService, issueIDs ...pgtype.UUID) {
	t.Helper()
	if err := svc.ReleaseAdmissionDeferrals(context.Background(), issueIDs); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func (f *admissionFixture) setCheck(t *testing.T, name, status, conclusion string) {
	t.Helper()
	f.Exec(t, `UPDATE github_pull_request_check_run SET status = $3, conclusion = NULLIF($4, '') WHERE pr_id = $1 AND name = $2`, f.pr, name, status, conclusion)
}

func (f *admissionFixture) wantTasks(t *testing.T, want int, why string) {
	t.Helper()
	if got := f.tasksOn(t, f.issue); got != want {
		t.Fatalf("%s: %d tasks, want %d", why, got, want)
	}
}

func (f *admissionFixture) wantRecord(t *testing.T, want bool, why string) {
	t.Helper()
	if _, _, _, ok := f.deferral(t); ok != want {
		t.Fatalf("%s: record present = %v, want %v", why, ok, want)
	}
}

// Recovery 1: CI pending, deferred. CI turns green. The run starts once.
func TestAdmissionReleaseStartsOnceWhenCIturnsGreen(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	svc := f.service(t)
	f.check(t, "ci", "in_progress", "")
	f.check(t, "security-gate", "in_progress", "")
	f.wantDeferred(t, "ci: pending")

	f.release(t, svc)
	f.wantTasks(t, 0, "CI still pending")
	f.wantRecord(t, true, "CI still pending")

	f.setCheck(t, "ci", "completed", "success")
	f.release(t, svc)
	f.release(t, svc)
	f.wantTasks(t, 1, "CI green, released twice")
	f.wantRecord(t, false, "after the start")
}

// Recovery 2: DIRTY, deferred. The conflict is resolved. The run starts once.
func TestAdmissionReleaseStartsOnceWhenTheConflictClears(t *testing.T) {
	f := newAdmissionFixture(t, "DIRTY")
	svc := f.service(t)
	f.check(t, "ci", "completed", "success")
	f.check(t, "security-gate", "in_progress", "")
	f.wantDeferred(t, "merge conflict")

	f.release(t, svc)
	f.wantTasks(t, 0, "still conflicting")

	f.Exec(t, `UPDATE github_pull_request SET api_merge_state_status = 'CLEAN' WHERE id = $1`, f.pr)
	f.release(t, svc)
	f.release(t, svc)
	f.wantTasks(t, 1, "conflict resolved, released twice")
	f.wantRecord(t, false, "after the start")
}

// Recovery 3: CI red, deferred. A new head is pushed with CI green. The run
// starts on the new head and the record for the old head is gone.
func TestAdmissionReleaseStartsOnTheNewHeadAndDropsTheOldRecord(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	svc := f.service(t)
	f.check(t, "ci", "completed", "failure")
	f.check(t, "security-gate", "in_progress", "")
	f.wantDeferred(t, "ci: failing")

	f.Exec(t, `UPDATE github_pull_request SET head_sha = 'bbbb2222' WHERE id = $1`, f.pr)
	f.InsertNoID(t, "github_pull_request_check_run", testutil.Cols{
		"pr_id": f.pr, "head_sha": "bbbb2222", "ordinal": 9, "name": "ci", "status": "completed", "conclusion": "success",
	}, "pr_id = $1 AND ordinal = 9", f.pr)
	f.release(t, svc)

	f.wantTasks(t, 1, "new head green")
	var head string
	f.QueryRow(t, `SELECT COALESCE(context->>'head_sha', '') FROM agent_task_queue WHERE issue_id = $1`, f.issue).Scan(&head)
	if head != "bbbb2222" {
		t.Fatalf("run head = %q, want bbbb2222", head)
	}
	f.wantRecord(t, false, "the old head's record")
}

// A new head that is not admissible either replaces the record: a fresh 24 h
// window on the new head, no run.
func TestAdmissionReleaseRecordsTheNewHeadWhenItIsNotAdmissible(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	svc := f.service(t)
	f.check(t, "ci", "completed", "failure")
	f.wantDeferred(t, "ci: failing")
	f.Exec(t, `UPDATE review_admission_deferral SET created_at = now() - interval '20 hours', expires_at = now() + interval '4 hours' WHERE issue_id = $1`, f.issue)

	f.Exec(t, `UPDATE github_pull_request SET head_sha = 'bbbb2222' WHERE id = $1`, f.pr)
	f.release(t, svc)

	f.wantTasks(t, 0, "new head has no CI yet")
	head, reasons, window, ok := f.deferral(t)
	if !ok || head != "bbbb2222" || window != 24*time.Hour || len(reasons) != 1 || reasons[0] != "ci: missing" {
		t.Fatalf("record head %q reasons %v window %v ok %v, want bbbb2222, [ci: missing], 24h", head, reasons, window, ok)
	}
}

// Recovery 4: security-gate changing state alone neither starts nor cancels a
// deferred run; only the admission check does.
func TestAdmissionReleaseIgnoresSecurityGate(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	svc := f.service(t)
	f.check(t, "ci", "completed", "failure")
	f.check(t, "security-gate", "in_progress", "")
	f.wantDeferred(t, "ci: failing")

	for _, c := range [][2]string{{"completed", "success"}, {"completed", "failure"}, {"in_progress", ""}} {
		f.setCheck(t, "security-gate", c[0], c[1])
		f.release(t, svc)
		f.wantTasks(t, 0, "security-gate "+c[0]+"/"+c[1])
		f.wantRecord(t, true, "security-gate "+c[0]+"/"+c[1])
	}
	f.setCheck(t, "security-gate", "completed", "failure")
	f.setCheck(t, "ci", "completed", "success")
	f.release(t, svc)
	f.wantTasks(t, 1, "CI green with security-gate failing")
}

// Recovery 5: no admissible event arrives. At expiry one run starts with the
// reason "deferral expired", and the record is gone.
func TestAdmissionReleaseStartsOnceAtExpiry(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	svc := f.service(t)
	f.check(t, "ci", "completed", "failure")
	f.wantDeferred(t, "ci: failing")

	f.Exec(t, `UPDATE review_admission_deferral SET created_at = now() - interval '23 hours', expires_at = now() + interval '1 hour' WHERE issue_id = $1`, f.issue)
	f.release(t, svc)
	f.wantTasks(t, 0, "before expiry")

	f.Exec(t, `UPDATE review_admission_deferral SET created_at = now() - interval '25 hours', expires_at = now() - interval '1 hour' WHERE issue_id = $1`, f.issue)
	f.release(t, svc)
	f.release(t, svc)
	f.wantTasks(t, 1, "after expiry, released twice")
	var note string
	f.QueryRow(t, `SELECT COALESCE(handoff_note, '') FROM agent_task_queue WHERE issue_id = $1`, f.issue).Scan(&note)
	if note != "deferral expired" {
		t.Fatalf("run reason = %q, want %q", note, "deferral expired")
	}
	f.wantRecord(t, false, "after the expiry start")
}

// Recovery 6: two triggers for the same issue, agent and head give one run.
func TestAdmissionReleaseTwoTriggersGiveOneRun(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	svc := f.service(t)
	f.check(t, "ci", "in_progress", "")
	f.check(t, "security-gate", "in_progress", "")
	f.wantDeferred(t, "ci: pending")
	if err := f.guardAs(f.agentID, "wakeup", true); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("second trigger: err = %v, want ErrAdmissionDeferred", err)
	}
	if n := f.Count(t, `SELECT count(*) FROM review_admission_deferral WHERE issue_id = $1`, f.issue); n != 1 {
		t.Fatalf("%d records for two triggers, want 1", n)
	}

	f.setCheck(t, "ci", "completed", "success")
	f.release(t, svc)
	f.release(t, svc)
	f.wantTasks(t, 1, "two triggers, CI green")
}

// A released run keeps who asked, the trigger comment and the taint the asker
// carried; release never launders a tainted request into a clean run.
func TestAdmissionReleaseKeepsTheRequesterCommentAndTaint(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	svc := f.service(t)
	actor := f.User(t, "Requester", fmt.Sprintf("admission-actor-%d@multica.test", time.Now().UnixNano()))
	f.Member(t, f.WorkspaceID, actor, "member")
	comment := f.Comment(t, f.issue, "please review", testutil.Cols{"author_id": actor})
	f.check(t, "ci", "in_progress", "")
	issue, err := f.q.GetIssue(context.Background(), util.MustParseUUID(f.issue))
	if err != nil {
		t.Fatal(err)
	}
	taintedTask := f.run(t, f.Issue(t, "elsewhere", testutil.Cols{"status": "in_progress"}), "running")
	f.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['ext-1'] WHERE id = $1`, taintedTask)
	ctx := WithActingTask(context.Background(), util.MustParseUUID(taintedTask))

	_, err = svc.EnqueueCommentTriggeredTask(ctx, issue, util.MustParseUUID(f.agentID), util.MustParseUUID(comment),
		true, false, pgtype.UUID{}, "", util.MustParseUUID(actor), OriginDerived)
	if !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("comment-triggered enqueue on pending CI: err = %v, want ErrAdmissionDeferred", err)
	}
	f.wantTasks(t, 0, "after the deferral")

	f.setCheck(t, "ci", "completed", "success")
	f.release(t, svc)
	originator, taint := f.releasedTask(t, f.issue)
	var got string
	f.QueryRow(t, `SELECT COALESCE(trigger_comment_id::text, '') FROM agent_task_queue WHERE issue_id = $1`, f.issue).Scan(&got)
	if originator != actor || got != comment || len(taint) != 1 || taint[0] != "ext-1" {
		t.Fatalf("released run originator %q comment %q taint %v, want %q %q [ext-1]", originator, got, taint, actor, comment)
	}
}

// A GitHub event releases only the issues of its PR; the tick releases all.
func TestAdmissionReleaseScopesToTheGivenIssues(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	svc := f.service(t)
	f.check(t, "ci", "in_progress", "")
	f.wantDeferred(t, "ci: pending")
	f.setCheck(t, "ci", "completed", "success")

	f.release(t, svc, util.MustParseUUID(f.Issue(t, "unrelated", testutil.Cols{})))
	f.wantTasks(t, 0, "event for another issue")
	f.release(t, svc, util.MustParseUUID(f.issue))
	f.wantTasks(t, 1, "event for this issue")
}

// A record whose PR is gone has nothing to review: it is dropped, no run.
func TestAdmissionReleaseDropsARecordWithoutAnOpenPR(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	svc := f.service(t)
	f.check(t, "ci", "in_progress", "")
	f.wantDeferred(t, "ci: pending")
	f.Exec(t, `UPDATE github_pull_request SET state = 'merged' WHERE id = $1`, f.pr)
	f.release(t, svc)
	f.wantTasks(t, 0, "merged PR")
	f.wantRecord(t, false, "merged PR")
}

// The GitHub dispatch halt (CHE-2220) holds a deferred run too: the tick must
// not start what the event path may not. The record stays for after the resume.
func TestAdmissionReleaseHonoursTheDispatchHalt(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	svc := f.service(t)
	f.check(t, "ci", "in_progress", "")
	f.wantDeferred(t, "ci: pending")
	f.setCheck(t, "ci", "completed", "success")
	f.Exec(t, `INSERT INTO github_dispatch_halt (workspace_id, scope) VALUES ($1, '*')`, f.WorkspaceID)

	f.release(t, svc)
	f.wantTasks(t, 0, "dispatch halted")
	f.wantRecord(t, true, "dispatch halted")

	f.Exec(t, `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, f.WorkspaceID)
	f.release(t, svc)
	f.wantTasks(t, 1, "after the resume")
}

// CHE-4651 security finding: the release runs with no acting task, so the
// taint the deferral recorded travels only in the context (stageTaint). When
// the stage is full at release time, the stage refusal must keep that taint on
// its waiting record; otherwise the later stage release starts a clean task.
func TestAdmissionReleaseKeepsTaintWhenTheStageIsFull(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	stageLimits = stagequeue.Limits{stagequeue.StageReview: 1}
	svc := f.service(t)
	f.check(t, "ci", "in_progress", "")
	issue, err := f.q.GetIssue(context.Background(), util.MustParseUUID(f.issue))
	if err != nil {
		t.Fatal(err)
	}
	taintedTask := f.run(t, f.Issue(t, "elsewhere", testutil.Cols{"status": "in_progress"}), "running")
	f.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['ext-1'] WHERE id = $1`, taintedTask)
	ctx := WithActingTask(context.Background(), util.MustParseUUID(taintedTask))
	if _, err := svc.EnqueueTaskForIssue(ctx, issue); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("enqueue on pending CI: err = %v, want ErrAdmissionDeferred", err)
	}

	// CI turns green while another PR holds the only review slot.
	holder := f.reviewIssue(t, "slot holder", 3*time.Hour)
	holderRun := f.run(t, holder, "running")
	f.setCheck(t, "ci", "completed", "success")
	f.release(t, svc)
	f.wantTasks(t, 0, "stage full")

	var waiting []string
	f.QueryRow(t, `SELECT taint_ids FROM stage_queue_waiting WHERE issue_id = $1 AND agent_id = $2`, f.issue, f.agentID).Scan(&waiting)
	if len(waiting) != 1 || waiting[0] != "ext-1" {
		t.Fatalf("stage waiting record taint = %v, want [ext-1]", waiting)
	}

	f.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, holderRun)
	if err := svc.ReleaseStageQueues(context.Background()); err != nil {
		t.Fatalf("stage release: %v", err)
	}
	_, taint := f.releasedTask(t, f.issue)
	if len(taint) != 1 || taint[0] != "ext-1" {
		t.Fatalf("released run taint = %v, want [ext-1]", taint)
	}
}
