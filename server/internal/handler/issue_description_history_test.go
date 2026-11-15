package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type descRevision struct {
	Revision  int64   `json:"revision"`
	ActorType *string `json:"actor_type"`
	ActorID   *string `json:"actor_id"`
	CreatedAt string  `json:"created_at"`
}

func setDescription(t *testing.T, issueID, desc string) {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID, map[string]any{"description": desc}), "id", issueID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
}

func descHistory(t *testing.T, issueID string) []descRevision {
	t.Helper()
	req := withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID+"/description/history", nil), "id", issueID)
	w := testutil.Call(t, testHandler.ListIssueDescriptionRevisions, req).Want(http.StatusOK)
	var out struct {
		Revisions []descRevision `json:"revisions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	return out.Revisions
}

func descDiff(t *testing.T, issueID, query string) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID+"/description/diff?"+query, nil), "id", issueID)
	return testutil.Call(t, testHandler.DiffIssueDescription, req)
}

func TestIssueDescriptionHistoryListsRevisionsInOrder(t *testing.T) {
	issueID := dbfx.Issue(t, "history order", testutil.Cols{"description": "v1"})
	setDescription(t, issueID, "v2")
	setDescription(t, issueID, "v3")

	got := descHistory(t, issueID)
	if len(got) != 3 {
		t.Fatalf("revisions = %d, want 3 (create + 2 edits): %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Revision <= got[i-1].Revision {
			t.Fatalf("revisions not ascending: %+v", got)
		}
	}
	last := got[len(got)-1]
	if last.ActorType == nil || *last.ActorType != "member" || last.ActorID == nil || *last.ActorID != testUserID {
		t.Fatalf("last actor = %v/%v, want member/%s", last.ActorType, last.ActorID, testUserID)
	}
	if last.CreatedAt == "" {
		t.Fatal("created_at empty")
	}
}

func TestIssueDescriptionDiffBetweenRevisions(t *testing.T) {
	issueID := dbfx.Issue(t, "history diff", testutil.Cols{"description": "alpha\nbeta"})
	setDescription(t, issueID, "alpha\ngamma")
	got := descHistory(t, issueID)
	from, to := got[0].Revision, got[len(got)-1].Revision

	w := descDiff(t, issueID, "from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10)).Want(http.StatusOK)
	var out struct {
		From int64  `json:"from"`
		To   int64  `json:"to"`
		Diff string `json:"diff"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	if out.From != from || out.To != to {
		t.Fatalf("from/to = %d/%d, want %d/%d", out.From, out.To, from, to)
	}
	for _, want := range []string{"-beta", "+gamma", " alpha"} {
		if !strings.Contains(out.Diff, want) {
			t.Fatalf("diff missing %q:\n%s", want, out.Diff)
		}
	}

	// A revision that bumped only the title resolves to the text as of then.
	req := withURLParam(newRequest(http.MethodPut, "/api/issues/"+issueID, map[string]any{"title": "renamed"}), "id", issueID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	var rev int64
	dbfx.QueryRow(t, `SELECT revision FROM issue WHERE id = $1`, issueID).Scan(&rev)
	w = descDiff(t, issueID, "from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(rev, 10)).Want(http.StatusOK)
	if !strings.Contains(w.Body.String(), "+gamma") {
		t.Fatalf("diff to title-only revision lost the description change: %s", w.Body.String())
	}
}

func TestIssueDescriptionDiffRejectsUnknownRevision(t *testing.T) {
	issueID := dbfx.Issue(t, "history unknown", testutil.Cols{"description": "x"})
	got := descHistory(t, issueID)
	first := strconv.FormatInt(got[0].Revision, 10)

	descDiff(t, issueID, "from="+first+"&to=9999").Want(http.StatusNotFound)
	descDiff(t, issueID, "from=0&to="+first).Want(http.StatusBadRequest)
	descDiff(t, issueID, "from="+first).Want(http.StatusBadRequest)
	descDiff(t, issueID, "from=abc&to="+first).Want(http.StatusBadRequest)
}

func TestIssueDescriptionHistoryStaysInsideWorkspace(t *testing.T) {
	otherWorkspaceID := dbfx.Insert(t, "workspace", testutil.Cols{
		"name": "history boundary", "slug": "history-boundary", "description": "foreign", "issue_prefix": "HBX",
	})
	foreign := dbfx.Issue(t, "foreign", testutil.Cols{"workspace_id": otherWorkspaceID, "number": 1, "description": "secret"})

	req := withURLParam(newRequest(http.MethodGet, "/api/issues/"+foreign+"/description/history", nil), "id", foreign)
	testutil.Call(t, testHandler.ListIssueDescriptionRevisions, req).Want(http.StatusNotFound)
	descDiff(t, foreign, "from=1&to=1").Want(http.StatusNotFound)
}
