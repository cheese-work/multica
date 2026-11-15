package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
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
