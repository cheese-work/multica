package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestCommentEnqueueFailureReasonTypesDispatchRefusals (CHE-4012): a refused
// enqueue is not an internal fault. The stage queue, the run budget and triage
// refusals keep the typed reason the rerun path already reports; only an
// unknown error stays internal_error.
func TestCommentEnqueueFailureReasonTypesDispatchRefusals(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want DispatchReasonCode
	}{
		{"stage queue full", service.ErrStageQueueFull, ReasonStageQueueFull},
		{"stage queue full, wrapped", fmt.Errorf("enqueue: %w", service.ErrStageQueueFull), ReasonStageQueueFull},
		{"run budget exhausted", service.ErrRunBudgetExhausted, ReasonRunBudgetExhausted},
		{"issue in triage", service.ErrIssueInTriage, ReasonIssueInTriage},
		{"rate limited", service.ErrCommentTriggerRateLimited, ReasonCommentTriggerRateLimited},
		{"attribution fail-closed", service.ErrAttributionFailClosed, ReasonAttributionBlocked},
		{"unknown error stays internal", errors.New("db down"), ReasonInternalError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commentEnqueueFailureReason(tc.err); got != tc.want {
				t.Errorf("commentEnqueueFailureReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

func failQueries(t *testing.T, failSQL string) {
	t.Helper()
	prev := testHandler.Queries
	testHandler.Queries = db.New(failQueryDBTX{DBTX: testPool, failOn: failSQL, err: errors.New("forced db failure")})
	t.Cleanup(func() { testHandler.Queries = prev })
}

// TestCommentAdmissionInternalErrorBranchesLog (CHE-4012): every comment
// admission branch that reports internal_error writes one warning with the ids
// and a branch name, so the reason code never needs the error text.
func TestCommentAdmissionInternalErrorBranchesLog(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name       string
		failSQL    string
		content    func(fx squadCommentTriggerFixture) string
		wantBranch string
	}{
		{
			name:       "agent mention pending-task check",
			failSQL:    "FROM agent_task_queue",
			content:    func(fx squadCommentTriggerFixture) string { return "[@Worker](mention://agent/" + fx.OtherID + ") go" },
			wantBranch: "agent_mention_pending_check",
		},
		{
			name:       "squad mention pending-task check",
			failSQL:    "FROM agent_task_queue",
			content:    func(fx squadCommentTriggerFixture) string { return "[@Squad](mention://squad/" + fx.SquadID + ") go" },
			wantBranch: "squad_mention_pending_check",
		},
		{
			name:       "project activity check",
			failSQL:    "FROM issue\nWHERE id = $1",
			content:    func(fx squadCommentTriggerFixture) string { return "[@Worker](mention://agent/" + fx.OtherID + ") go" },
			wantBranch: "project_active_check",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSquadCommentTriggerFixture(t)
			commentID := insertDupRaceComment(t, uuidToString(fx.Issue.ID), "admission probe", "1 minute")
			opts := commentTriggerComputeOptions{ExcludeTriggerCommentID: util.MustParseUUID(commentID), ThreadCommentID: util.MustParseUUID(commentID)}
			logs := captureWarnings(t)
			failQueries(t, tc.failSQL)

			_, targets := testHandler.computeCommentAgentTriggers(context.Background(), fx.Issue, tc.content(fx), nil, "member", testUserID, opts)

			if len(targets) != 1 || targets[0].ReasonCode != ReasonInternalError {
				t.Fatalf("targets = %+v, want one internal_error target", targets)
			}
			out := logs.String()
			for _, want := range []string{
				"level=WARN",
				"issue_id=" + uuidToString(fx.Issue.ID),
				"comment_id=" + commentID,
				"target_type=",
				"target_id=",
				"branch=" + tc.wantBranch,
				"forced db failure",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("warning missing %q; logs:\n%s", want, out)
				}
			}
			if n := strings.Count(out, "branch="+tc.wantBranch); n != 1 {
				t.Errorf("branch %s logged %d times, want 1; logs:\n%s", tc.wantBranch, n, out)
			}
		})
	}
}

// failFromNthTx lets the first n-1 transactions begin and fails every later one.
type failFromNthTx struct {
	txStarter
	failFrom int
	calls    int
}

func (s *failFromNthTx) Begin(ctx context.Context) (pgx.Tx, error) {
	s.calls++
	if s.calls >= s.failFrom {
		return nil, errors.New("forced tx failure")
	}
	return s.txStarter.Begin(ctx)
}

func failTxFrom(t *testing.T, n int) {
	t.Helper()
	prev := testHandler.TxStarter
	testHandler.TxStarter = &failFromNthTx{txStarter: prev, failFrom: n}
	t.Cleanup(func() { testHandler.TxStarter = prev })
}

// TestCommentEnqueueInternalErrorBranchesLog (CHE-4012): the branches of
// resolveCommentTriggerEnqueue that report internal_error each write one warning
// with issue_id, comment_id, target_type, target_id, a branch name and the error.
func TestCommentEnqueueInternalErrorBranchesLog(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name       string
		pending    bool
		nonLeader  bool
		archived   bool
		failSQL    string
		failTxFrom int
		wantBranch string
		wantErr    string
	}{
		{name: "enqueue fails with an unclassified error", archived: true, wantBranch: "enqueue_failed", wantErr: "agent is archived"},
		{name: "pending-task merge fails", pending: true, failTxFrom: 1, wantBranch: "merge_failed", wantErr: "comment merge into the pending task failed"},
		{name: "planned-comment registration fails", pending: true, nonLeader: true, failTxFrom: 2, wantBranch: "register_planned_failed", wantErr: "forced tx failure"},
		{name: "active-task check fails after a missed merge", pending: true, failSQL: "has_active", wantBranch: "active_task_check", wantErr: "forced db failure"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			agentID, issueID, _ := dupRaceFixture(t, fmt.Sprintf("che4012-log-%d", i), 999400+i)
			issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
			if err != nil {
				t.Fatalf("load issue: %v", err)
			}
			if tc.archived {
				dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, agentID)
			}
			agent, err := testHandler.Queries.GetAgent(ctx, util.MustParseUUID(agentID))
			if err != nil {
				t.Fatalf("load agent: %v", err)
			}
			commentID := insertDupRaceComment(t, issueID, "enqueue probe", "1 minute")
			trigger := commentAgentTrigger{Agent: agent, Source: commentTriggerSourceMentionAgent, AlreadyPending: tc.pending, NonLeaderAgentReply: tc.nonLeader}
			logs := captureWarnings(t)
			if tc.failSQL != "" {
				failQueries(t, tc.failSQL)
			}
			if tc.failTxFrom > 0 {
				failTxFrom(t, tc.failTxFrom)
			}

			res := testHandler.enqueueCommentAgentTriggers(ctx, issue, util.MustParseUUID(commentID), []commentAgentTrigger{trigger})[agentID]

			if res.status != DispatchBlocked || res.reason != ReasonInternalError {
				t.Fatalf("outcome = %q/%q, want blocked/internal_error", res.status, res.reason)
			}
			assertAdmissionWarning(t, logs.String(), issueID, commentID, agentID, tc.wantBranch, tc.wantErr)
		})
	}
}

func assertAdmissionWarning(t *testing.T, out, issueID, commentID, targetID, branch, wantErr string) {
	t.Helper()
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "comment admission reported internal_error") && strings.Contains(l, "branch="+branch) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no internal_error warning for branch %s; logs:\n%s", branch, out)
	}
	for _, want := range []string{"level=WARN", "issue_id=" + issueID, "comment_id=" + commentID, "target_type=agent", "target_id=" + targetID, wantErr} {
		if !strings.Contains(line, want) {
			t.Errorf("warning missing %q: %s", want, line)
		}
	}
	if n := strings.Count(out, "branch="+branch); n != 1 {
		t.Errorf("branch %s logged %d times, want 1; logs:\n%s", branch, n, out)
	}
}

// TestCreateCommentMentionAtFullReviewStageReportsStageQueueFull (CHE-4012): the
// whole HTTP path. An agent mention that the stage queue refuses answers
// trigger_outcomes {blocked, stage_queue_full}, never internal_error.
func TestCreateCommentMentionAtFullReviewStageReportsStageQueueFull(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	createTestCustomStatus(t, "agent_review", "started")
	agentID := seededReadyAgentID(t)
	reviewerFull(t, agentID, 2)
	issueID := dbfx.Issue(t, "stage queue mention", testutil.Cols{
		"status": "agent_review", "assignee_type": "agent", "assignee_id": agentID,
	})
	linkOpenPR(t, issueID, "now()")

	// A person's named mention is never stage-refused (OriginNamed); an agent's
	// mention is derived and is.
	authorID := createHandlerTestAgent(t, "che4012-mention-author", nil)
	taskID := createAgentCommentTaskForPolicyTest(t, authorID, issueID)
	outcomes := postAgentCommentForTriggerPolicyTest(t, issueID, authorID, taskID,
		"[@Reviewer](mention://agent/"+agentID+") please review")

	got := findCommentOutcome(t, outcomes, agentID)
	if got.Status != DispatchBlocked || got.ReasonCode != ReasonStageQueueFull {
		t.Fatalf("outcome = %+v, want blocked/stage_queue_full", got)
	}
}
