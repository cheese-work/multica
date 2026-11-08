package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/stagequeue"
	"github.com/multica-ai/multica/server/internal/util"
)

// admissionFixture is a review issue (agent_review, assigned to the reviewer)
// with one open PR at head "h1" and admission checks "build" and "lint".
type admissionFixture struct {
	*stageFixture
	issue, pr string
	ordinal   int
}

func newAdmissionFixture(t *testing.T) *admissionFixture {
	t.Helper()
	f := &admissionFixture{stageFixture: newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 100})}
	prev := admissionChecks
	admissionChecks = []string{"build", "lint"}
	t.Cleanup(func() { admissionChecks = prev })
	f.issue = f.reviewIssue(t, "PR under review", time.Hour)
	f.QueryRow(t, `SELECT pull_request_id::text FROM issue_pull_request WHERE issue_id = $1`, f.issue).Scan(&f.pr)
	f.Exec(t, `UPDATE github_pull_request SET head_sha = 'h1', snapshot_head_sha = 'h1', api_merge_state_status = 'CLEAN' WHERE id = $1`, f.pr)
	return f
}

// check stores one check run on a head.
func (f *admissionFixture) check(t *testing.T, head, name, status, conclusion string) {
	t.Helper()
	f.ordinal++
	f.Exec(t, `INSERT INTO github_pull_request_check_run (pr_id, head_sha, ordinal, name, status, conclusion)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`, f.pr, head, f.ordinal, name, status, conclusion)
}

func (f *admissionFixture) green(t *testing.T) {
	t.Helper()
	f.check(t, "h1", "build", "completed", "success")
	f.check(t, "h1", "lint", "completed", "success")
}

func (f *admissionFixture) deferral(t *testing.T) (head string, reasons []string, n int) {
	t.Helper()
	n = f.Count(t, `SELECT count(*) FROM admission_deferral WHERE issue_id = $1`, f.issue)
	if n > 0 {
		f.QueryRow(t, `SELECT head_sha, reasons FROM admission_deferral WHERE issue_id = $1`, f.issue).Scan(&head, &reasons)
	}
	return head, reasons, n
}

func (f *admissionFixture) wantDeferred(t *testing.T, reasons ...string) {
	t.Helper()
	if err := f.guard(f.issue); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("guard err = %v, want ErrAdmissionDeferred", err)
	}
	head, got, n := f.deferral(t)
	if n != 1 || head != "h1" || !reflect.DeepEqual(got, reasons) {
		t.Fatalf("deferral = (head %q, reasons %v, rows %d), want (h1, %v, 1)", head, got, n, reasons)
	}
}

func TestAdmissionDefersWhileCIPending(t *testing.T) {
	f := newAdmissionFixture(t)
	f.check(t, "h1", "build", "in_progress", "")
	f.check(t, "h1", "lint", "completed", "success")
	f.check(t, "h1", "security-gate", "completed", "success")
	f.wantDeferred(t, "check:build:pending")
}

func TestAdmissionDefersWhileCIRed(t *testing.T) {
	f := newAdmissionFixture(t)
	f.check(t, "h1", "build", "completed", "failure")
	f.check(t, "h1", "lint", "completed", "success")
	f.wantDeferred(t, "check:build:failing")
}

func TestAdmissionDefersWhileCheckMissing(t *testing.T) {
	f := newAdmissionFixture(t)
	f.check(t, "h1", "build", "completed", "success")
	f.check(t, "h0", "lint", "completed", "success") // an old head's run does not count
	f.wantDeferred(t, "check:lint:missing")
}

func TestAdmissionDefersWhileDirty(t *testing.T) {
	f := newAdmissionFixture(t)
	f.green(t)
	f.Exec(t, `UPDATE github_pull_request SET api_merge_state_status = 'DIRTY' WHERE id = $1`, f.pr)
	f.wantDeferred(t, "merge_state:DIRTY")
	// A DIRTY verdict fetched for an older head says nothing about this one.
	f.Exec(t, `UPDATE github_pull_request SET snapshot_head_sha = 'h0' WHERE id = $1`, f.pr)
	if err := f.guard(f.issue); err != nil {
		t.Fatalf("stale DIRTY verdict deferred the run: %v", err)
	}
}

// CHE-4615 recovery test 7: with CI green and security-gate pending the
// reviewer starts; the same run with a failing CI check is deferred.
func TestAdmissionIgnoresReviewerDependentCheck(t *testing.T) {
	f := newAdmissionFixture(t)
	f.green(t)
	f.check(t, "h1", "security-gate", "in_progress", "")
	if err := f.guard(f.issue); err != nil {
		t.Fatalf("security-gate pending deferred the reviewer: %v", err)
	}
	if _, _, n := f.deferral(t); n != 0 {
		t.Fatalf("a deferral was stored for an admissible head")
	}
	f.Exec(t, `UPDATE github_pull_request_check_run SET conclusion = 'failure' WHERE pr_id = $1 AND name = 'build'`, f.pr)
	f.wantDeferred(t, "check:build:failing")
}

func TestAdmissionGuardScope(t *testing.T) {
	for name, setup := range map[string]func(f *admissionFixture){
		"guard off without a configured list": func(*admissionFixture) { admissionChecks = nil },
		"not a review status": func(f *admissionFixture) {
			f.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, f.issue)
		},
		"two open pull requests": func(f *admissionFixture) { f.link(t, f.issue, time.Hour) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t)
			f.check(t, "h1", "build", "completed", "failure")
			setup(f)
			if err := f.guard(f.issue); errors.Is(err, ErrAdmissionDeferred) {
				t.Fatalf("deferred outside the guard's scope")
			}
			if _, _, n := f.deferral(t); n != 0 {
				t.Fatalf("a deferral was stored outside the guard's scope")
			}
		})
	}
}

// A repeat deferral keeps the 24 h bound; a new head replaces the record.
func TestAdmissionRecordKeepsBoundAndFollowsHead(t *testing.T) {
	f := newAdmissionFixture(t)
	f.check(t, "h1", "build", "completed", "failure")
	f.wantDeferred(t, "check:build:failing", "check:lint:missing")
	var created, expires time.Time
	f.QueryRow(t, `SELECT created_at, expires_at FROM admission_deferral WHERE issue_id = $1`, f.issue).Scan(&created, &expires)
	if d := expires.Sub(created); d != 24*time.Hour {
		t.Fatalf("expiry bound = %v, want 24h", d)
	}
	f.Exec(t, `UPDATE admission_deferral SET created_at = created_at - interval '1 hour', expires_at = expires_at - interval '1 hour' WHERE issue_id = $1`, f.issue)
	f.wantDeferred(t, "check:build:failing", "check:lint:missing")
	var created2 time.Time
	f.QueryRow(t, `SELECT created_at FROM admission_deferral WHERE issue_id = $1`, f.issue).Scan(&created2)
	if !created2.Equal(created.Add(-time.Hour)) {
		t.Fatalf("a repeat trigger moved created_at")
	}

	f.Exec(t, `UPDATE github_pull_request SET head_sha = 'h2', snapshot_head_sha = 'h2' WHERE id = $1`, f.pr)
	if err := f.guard(f.issue); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("new head err = %v, want ErrAdmissionDeferred", err)
	}
	head, reasons, n := f.deferral(t)
	if n != 1 || head != "h2" || len(reasons) != 2 {
		t.Fatalf("after a new head: head %q reasons %v rows %d, want one record for h2", head, reasons, n)
	}
}

// Through the real enqueue: no task, and the record outlives the rollback.
func TestAdmissionRealEnqueueStoresDeferral(t *testing.T) {
	f := newAdmissionFixture(t)
	f.check(t, "h1", "build", "in_progress", "")
	f.check(t, "h1", "lint", "completed", "success")
	svc := f.service(t)
	issue, err := f.q.GetIssue(context.Background(), util.MustParseUUID(f.issue))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("enqueue err = %v, want ErrAdmissionDeferred", err)
	}
	if got := f.tasksOn(t, f.issue); got != 0 {
		t.Fatalf("%d tasks started on a deferred head", got)
	}
	if head, reasons, n := f.deferral(t); n != 1 || head != "h1" || fmt.Sprint(reasons) != "[check:build:pending]" {
		t.Fatalf("deferral = (%q, %v, %d)", head, reasons, n)
	}
}
