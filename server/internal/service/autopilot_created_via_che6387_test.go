package service

import (
	"testing"
)

// CHE-6387: an issue made by an autopilot dispatch stores created_via=autopilot
// (it stored NULL before).
func TestAutopilotCreateIssue_StoresCreatedViaAutopilot(t *testing.T) {
	fx, creator := newPrincipalFixture(t)
	agentID := fx.privateAgentOwnedBy(t, creator, "created-via-ap")
	autopilotID, triggerID := fx.createIssueAutopilot(t, agentID, creator, "member", creator)

	run := fx.dispatch(t, autopilotID, triggerID)
	if !run.IssueID.Valid {
		t.Fatalf("dispatch created no issue (status %q, reason %q)", run.Status, run.FailureReason.String)
	}
	var got *string
	fx.QueryRow(t, `SELECT created_via FROM issue WHERE id = $1`, run.IssueID).Scan(&got)
	if got == nil || *got != "autopilot" {
		t.Fatalf("created_via = %v, want autopilot", got)
	}
}
