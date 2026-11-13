package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/stagequeue"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// An app decision resumes the agent through the same run gate as every other
// derived run: a paused agent starts nothing, and an open gate starts exactly
// one run. (Upstream v0.8.2 enqueues without the fork's gate; CHE-5321.)
func TestEnqueueAppDecisionRunUsesTheRunGate(t *testing.T) {
	f := newStageFixture(t, stagequeue.Limits{})
	svc := f.service(t)
	issue := f.Issue(t, "needs a decision", testutil.Cols{"status": "in_progress", "assignee_type": "agent", "assignee_id": f.agentID})
	ctx := context.Background()
	enqueue := func() error {
		return svc.EnqueueAppDecisionRun(ctx, util.MustParseUUID(issue), util.MustParseUUID(f.agentID), util.MustParseUUID(f.UserID), pgtype.UUID{}, "approved")
	}

	f.Exec(t, `UPDATE agent SET scheduling_paused_at = now() WHERE id = $1`, f.agentID)
	if err := enqueue(); !errors.Is(err, ErrAgentPaused) {
		t.Fatalf("paused agent: err = %v, want ErrAgentPaused", err)
	}
	f.wantNoTask(t, issue, "paused agent")

	f.Exec(t, `UPDATE agent SET scheduling_paused_at = NULL WHERE id = $1`, f.agentID)
	f.Exec(t, `UPDATE issue SET triage_state = 'pending' WHERE id = $1`, issue)
	if err := enqueue(); !errors.Is(err, ErrIssueInTriage) {
		t.Fatalf("issue in triage: err = %v, want ErrIssueInTriage", err)
	}
	f.wantNoTask(t, issue, "issue in triage")

	f.Exec(t, `UPDATE issue SET triage_state = NULL WHERE id = $1`, issue)
	if err := enqueue(); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if got := f.tasksOn(t, issue); got != 1 {
		t.Fatalf("open gate: %d tasks, want 1", got)
	}
}

func (f *stageFixture) wantNoTask(t *testing.T, issue, why string) {
	t.Helper()
	if got := f.tasksOn(t, issue); got != 0 {
		t.Fatalf("%s: %d tasks, want 0", why, got)
	}
}
