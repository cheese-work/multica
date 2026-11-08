package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/stagequeue"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

const admissionHead = "aaaa1111"

type admissionFixture struct {
	*stageFixture
	issue, pr string
	checks    int
}

// newAdmissionFixture is an agent_review issue with a linked open PR at
// admissionHead whose merge state is mergeState. "ci" is the only admission
// check.
func newAdmissionFixture(t *testing.T, mergeState string) *admissionFixture {
	t.Helper()
	prev := admissionChecks
	admissionChecks = []string{"ci"}
	t.Cleanup(func() { admissionChecks = prev })
	f := &admissionFixture{stageFixture: newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 10})}
	f.issue = f.Issue(t, "PR under review", testutil.Cols{"status": "agent_review", "assignee_type": "agent", "assignee_id": f.agentID})
	f.pr = f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 1, "repo_owner": "o", "repo_name": "r",
		"pr_number": int(time.Now().UnixNano() % 1000000000), "title": "t", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": time.Now(), "pr_updated_at": time.Now(),
		"head_sha": admissionHead, "api_merge_state_status": mergeState,
	})
	f.InsertNoID(t, "issue_pull_request", testutil.Cols{"issue_id": f.issue, "pull_request_id": f.pr},
		"issue_id = $1 AND pull_request_id = $2", f.issue, f.pr)
	f.Cleanup(t, `DELETE FROM review_admission_deferral WHERE issue_id = $1`, f.issue)
	return f
}

func (f *admissionFixture) check(t *testing.T, name, status, conclusion string) {
	t.Helper()
	f.checks++
	cols := testutil.Cols{"pr_id": f.pr, "head_sha": admissionHead, "ordinal": f.checks, "name": name, "status": status}
	if conclusion != "" {
		cols["conclusion"] = conclusion
	}
	f.InsertNoID(t, "github_pull_request_check_run", cols, "pr_id = $1 AND ordinal = $2", f.pr, f.checks)
}

// deferral reads the stored record; ok is false when none is stored.
func (f *admissionFixture) deferral(t *testing.T) (head string, reasons []string, window time.Duration, ok bool) {
	t.Helper()
	var created, expires time.Time
	err := f.Pool.QueryRow(context.Background(),
		`SELECT head_sha, reasons, created_at, expires_at FROM review_admission_deferral WHERE issue_id = $1 AND agent_id = $2`,
		f.issue, f.agentID).Scan(&head, &reasons, &created, &expires)
	if err != nil {
		return "", nil, 0, false
	}
	return head, reasons, expires.Sub(created), true
}

func (f *admissionFixture) wantDeferred(t *testing.T, reason string) {
	t.Helper()
	if err := f.guard(f.issue); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("guard err = %v, want ErrAdmissionDeferred", err)
	}
	head, reasons, window, ok := f.deferral(t)
	if !ok {
		t.Fatal("no deferral record stored")
	}
	if head != admissionHead || window != 24*time.Hour {
		t.Fatalf("record head %q window %v, want %q and 24h", head, window, admissionHead)
	}
	for _, r := range reasons {
		if r == reason {
			return
		}
	}
	t.Fatalf("record reasons %v lack %q", reasons, reason)
}

func (f *admissionFixture) wantAdmitted(t *testing.T) {
	t.Helper()
	if err := f.guard(f.issue); err != nil {
		t.Fatalf("guard err = %v, want the run admitted", err)
	}
	if _, _, _, ok := f.deferral(t); ok {
		t.Fatal("a deferral record was stored for an admitted run")
	}
}

func TestAdmissionDefersCIPending(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "in_progress", "")
	f.wantDeferred(t, "ci: pending")
}

func TestAdmissionDefersCIFailing(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "completed", "failure")
	f.wantDeferred(t, "ci: failing")
}

func TestAdmissionDefersMissingCheck(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.wantDeferred(t, "ci: missing")
}

func TestAdmissionDefersDirtyPR(t *testing.T) {
	f := newAdmissionFixture(t, "DIRTY")
	f.check(t, "ci", "completed", "success")
	f.wantDeferred(t, "merge conflict")
}

// CHE-4615 recovery test 7: a reviewer-dependent check never counts.
func TestAdmissionIgnoresReviewerDependentCheck(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "completed", "success")
	f.check(t, "security-gate", "in_progress", "")
	f.wantAdmitted(t)
}

func TestAdmissionFailingCIDefersDespitePendingSecurityGate(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "completed", "failure")
	f.check(t, "security-gate", "in_progress", "")
	f.wantDeferred(t, "ci: failing")
}

// Naming the gate in the config does not make it count.
func TestAdmissionNeverCountsSecurityGateEvenWhenConfigured(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	admissionChecks = admissionChecksFromEnv(func(string) string { return "ci, security-gate" })
	f.check(t, "ci", "completed", "success")
	f.check(t, "security-gate", "completed", "failure")
	f.wantAdmitted(t)
}

func TestAdmissionAdmitsGreenHead(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "completed", "success")
	f.wantAdmitted(t)
}

// A check stored for an older head is not evidence for this head.
func TestAdmissionStaleHeadCheckIsMissing(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.Exec(t, `UPDATE github_pull_request SET head_sha = 'bbbb2222' WHERE id = $1`, f.pr)
	f.check(t, "ci", "completed", "success")
	if err := f.guard(f.issue); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("guard err = %v, want ErrAdmissionDeferred", err)
	}
}

// Only the reviewer's own run is judged; an author run is never deferred.
func TestAdmissionLeavesNonReviewRunsAlone(t *testing.T) {
	f := newAdmissionFixture(t, "DIRTY")
	_, err := guardIssueRunGate(context.Background(), f.q, util.MustParseUUID(f.issue), OriginDerived,
		RunTarget{AgentID: util.MustParseUUID(f.agentID), Trigger: "comment"})
	if err != nil {
		t.Fatalf("non-review run: err = %v, want nil", err)
	}
}
