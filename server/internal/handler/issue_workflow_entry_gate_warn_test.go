package handler

import (
	"net/http"
	"testing"
)

// The response to a write that posts the first-warning comment carries the
// revision after that comment, so the caller's next edit does not conflict
// with the platform's own comment (CHE-2531 fix round).
func TestEntryGateWarnResponseCarriesCurrentRevision(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	installationID := gateInstall + 800
	c := newEdgeCase(t, installationID, gateSteps(gate("pr.ready", "warn")))
	linkGatePRs(t, c, installationID, draftPR)
	c.enter(t, "in_progress")
	var moved struct {
		Revision int64 `json:"revision"`
	}
	putIssue(t, c.issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK).JSON(&moved)
	var stored int64
	dbfx.QueryRow(t, `SELECT revision FROM issue WHERE id=$1`, c.issueID).Scan(&stored)
	if moved.Revision != stored {
		t.Fatalf("response revision = %d, stored revision = %d", moved.Revision, stored)
	}
	putIssue(t, c.issueID, map[string]any{"title": "next edit", "expected_revision": moved.Revision}).Want(http.StatusOK)
}
