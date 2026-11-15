package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Private-project issues stay hidden from non-members on both fork relations
// read paths (CHE-5611 review P1): a visible issue must not leak the title, id
// or status of a hidden neighbour, and a hidden root is a 404.

type privateRelationsFixture struct {
	privateProjectFixture
	root, hiddenChild, hiddenDep, visibleGrandchild string
}

// newPrivateRelationsFixture: public root -> hidden child -> visible grandchild,
// plus a dependency from root on a hidden issue.
func newPrivateRelationsFixture(t *testing.T) privateRelationsFixture {
	t.Helper()
	f := privateRelationsFixture{privateProjectFixture: newPrivateProjectFixture(t)}
	f.root = dbfx.Issue(t, f.marker+" relations root")
	f.hiddenChild = dbfx.Issue(t, f.marker+" PRIVATE CHILD", testutil.Cols{"parent_issue_id": f.root, "project_id": f.projectID})
	f.visibleGrandchild = dbfx.Issue(t, f.marker+" visible grandchild", testutil.Cols{"parent_issue_id": f.hiddenChild})
	f.hiddenDep = f.secretIssue
	dbfx.Exec(t, `INSERT INTO issue_dependency(issue_id,depends_on_issue_id,type) VALUES ($1,$2,'blocks')`, f.root, f.hiddenDep)
	return f
}

func relationsAs(t *testing.T, userID, id, query string, want int) relationsGraph {
	t.Helper()
	var graph relationsGraph
	req := withURLParam(asProjectUser(newRequest("GET", "/api/issues/"+id+"/relations"+query, nil), userID), "id", id)
	res := testutil.Call(t, testHandler.GetIssueRelations, req).Want(want)
	if want == http.StatusOK {
		res.JSON(&graph)
	}
	return graph
}

func batchAs(t *testing.T, userID string, ids []string, want int) relationsBatchPage {
	t.Helper()
	var page relationsBatchPage
	req := asProjectUser(newRequest("POST", "/api/issues/relations/batch", map[string]any{"issue_ids": ids}), userID)
	res := testutil.Call(t, testHandler.GetIssueRelationsBatch, req).Want(want)
	if want == http.StatusOK {
		res.JSON(&page)
	}
	return page
}

func graphIDs(graph relationsGraph) map[string]bool {
	ids := map[string]bool{}
	for _, node := range graph.Nodes {
		ids[node.ID] = true
	}
	return ids
}

func TestIssueRelationsHidePrivateProjectNeighbours(t *testing.T) {
	f := newPrivateRelationsFixture(t)
	for _, query := range []string{"", "?depth=2", "?include=mentions&depth=2"} {
		ids := graphIDs(relationsAs(t, f.outsider, f.root, query, http.StatusOK))
		for _, hidden := range []string{f.hiddenChild, f.hiddenDep, f.visibleGrandchild} {
			if ids[hidden] {
				t.Errorf("outsider %q sees hidden or hidden-only-reachable issue %s", query, hidden)
			}
		}
		if !ids[f.root] {
			t.Errorf("outsider %q lost the visible root", query)
		}
		for _, who := range []string{f.insider, testUserID} {
			ids := graphIDs(relationsAs(t, who, f.root, query, http.StatusOK))
			if !ids[f.hiddenChild] || !ids[f.hiddenDep] {
				t.Errorf("project viewer %s %q does not see the private neighbours", who, query)
			}
		}
	}
	// A hidden root is already a 404 via loadIssueForUser.
	relationsAs(t, f.outsider, f.hiddenChild, "", http.StatusNotFound)
}

func TestIssueRelationsBatchHidePrivateProjectNeighbours(t *testing.T) {
	f := newPrivateRelationsFixture(t)
	page := batchAs(t, f.outsider, []string{f.root, f.visibleGrandchild}, http.StatusOK)
	for _, node := range page.Nodes {
		if node.ID == f.hiddenChild || node.ID == f.hiddenDep {
			t.Errorf("outsider batch node leaks hidden issue %s", node.ID)
		}
	}
	for _, edge := range page.Edges {
		for _, end := range []string{edge.Source, edge.Target} {
			if end == f.hiddenChild || end == f.hiddenDep {
				t.Errorf("outsider batch edge %s %s->%s leaks hidden issue", edge.Type, edge.Source, edge.Target)
			}
		}
	}
	// Hidden root, alone or among visible roots, is the same 404 as a missing issue.
	batchAs(t, f.outsider, []string{f.hiddenChild}, http.StatusNotFound)
	batchAs(t, f.outsider, []string{f.root, f.hiddenChild}, http.StatusNotFound)
	for _, who := range []string{f.insider, testUserID} {
		page := batchAs(t, who, []string{f.root, f.hiddenChild}, http.StatusOK)
		seen := ""
		for _, node := range page.Nodes {
			seen += node.ID + " "
		}
		if !strings.Contains(seen, f.hiddenChild) || !strings.Contains(seen, f.hiddenDep) {
			t.Errorf("project viewer %s: batch nodes %q miss the private issues", who, seen)
		}
	}
}
