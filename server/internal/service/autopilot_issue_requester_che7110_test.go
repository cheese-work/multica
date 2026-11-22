package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-7110: a scheduled create_issue autopilot made an issue with no requester,
// so `multica chat attention --stale` (CHE-6360, requester-scoped) was refused
// on the morning brief. The issue is made for the human the dispatch runs as.

func (f principalFixture) createIssueAutopilot(t *testing.T, agentID, apCreatorID string, trigCreatorType, trigCreatorID any) (string, string) {
	t.Helper()
	n := principalSeq.Add(1)
	autopilotID := f.Insert(t, "autopilot", testutil.Cols{
		"workspace_id":    f.WorkspaceID,
		"title":           fmt.Sprintf("requester-ap-%d", n),
		"assignee_type":   "agent",
		"assignee_id":     agentID,
		"status":          "active",
		"execution_mode":  "create_issue",
		"created_by_type": "member",
		"created_by_id":   apCreatorID,
	})
	return autopilotID, f.trigger(t, autopilotID, trigCreatorType, trigCreatorID)
}

func (f principalFixture) dispatchedIssueRequester(t *testing.T, autopilotID, triggerID string) pgtype.UUID {
	t.Helper()
	run := f.dispatch(t, autopilotID, triggerID)
	if !run.IssueID.Valid {
		t.Fatalf("dispatch created no issue (status %q, reason %q)", run.Status, run.FailureReason.String)
	}
	var requester pgtype.UUID
	f.QueryRow(t, `SELECT requested_by_id FROM issue WHERE id = $1`, run.IssueID).Scan(&requester)
	return requester
}

func TestAutopilotCreateIssue_RequesterIsTheTriggerCreator(t *testing.T) {
	fx, creatorA := newPrincipalFixture(t)
	triggerOwnerB := fx.member(t, "requester-b")
	agentID := fx.privateAgentOwnedBy(t, triggerOwnerB, "requester-b")
	autopilotID, triggerID := fx.createIssueAutopilot(t, agentID, creatorA, "member", triggerOwnerB)

	got := fx.dispatchedIssueRequester(t, autopilotID, triggerID)

	if util.UUIDToString(got) != triggerOwnerB {
		t.Fatalf("issue requested_by_id = %q, want the trigger creator %q", util.UUIDToString(got), triggerOwnerB)
	}
}

func TestAutopilotCreateIssue_NoPrincipalLeavesNoRequester(t *testing.T) {
	fx, creatorA := newPrincipalFixture(t)
	agentID := fx.privateAgentOwnedBy(t, creatorA, "requester-legacy")
	// Legacy trigger: created_by is NULL, so no human can be named.
	autopilotID, triggerID := fx.createIssueAutopilot(t, agentID, creatorA, nil, nil)

	run := fx.dispatch(t, autopilotID, triggerID)
	if run.IssueID.Valid {
		var requester pgtype.UUID
		fx.QueryRow(t, `SELECT requested_by_id FROM issue WHERE id = $1`, run.IssueID).Scan(&requester)
		if requester.Valid {
			t.Fatalf("issue requested_by_id = %q, want none", util.UUIDToString(requester))
		}
	}
}

func TestAutopilotCreateIssue_ManualRunRequesterIsTheClicker(t *testing.T) {
	fx, creatorA := newPrincipalFixture(t)
	clicker := fx.member(t, "requester-clicker")
	agentID := fx.privateAgentOwnedBy(t, clicker, "requester-clicker")
	autopilotID, triggerID := fx.createIssueAutopilot(t, agentID, creatorA, "member", creatorA)

	ap, err := fx.q.GetAutopilot(context.Background(), util.MustParseUUID(autopilotID))
	if err != nil {
		t.Fatalf("load autopilot: %v", err)
	}
	run, _, err := fx.svc.DispatchAutopilotManual(context.Background(), ap, util.MustParseUUID(triggerID), nil, util.MustParseUUID(clicker))
	if err != nil || run == nil || !run.IssueID.Valid {
		t.Fatalf("manual dispatch: run=%v err=%v", run, err)
	}
	var requester pgtype.UUID
	fx.QueryRow(t, `SELECT requested_by_id FROM issue WHERE id = $1`, run.IssueID).Scan(&requester)
	if util.UUIDToString(requester) != clicker {
		t.Fatalf("issue requested_by_id = %q, want the clicking member %q", util.UUIDToString(requester), clicker)
	}
}
