package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/browserlease"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// s2Fixture is a running task on an issue with an S2 profile, the real decision hook and an
// open user gate.
func s2Fixture(t *testing.T) (browserLeaseFixture, string) {
	t.Helper()
	f := newBrowserLeaseFixture(t, "running")
	q := db.New(testPool)
	prof, err := q.CreateBrowserProfile(context.Background(), db.CreateBrowserProfileParams{
		WorkspaceID: parseUUID(testWorkspaceID), Name: "figma-login", Class: "S2", Hosts: []string{"figma.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The default fixture name embeds a UUID, which an ask refuses as a bare ID.
	dbfx.Exec(t, `UPDATE agent SET name = 'Browser Lease Tester' WHERE id = $1`, f.agentID)
	deny, err := browserlease.ParseClass3Deny(strings.NewReader("[hosts]\npaypal.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	f.h.BrowserLeases.Class3 = deny
	f.h.BrowserLeases.Decisions = f.h.BrowserLeaseDecisions(browserlease.NewDecisions(db.New(testPool)))
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM decision_request WHERE workspace_id = $1 AND request_fingerprint LIKE 'server:browser_lease:%'`, testWorkspaceID)
	})
	return f, uuidToString(prof.ID)
}

type decisionRequiredBody struct {
	Code              string `json:"code"`
	DecisionRequestID string `json:"decision_request_id"`
}

func mintS2(t *testing.T, f browserLeaseFixture, profile string) decisionRequiredBody {
	t.Helper()
	w := f.call(t, f.h.MintBrowserLease, "/x", map[string]string{"profile_id": profile})
	if w.Code != http.StatusConflict {
		t.Fatalf("S2 mint without a decision: %d %s", w.Code, w.Body.String())
	}
	var out decisionRequiredBody
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBrowserLeaseMintS2CreatesTheDecisionRequest(t *testing.T) {
	f, profile := s2Fixture(t)
	out := mintS2(t, f, profile)
	if out.Code != "decision_required" || out.DecisionRequestID == "" {
		t.Fatalf("body = %+v, want decision_required with a request id", out)
	}
	req, err := db.New(testPool).GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), ID: parseUUID(out.DecisionRequestID),
	})
	if err != nil {
		t.Fatalf("request %s not stored: %v", out.DecisionRequestID, err)
	}
	if req.Status != "open" || uuidToString(req.HumanUserID) != testUserID {
		t.Fatalf("request = status %q, human %s", req.Status, uuidToString(req.HumanUserID))
	}
	var agentName, issueID string
	dbfx.QueryRow(t, `SELECT a.name, t.issue_id::text FROM agent_task_queue t JOIN agent a ON a.id = t.agent_id WHERE t.id = $1`, f.taskID).Scan(&agentName, &issueID)
	if uuidToString(req.IssueID) != issueID {
		t.Fatalf("request issue %s, want %s", uuidToString(req.IssueID), issueID)
	}
	for _, want := range []string{agentName, "/issues/" + issueID, "figma-login", "S2", "2 hours"} {
		if !strings.Contains(req.Ask, want) {
			t.Errorf("ask does not name %q:\n%s", want, req.Ask)
		}
	}
}

func TestBrowserLeaseMintS2FindsTheOpenRequest(t *testing.T) {
	f, profile := s2Fixture(t)
	first := mintS2(t, f, profile)
	second := mintS2(t, f, profile)
	if first.DecisionRequestID == "" || first.DecisionRequestID != second.DecisionRequestID {
		t.Fatalf("second mint opened %q, want the first request %q", second.DecisionRequestID, first.DecisionRequestID)
	}
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM decision_request WHERE workspace_id = $1 AND request_fingerprint LIKE 'server:browser_lease:%'`, testWorkspaceID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}
