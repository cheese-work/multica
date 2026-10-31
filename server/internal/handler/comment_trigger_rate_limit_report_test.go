package handler

import (
	"context"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A rate-limited trigger with no explicit mention target still reports its
// blocked outcome (CHE-2151), so trigger_outcomes never hides a refused wake.
// No current route produces such a trigger, so the test removes the target.
func TestRateLimitedTriggerWithoutTargetIsReported(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, true)

	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)
	authorTaskID := createAgentCommentTaskForPolicyTest(t, fx.LeaderID, issueID)
	content := "[@Worker](mention://agent/" + fx.OtherID + ") please review"
	if first := postAgentCommentForTriggerPolicyTest(t, issueID, fx.LeaderID, authorTaskID, content); len(first) != 1 || first[0].Status != DispatchQueued {
		t.Fatalf("first comment outcomes = %+v, want queued", first)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE issue_id=$1 AND agent_id=$2`, issueID, fx.OtherID); err != nil {
		t.Fatalf("complete first run: %v", err)
	}

	original := computeCommentTriggers
	computeCommentTriggers = func(h *Handler, ctx context.Context, issue db.Issue, content string, parent *db.Comment, actorType, actorID string, opts commentTriggerComputeOptions) ([]commentAgentTrigger, []commentMentionTarget) {
		triggers, targets := original(h, ctx, issue, content, parent, actorType, actorID, opts)
		kept := targets[:0]
		for _, target := range targets {
			if target.ExecAgentID != fx.OtherID {
				kept = append(kept, target)
			}
		}
		return triggers, kept
	}
	t.Cleanup(func() { computeCommentTriggers = original })

	outcomes := postAgentCommentForTriggerPolicyTest(t, issueID, fx.LeaderID, authorTaskID, content)
	if len(outcomes) != 1 || outcomes[0].TargetType != "agent" || outcomes[0].TargetID != fx.OtherID ||
		outcomes[0].Status != DispatchBlocked || outcomes[0].ReasonCode != ReasonCommentTriggerRateLimited {
		t.Fatalf("rate-limited trigger without a target: outcomes = %+v, want one blocked comment_trigger_rate_limited for %s", outcomes, fx.OtherID)
	}
}
