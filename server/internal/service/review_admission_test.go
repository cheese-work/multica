package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/stagequeue"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const admissionHead = "aaaa1111"

// setAdmissionReviewers sets the configured reviewer set for one test.
func setAdmissionReviewers(t *testing.T, ids ...string) {
	t.Helper()
	prev := admissionReviewers
	admissionReviewers = map[string]bool{}
	for _, id := range ids {
		admissionReviewers[id] = true
	}
	t.Cleanup(func() { admissionReviewers = prev })
}

// enableAdmission sets the deferral switch for one test.
func enableAdmission(t *testing.T, on bool) {
	t.Helper()
	prev := admissionEnabled
	admissionEnabled = on
	t.Cleanup(func() { admissionEnabled = prev })
}

type admissionFixture struct {
	*stageFixture
	issue, pr string
	author    string // the agent bound as the author of f.pr
	checks    int
}

// bindAuthor records agent as the author of pr in the workspace settings, the
// binding GitHubPullRequestAuthor reads first.
func (f *admissionFixture) bindAuthor(t *testing.T, pr, agent string) {
	t.Helper()
	var prev []byte
	if err := f.Pool.QueryRow(context.Background(), `SELECT settings FROM workspace WHERE id = $1`, f.WorkspaceID).Scan(&prev); err != nil {
		t.Fatalf("read workspace settings: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.Pool.Exec(context.Background(), `UPDATE workspace SET settings = $2 WHERE id = $1`, f.WorkspaceID, prev)
	})
	f.Exec(t, `UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'::jsonb), '{github_review_authors}',
		COALESCE(settings->'github_review_authors', '{}'::jsonb) ||
		(SELECT jsonb_build_object(lower(repo_owner) || '/' || lower(repo_name) || '#' || pr_number, $3::text)
		 FROM github_pull_request WHERE id = $2)) WHERE id = $1`, f.WorkspaceID, pr, agent)
}

// addPR links a second open PR at admissionHead to issue, with a failing "ci".
func (f *admissionFixture) addPR(t *testing.T, issue string) string {
	t.Helper()
	pr := f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 1, "repo_owner": "o", "repo_name": "r",
		"pr_number": int(time.Now().UnixNano()%1000000000) + 1, "title": "t", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": time.Now(), "pr_updated_at": time.Now(),
		"head_sha": admissionHead, "api_merge_state_status": "CLEAN",
	})
	f.InsertNoID(t, "issue_pull_request", testutil.Cols{"issue_id": issue, "pull_request_id": pr},
		"issue_id = $1 AND pull_request_id = $2", issue, pr)
	f.InsertNoID(t, "github_pull_request_check_run", testutil.Cols{
		"pr_id": pr, "head_sha": admissionHead, "ordinal": 1, "name": "ci", "status": "completed", "conclusion": "failure",
	}, "pr_id = $1 AND ordinal = $2", pr, 1)
	f.Cleanup(t, `DELETE FROM review_admission_deferral WHERE issue_id = $1`, issue)
	return pr
}

// newAdmissionFixture is an agent_review issue with a linked open PR at
// admissionHead whose merge state is mergeState. "ci" is the only admission
// check.
func newAdmissionFixture(t *testing.T, mergeState string) *admissionFixture {
	t.Helper()
	prev := admissionChecks
	admissionChecks = []string{"ci"}
	t.Cleanup(func() { admissionChecks = prev })
	enableAdmission(t, true)
	f := &admissionFixture{stageFixture: newStageFixture(t, stagequeue.Limits{stagequeue.StageReview: 10})}
	setAdmissionReviewers(t, f.agentID)
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
	// The PR's author is a third agent, so the configured reviewers are
	// reviewers here unless a test binds one of them as author.
	f.author = f.Agent(t, "PR author", f.runtime)
	f.bindAuthor(t, f.pr, f.author)
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
	return f.deferralOf(t, f.agentID)
}

func (f *admissionFixture) deferralOf(t *testing.T, agent string) (head string, reasons []string, window time.Duration, ok bool) {
	t.Helper()
	var created, expires time.Time
	err := f.Pool.QueryRow(context.Background(),
		`SELECT head_sha, reasons, created_at, expires_at FROM review_admission_deferral WHERE issue_id = $1 AND agent_id = $2`,
		f.issue, agent).Scan(&head, &reasons, &created, &expires)
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

// CHE-5432: a draft PR is judged like an open one. A draft head with green
// checks admits the review; a draft head with a red check still defers.
func TestAdmissionAdmitsGreenDraftHead(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.Exec(t, `UPDATE github_pull_request SET state = 'draft' WHERE id = $1`, f.pr)
	f.check(t, "ci", "completed", "success")
	f.wantAdmitted(t)
}

func TestAdmissionDefersRedDraftHead(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.Exec(t, `UPDATE github_pull_request SET state = 'draft' WHERE id = $1`, f.pr)
	f.check(t, "ci", "completed", "failure")
	f.wantDeferred(t, "ci: failing")
}

func TestAdmissionDefersPendingDraftHead(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.Exec(t, `UPDATE github_pull_request SET state = 'draft' WHERE id = $1`, f.pr)
	f.check(t, "ci", "in_progress", "")
	f.wantDeferred(t, "ci: pending")
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

// guardAs runs the guard for another agent on the review issue.
func (f *admissionFixture) guardAs(agent, trigger string, reviewRun bool) error {
	_, err := guardIssueRunGate(context.Background(), f.q, util.MustParseUUID(f.issue), OriginDerived,
		RunTarget{AgentID: util.MustParseUUID(agent), Trigger: trigger, ReviewRun: reviewRun})
	return err
}

// A security reviewer mentioned on an issue another agent holds is a reviewer
// run too: failing CI defers it, and recovery test 7 still starts it.
func TestAdmissionJudgesAReviewerMentionedOnAnotherAgentsIssue(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.Cleanup(t, `DELETE FROM review_admission_deferral WHERE issue_id = $1`, f.issue)
	security := f.Agent(t, "Security reviewer", f.runtime)
	setAdmissionReviewers(t, f.agentID, security)
	f.check(t, "ci", "completed", "failure")
	f.check(t, "security-gate", "in_progress", "")
	if err := f.guardAs(security, "mention", true); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("mention on red CI: err = %v, want ErrAdmissionDeferred", err)
	}
	if _, reasons, _, ok := f.deferralOf(t, security); !ok || len(reasons) != 1 || reasons[0] != "ci: failing" {
		t.Fatalf("stored reasons = %v ok=%v, want [ci: failing]", reasons, ok)
	}
	f.Exec(t, `UPDATE github_pull_request_check_run SET conclusion = 'success' WHERE pr_id = $1 AND name = 'ci'`, f.pr)
	if err := f.guardAs(security, "mention", true); err != nil {
		t.Fatalf("mention on green CI with a pending security-gate: err = %v, want admitted", err)
	}
}

// The reviewer set decides, not the issue state: a security reviewer mentioned
// while the issue is still in_progress is deferred on red CI (recovery test 7).
func TestAdmissionJudgesAReviewerWhileTheIssueIsInProgress(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	security := f.Agent(t, "Security reviewer", f.runtime)
	setAdmissionReviewers(t, security)
	f.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, f.issue)
	f.check(t, "ci", "completed", "failure")
	f.check(t, "security-gate", "in_progress", "")
	if err := f.guardAs(security, "mention", true); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("mention on red CI: err = %v, want ErrAdmissionDeferred", err)
	}
	f.Exec(t, `UPDATE github_pull_request_check_run SET conclusion = 'success' WHERE pr_id = $1 AND name = 'ci'`, f.pr)
	if err := f.guardAs(security, "mention", true); err != nil {
		t.Fatalf("mention on green CI with a pending security-gate: err = %v, want admitted", err)
	}
}

// An agent outside the reviewer set is never judged, even on a review-status
// issue: the author's run is what turns red CI green.
func TestAdmissionLeavesAgentsOutsideTheReviewerSetAlone(t *testing.T) {
	f := newAdmissionFixture(t, "DIRTY")
	author := f.Agent(t, "Author", f.runtime)
	f.check(t, "ci", "completed", "failure")
	for _, c := range []struct {
		trigger string
		review  bool
	}{{"mention", true}, {"comment", true}, {"wakeup", false}, {"system wakeup", false}} {
		if err := f.guardAs(author, c.trigger, c.review); err != nil {
			t.Fatalf("%s by a non-reviewer: err = %v, want nil", c.trigger, err)
		}
	}
	if _, _, _, ok := f.deferralOf(t, author); ok {
		t.Fatal("a deferral record was stored for an agent outside the reviewer set")
	}
}

// A system wakeup of a reviewer is judged like any other reviewer start.
func TestAdmissionDefersASystemWakeup(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "completed", "failure")
	if err := f.guardAs(f.agentID, "system wakeup", false); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("system wakeup on red CI: err = %v, want ErrAdmissionDeferred", err)
	}
}

// A person naming a reviewer in a wakeup is judged; naming one in a comment is
// still never refused.
func TestAdmissionJudgesANamedWakeupButNotANamedMention(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "completed", "failure")
	named := func(trigger string, review bool) error {
		_, err := guardIssueRunGate(context.Background(), f.q, util.MustParseUUID(f.issue), OriginNamed,
			RunTarget{AgentID: util.MustParseUUID(f.agentID), Trigger: trigger, ReviewRun: review})
		return err
	}
	if err := named("mention", true); err != nil {
		t.Fatalf("named mention: err = %v, want nil", err)
	}
	if err := named("wakeup", false); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("named wakeup on red CI: err = %v, want ErrAdmissionDeferred", err)
	}
}

// With no reviewer configured nobody is judged.
func TestAdmissionJudgesNobodyWithoutAReviewerSet(t *testing.T) {
	f := newAdmissionFixture(t, "DIRTY")
	setAdmissionReviewers(t)
	if err := f.guard(f.issue); err != nil {
		t.Fatalf("empty reviewer set: err = %v, want nil", err)
	}
}

func TestAdmissionReviewersFromEnv(t *testing.T) {
	got := admissionReviewersFromEnv(func(string) string { return " AAA-1, bbb-2 ,," })
	if len(got) != 2 || !got["aaa-1"] || !got["bbb-2"] {
		t.Fatalf("reviewers = %v, want aaa-1 and bbb-2", got)
	}
}

// A leader's callback is bookkeeping, never the review.
func TestAdmissionLeavesSquadLeaderCallbackAlone(t *testing.T) {
	f := newAdmissionFixture(t, "DIRTY")
	leader := f.Agent(t, "Leader", f.runtime)
	if err := f.guardAs(leader, "squad leader callback", true); err != nil {
		t.Fatalf("leader callback: err = %v, want nil", err)
	}
}

// A review wakeup is judged like any other reviewer start.
func TestAdmissionDefersAReviewWakeup(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "completed", "failure")
	if err := f.guardAs(f.agentID, "wakeup", false); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("wakeup on red CI: err = %v, want ErrAdmissionDeferred", err)
	}
}

// The wakeup transaction rolls back on a refusal; the deferral stays on record
// with the wakeup trigger.
func TestAdmissionWakeupTickKeepsTheDeferralRecord(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	prev := admissionChecks
	admissionChecks = []string{"ci"}
	t.Cleanup(func() { admissionChecks = prev })
	enableAdmission(t, true)
	setAdmissionReviewers(t, agent)
	f.Exec(t, `UPDATE issue SET status = 'in_review', assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, util.UUIDToString(issue), agent)
	pr := f.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": f.WorkspaceID, "installation_id": 1, "repo_owner": "o", "repo_name": "r",
		"pr_number": int(time.Now().UnixNano() % 1000000000), "title": "t", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": time.Now(), "pr_updated_at": time.Now(), "head_sha": admissionHead, "api_merge_state_status": "CLEAN",
	})
	f.InsertNoID(t, "issue_pull_request", testutil.Cols{"issue_id": util.UUIDToString(issue), "pull_request_id": pr},
		"issue_id = $1 AND pull_request_id = $2", util.UUIDToString(issue), pr)
	f.InsertNoID(t, "github_pull_request_check_run", testutil.Cols{"pr_id": pr, "head_sha": admissionHead, "ordinal": 1, "name": "ci", "status": "completed", "conclusion": "failure"},
		"pr_id = $1 AND ordinal = 1", pr)
	f.Cleanup(t, `DELETE FROM review_admission_deferral WHERE issue_id = $1`, util.UUIDToString(issue))
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", EventTypes: []string{"comment.created"}, Instruction: "Review latest changes"})
	f.Comment(t, util.UUIDToString(issue), "source")
	// A person created this wakeup, so the run is named: still judged.
	s.dispatchFromTick(context.Background(), w)
	var trigger, head string
	if err := f.Pool.QueryRow(context.Background(),
		`SELECT trigger, head_sha FROM review_admission_deferral WHERE issue_id = $1`, util.UUIDToString(issue)).Scan(&trigger, &head); err != nil {
		t.Fatalf("no deferral stored for the refused wakeup: %v", err)
	}
	if trigger != "wakeup" || head != admissionHead {
		t.Fatalf("record trigger %q head %q, want wakeup and %q", trigger, head, admissionHead)
	}
}

// Repeated triggers on one head never push the deadline out; a new head starts
// a new 24 h window.
func TestAdmissionRepeatedDeferralKeepsTheDeadline(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "completed", "failure")
	if err := f.guard(f.issue); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("first guard err = %v", err)
	}
	f.Exec(t, `UPDATE review_admission_deferral SET created_at = now() - interval '20 hours', expires_at = now() + interval '4 hours' WHERE issue_id = $1`, f.issue)
	if err := f.guard(f.issue); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("repeat guard err = %v", err)
	}
	var left time.Duration
	read := func() {
		if err := f.Pool.QueryRow(context.Background(),
			`SELECT expires_at - now() FROM review_admission_deferral WHERE issue_id = $1`, f.issue).Scan(&left); err != nil {
			t.Fatal(err)
		}
	}
	if read(); left > 5*time.Hour {
		t.Fatalf("same-head repeat left %v on the deadline, want it kept near 4h", left)
	}
	f.Exec(t, `UPDATE github_pull_request SET head_sha = 'bbbb2222' WHERE id = $1`, f.pr)
	if err := f.guard(f.issue); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("new-head guard err = %v", err)
	}
	if read(); left < 23*time.Hour {
		t.Fatalf("new head left %v on the deadline, want a fresh 24h", left)
	}
}

// One configured agent holds two roles, decided per PR by who authored that
// PR, never by the issue status or assignee. It authors PR A: its runs repair
// the red CI in every status and on every path. It reviews PR B, authored by
// someone else: the same red CI defers it, even as that issue's assignee.
func TestAdmissionJudgesTheRoleOnThePRNotTheAssignee(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "completed", "failure")
	f.bindAuthor(t, f.pr, f.agentID) // f.agentID authors PR A
	for _, status := range []string{"in_progress", "in_review", "agent_review"} {
		f.Exec(t, `UPDATE issue SET status = $2 WHERE id = $1`, f.issue, status)
		for _, c := range []struct {
			trigger string
			review  bool
		}{{"assignment", true}, {"comment", true}, {"mention", true}, {"wakeup", false}, {"system wakeup", false}} {
			if err := f.guardAs(f.agentID, c.trigger, c.review); err != nil {
				t.Fatalf("author %s in %s on red CI: err = %v, want admitted", c.trigger, status, err)
			}
		}
	}
	if _, _, _, ok := f.deferral(t); ok {
		t.Fatal("a deferral record was stored for an author run")
	}

	issueB := f.Issue(t, "PR B under review", testutil.Cols{"status": "in_progress", "assignee_type": "agent", "assignee_id": f.agentID})
	prB := f.addPR(t, issueB)
	f.bindAuthor(t, prB, f.author) // someone else authors PR B
	for _, c := range []struct {
		trigger string
		review  bool
	}{{"assignment", true}, {"mention", true}} {
		_, err := guardIssueRunGate(context.Background(), f.q, util.MustParseUUID(issueB), OriginDerived,
			RunTarget{AgentID: util.MustParseUUID(f.agentID), Trigger: c.trigger, ReviewRun: c.review})
		if !errors.Is(err, ErrAdmissionDeferred) {
			t.Fatalf("reviewer %s on PR B: err = %v, want ErrAdmissionDeferred", c.trigger, err)
		}
	}
	_, err := guardIssueRunGate(context.Background(), f.q, util.MustParseUUID(issueB), OriginNamed,
		RunTarget{AgentID: util.MustParseUUID(f.agentID), Trigger: "wakeup"})
	if !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("reviewer named wakeup on PR B: err = %v, want ErrAdmissionDeferred", err)
	}
}

// The author wakeup path too: a named wakeup of the PR author, in a review
// status, still starts on red CI.
func TestAdmissionAdmitsAnAuthorWakeupOnRedCI(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "completed", "failure")
	f.bindAuthor(t, f.pr, f.agentID)
	_, err := guardIssueRunGate(context.Background(), f.q, util.MustParseUUID(f.issue), OriginNamed,
		RunTarget{AgentID: util.MustParseUUID(f.agentID), Trigger: "wakeup"})
	if err != nil {
		t.Fatalf("named author wakeup on red CI: err = %v, want admitted", err)
	}
}

// No author on record: the exemption is skipped (not guessed from the
// assignee), the run is judged as a reviewer, and the record says why.
func TestAdmissionUnknownAuthorIsJudgedAndRecorded(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "ci", "completed", "failure")
	f.Exec(t, `UPDATE workspace SET settings = settings - 'github_review_authors' WHERE id = $1`, f.WorkspaceID)
	f.Exec(t, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, f.issue)
	if err := f.guardAs(f.agentID, "assignment", true); !errors.Is(err, ErrAdmissionDeferred) {
		t.Fatalf("assignee with no author on record: err = %v, want ErrAdmissionDeferred", err)
	}
	_, reasons, _, ok := f.deferral(t)
	want := []string{"ci: failing", "author: unresolved"}
	if !ok || len(reasons) != 2 || reasons[0] != want[0] || reasons[1] != want[1] {
		t.Fatalf("stored reasons = %v ok=%v, want %v", reasons, ok, want)
	}
}

// With the switch off (the default) a reviewer run starts as before, whatever
// the head looks like, and nothing is stored.
func TestAdmissionSwitchOffStartsTheReviewerRun(t *testing.T) {
	f := newAdmissionFixture(t, "DIRTY")
	f.check(t, "ci", "completed", "failure")
	enableAdmission(t, false)
	for _, c := range []struct {
		trigger string
		review  bool
	}{{"assignment", true}, {"mention", true}, {"wakeup", false}, {"system wakeup", false}} {
		if err := f.guardAs(f.agentID, c.trigger, c.review); err != nil {
			t.Fatalf("%s with the switch off: err = %v, want the run started", c.trigger, err)
		}
	}
	if _, _, _, ok := f.deferral(t); ok {
		t.Fatal("a deferral record was stored with the switch off")
	}
}

func TestAdmissionEnabledFromEnv(t *testing.T) {
	for _, c := range []struct {
		value string
		want  bool
	}{{"", false}, {"false", false}, {"0", false}, {"true", true}, {" TRUE ", true}, {"1", true}} {
		if got := admissionEnabledFromEnv(func(string) string { return c.value }); got != c.want {
			t.Fatalf("MULTICA_REVIEW_ADMISSION=%q: enabled = %v, want %v", c.value, got, c.want)
		}
	}
}

// CHE-5432: the stored snapshot is not evidence for the head until it was
// fetched for that exact head. A head judged on an empty or stale snapshot asks
// for a refresh; the refreshed snapshot then releases the deferred run.
func (f *admissionFixture) captureRefresh(t *testing.T) *[]db.GithubPullRequest {
	t.Helper()
	var asked []db.GithubPullRequest
	prev := admissionRefreshPR
	admissionRefreshPR = func(pr db.GithubPullRequest) { asked = append(asked, pr) }
	t.Cleanup(func() { admissionRefreshPR = prev })
	return &asked
}

func (f *admissionFixture) setSnapshot(t *testing.T, head string, age time.Duration) {
	t.Helper()
	f.Exec(t, `UPDATE github_pull_request SET snapshot_head_sha = $2, snapshot_fetched_at = now() - make_interval(secs => $3) WHERE id = $1`,
		f.pr, head, age.Seconds())
}

func TestAdmissionRefreshesEmptySnapshot(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	asked := f.captureRefresh(t)
	f.wantDeferred(t, "ci: missing")
	if len(*asked) != 1 || util.UUIDToString((*asked)[0].ID) != f.pr || (*asked)[0].HeadSha != admissionHead {
		t.Fatalf("refresh asked for %v, want exactly the PR %s at %s", *asked, f.pr, admissionHead)
	}
}

func TestAdmissionRefreshesSnapshotOfAnotherHead(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	asked := f.captureRefresh(t)
	f.setSnapshot(t, "bbbb2222", 0)
	f.wantDeferred(t, "ci: missing")
	if len(*asked) != 1 {
		t.Fatalf("refresh asked %d times, want once for a snapshot of another head", len(*asked))
	}
}

func TestAdmissionRefreshesOldSnapshot(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	asked := f.captureRefresh(t)
	f.setSnapshot(t, admissionHead, time.Hour)
	f.wantDeferred(t, "ci: missing")
	if len(*asked) != 1 {
		t.Fatalf("refresh asked %d times, want once for an old snapshot", len(*asked))
	}
}

// A fresh snapshot of this head is judged as stored: asking again on every
// release would loop a refresh against a persistently red head.
func TestAdmissionDoesNotRefreshFreshSnapshot(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	asked := f.captureRefresh(t)
	f.setSnapshot(t, admissionHead, 0)
	f.check(t, "ci", "completed", "failure")
	f.wantDeferred(t, "ci: failing")
	if len(*asked) != 0 {
		t.Fatalf("refresh asked %d times for a fresh snapshot, want none", len(*asked))
	}
}

// An admitted head asks for nothing.
func TestAdmissionDoesNotRefreshAdmittedHead(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	asked := f.captureRefresh(t)
	f.check(t, "ci", "completed", "success")
	f.wantAdmitted(t)
	if len(*asked) != 0 {
		t.Fatalf("refresh asked %d times for an admitted head, want none", len(*asked))
	}
}

// CHE-5432: a repo whose CI reports none of the configured names is judged on
// every check it reports (at least one, none red or pending), not deferred as
// "missing" forever.
func TestAdmissionAdmitsRepoWithOtherCheckNames(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "lint", "completed", "success")
	f.check(t, "build", "completed", "success")
	f.wantAdmitted(t)
}

func TestAdmissionDefersRepoWithOtherCheckNamesPending(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "lint", "completed", "success")
	f.check(t, "build", "in_progress", "")
	f.wantDeferred(t, "build: pending")
}

func TestAdmissionDefersRepoWithOtherCheckNamesFailing(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "lint", "completed", "success")
	f.check(t, "build", "completed", "failure")
	f.wantDeferred(t, "build: failing")
}

func TestAdmissionDefersRepoWithOtherCheckNamesConflict(t *testing.T) {
	f := newAdmissionFixture(t, "DIRTY")
	f.check(t, "build", "completed", "success")
	f.wantDeferred(t, "merge conflict")
}

// The reviewer-dependent check waits on the reviewer, so it never counts as a
// reported check for the repo.
func TestAdmissionIgnoresReviewerDependentCheckInReportedSet(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "build", "completed", "success")
	f.check(t, "security-gate", "in_progress", "")
	f.wantAdmitted(t)
}

// Only the reviewer-dependent check reported: nothing else shows CI ran, so
// the configured names are still missing.
func TestAdmissionDefersWhenOnlyReviewerDependentCheckReported(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	f.check(t, "security-gate", "completed", "success")
	f.wantDeferred(t, "ci: missing")
}

// A repo that reports one configured name keeps the configured set: the other
// configured names stay required.
func TestAdmissionKeepsConfiguredSetWhenRepoReportsOne(t *testing.T) {
	f := newAdmissionFixture(t, "CLEAN")
	admissionChecks = []string{"ci", "e2e"}
	f.check(t, "ci", "completed", "success")
	f.check(t, "build", "completed", "success")
	f.wantDeferred(t, "e2e: missing")
}
