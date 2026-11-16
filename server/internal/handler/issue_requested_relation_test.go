package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// requestedRelationFixture seeds: an agent-created issue requested by the
// viewer (want), and a viewer-created issue requested by someone else (not).
func requestedRelationFixture(t *testing.T) (wantID, otherID, agentID string) {
	t.Helper()
	otherMember := dbfx.User(t, "requested relation other", fmt.Sprintf("requested-%d@multica.test", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, otherMember, "member")
	agentID = dbfx.Agent(t, "requested relation agent", "")
	wantID = dbfx.Issue(t, "requested by viewer", testutil.Cols{
		"creator_type": "agent", "creator_id": agentID, "requested_by_id": testUserID,
	})
	otherID = dbfx.Issue(t, "created by viewer requested by other", testutil.Cols{
		"creator_id": testUserID, "requested_by_id": otherMember,
	})
	return wantID, otherID, agentID
}

func requestedRelationTableIDs(t *testing.T, relation string) []string {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/issues/table/rows", map[string]any{
		"query": map[string]any{
			"scope":   map[string]any{"kind": "my", "relation": relation},
			"filters": map[string]any{},
			"sort":    map[string]any{"field": "position", "direction": "asc"},
		},
		"group": map[string]any{"kind": "none"},
		"page":  map[string]any{"limit": 100},
	})
	var resp issueTableRowsResponse
	testutil.Call(t, testHandler.ListIssueTableRows, req).Want(http.StatusOK).JSON(&resp)
	ids := make([]string, 0, len(resp.Rows))
	for _, row := range resp.Rows {
		ids = append(ids, row.Issue.ID)
	}
	return ids
}

func TestIssueTableMyRequestedRelation(t *testing.T) {
	wantID, otherID, _ := requestedRelationFixture(t)

	got := requestedRelationTableIDs(t, "requested")
	if !slices.Contains(got, wantID) {
		t.Errorf("requested: missing agent-created issue requested by viewer; got %v", got)
	}
	if slices.Contains(got, otherID) {
		t.Errorf("requested: leaked viewer-created issue requested by another member")
	}

	if any := requestedRelationTableIDs(t, "any"); !slices.Contains(any, wantID) {
		t.Errorf("any: missing agent-created issue requested by viewer; got %v", any)
	}

	w := httptest.NewRecorder()
	testHandler.ListIssueTableRows(w, newRequest(http.MethodPost, "/api/issues/table/rows", map[string]any{
		"query": map[string]any{
			"scope": map[string]any{"kind": "my", "relation": "bogus"},
			"sort":  map[string]any{"field": "position", "direction": "asc"},
		},
		"group": map[string]any{"kind": "none"},
	}))
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown relation: want 400, got %d", w.Code)
	}
}

func TestIssueViewMyRequestedVariant(t *testing.T) {
	wantID, otherID, _ := requestedRelationFixture(t)

	view, code, body := createIssueViewForTest(t, map[string]any{
		"name": "My requested", "scope_type": "my", "scope_variant": "requested", "query": map[string]any{},
	})
	if code != http.StatusCreated {
		t.Fatalf("create requested view: want 201, got %d: %s", code, body)
	}
	var resp issueViewIssuesResponse
	listIssueViewIssuesForTest(t, testUserID, view.ID, "").Want(http.StatusOK).JSON(&resp)
	ids := make([]string, 0, len(resp.Issues))
	for _, issue := range resp.Issues {
		ids = append(ids, issue.ID)
	}
	if !slices.Contains(ids, wantID) || slices.Contains(ids, otherID) {
		t.Errorf("view listing = %v, want %s and not %s", ids, wantID, otherID)
	}

	if _, code, _ := createIssueViewForTest(t, map[string]any{
		"name": "My bogus", "scope_type": "my", "scope_variant": "bogus", "query": map[string]any{},
	}); code != http.StatusBadRequest {
		t.Errorf("unknown variant: want 400, got %d", code)
	}
}

func TestWorkingAgentsRequestedRelation(t *testing.T) {
	wantID, otherID, agentID := requestedRelationFixture(t)
	for _, id := range []string{wantID, otherID} {
		dbfx.Task(t, agentID, testutil.Cols{"runtime_id": testRuntimeID, "issue_id": id, "status": "running"})
	}
	for relation, want := range map[string]int64{"requested": 1, "any": 2} {
		req := workingAgentsFacetRequest(map[string]any{"kind": "my", "relation": relation}, nil)
		if got := workingAgentsFacetCounts(t, req)[agentID]; got != want {
			t.Errorf("facet relation %q: count = %d, want %d", relation, got, want)
		}
	}

	var agents []WorkspaceWorkingAgent
	testutil.Call(t, testHandler.ListWorkspaceWorkingAgents, newRequest(http.MethodGet, "/api/working-agents?type=issue&scope=mine&relation=requested", nil)).Want(http.StatusOK).JSON(&agents)
	var issueIDs []string
	for _, a := range agents {
		if a.ID == agentID {
			issueIDs = a.IssueIDs
		}
	}
	if !slices.Contains(issueIDs, wantID) || slices.Contains(issueIDs, otherID) {
		t.Errorf("working-agents issue_ids = %v, want %s and not %s", issueIDs, wantID, otherID)
	}
}

func TestListIssuesRequestedByIDParam(t *testing.T) {
	wantID, otherID, _ := requestedRelationFixture(t)
	path := "/api/issues?requested_by_id=" + testUserID

	var list struct {
		Issues []IssueResponse `json:"issues"`
	}
	testutil.Call(t, testHandler.ListIssues, newRequest(http.MethodGet, path, nil)).Want(http.StatusOK).JSON(&list)
	var ids []string
	for _, i := range list.Issues {
		ids = append(ids, i.ID)
		if i.RequestedByID == nil || *i.RequestedByID != testUserID {
			t.Errorf("ListIssues leaked %s with requested_by_id %v", i.ID, i.RequestedByID)
		}
	}
	if !slices.Contains(ids, wantID) || slices.Contains(ids, otherID) {
		t.Errorf("ListIssues = %v, want %s and not %s", ids, wantID, otherID)
	}

	var grouped GroupedIssuesResponse
	testutil.Call(t, testHandler.ListGroupedIssues, newRequest(http.MethodGet, path, nil)).Want(http.StatusOK).JSON(&grouped)
	ids = nil
	for _, g := range grouped.Groups {
		for _, i := range g.Issues {
			ids = append(ids, i.ID)
		}
	}
	if !slices.Contains(ids, wantID) || slices.Contains(ids, otherID) {
		t.Errorf("ListGroupedIssues = %v, want %s and not %s", ids, wantID, otherID)
	}

	for name, h := range map[string]http.HandlerFunc{"ListIssues": testHandler.ListIssues, "ListGroupedIssues": testHandler.ListGroupedIssues} {
		w := httptest.NewRecorder()
		h(w, newRequest(http.MethodGet, "/api/issues?requested_by_id=not-a-uuid", nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s malformed requested_by_id: want 400, got %d", name, w.Code)
		}
	}
}
