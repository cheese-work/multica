package handler

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// A batch that moves status AND writes labels or properties is one atomic
// transaction per issue. It must still pass the locked workflow decision and
// the entry gates the status-only path checks (CHE-6566 A3c review P1): when
// the gate blocks, neither the status nor the label/property change commits.
func TestEntryGateMixedBatchWithLabelOrProperty(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	installationID := gateInstall + 710
	c := newEdgeCase(t, installationID, gateSteps(gate("pr.ready", "block")))
	// A draft PR is not ready: the in_review entry gate blocks.
	linkGatePRs(t, c, installationID, draftPR)
	c.enter(t, "in_progress")
	label := createTestIssueLabel(t, "gate-mixed-"+uuid.NewString()[:8])
	prop := createTestProperty(t, map[string]any{"name": "Gate mixed " + uuid.NewString()[:8], "type": "text"})

	for name, extra := range map[string]map[string]any{
		"label":    {"labels": map[string]any{"add": []string{label}}},
		"property": {"properties": map[string]any{"set": map[string]any{prop.ID: "slipped past the gate"}}},
	} {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{
				"issue_ids": []string{c.issueID},
				"updates":   map[string]any{"status": "in_review"},
			}
			for k, v := range extra {
				body[k] = v
			}
			var out struct {
				Updated int `json:"updated"`
				Failed  []struct {
					ID   string `json:"id"`
					Code string `json:"code"`
				} `json:"failed"`
			}
			req := newRequest("POST", "/api/issues/batch?workspace_id="+testWorkspaceID, body)
			testutil.Call(t, testHandler.BatchUpdateIssues, req).Want(http.StatusOK).JSON(&out)
			if out.Updated != 0 || len(out.Failed) != 1 || out.Failed[0].ID != c.issueID || out.Failed[0].Code != "entry_gate_failed" {
				t.Fatalf("updated=%d failed=%+v, want 0 updated and entry_gate_failed for %s", out.Updated, out.Failed, c.issueID)
			}
			wantIssueStatus(t, c.issueID, "in_progress")
			if got := issueLabelSet(t, c.issueID); got[label] {
				t.Errorf("blocked batch still attached the label: %v", got)
			}
			if got, ok := issuePropertyValue(t, c.issueID, prop.ID); ok {
				t.Errorf("blocked batch still set the property: %q", got)
			}
		})
	}
}
