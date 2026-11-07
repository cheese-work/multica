package handler

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type relationsBatchPage struct {
	Nodes []struct {
		ID            string  `json:"id"`
		Context       bool    `json:"context"`
		ParentIssueID *string `json:"parent_issue_id"`
	} `json:"nodes"`
	Edges []struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Type   string `json:"type"`
	} `json:"edges"`
	NextCursor string `json:"next_cursor"`
}

func postRelationsBatch(t *testing.T, body map[string]any) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.GetIssueRelationsBatch, newRequest("POST", "/api/issues/relations/batch", body))
}

func readRelationsBatch(t *testing.T, body map[string]any) relationsBatchPage {
	t.Helper()
	var page relationsBatchPage
	postRelationsBatch(t, body).Want(http.StatusOK).JSON(&page)
	return page
}

// X1 (server part): membership comes from issue_ids, so a disconnected tree and
// a fan-out above the 40-node walk cap are both complete.
func TestRelationsBatchHasNoWalkCap(t *testing.T) {
	root := dbfx.Issue(t, "batch root")
	ids := []string{root}
	for index := 0; index < 45; index++ {
		ids = append(ids, dbfx.Issue(t, fmt.Sprintf("batch child %d", index), testutil.Cols{"parent_issue_id": root}))
	}
	otherRoot := dbfx.Issue(t, "batch other root")
	ids = append(ids, otherRoot)
	for index := 0; index < 2; index++ {
		ids = append(ids, dbfx.Issue(t, fmt.Sprintf("batch other child %d", index), testutil.Cols{"parent_issue_id": otherRoot}))
	}
	page := readRelationsBatch(t, map[string]any{"issue_ids": ids, "types": []string{"parent"}})
	if len(page.Nodes) != 49 || len(page.Edges) != 47 || page.NextCursor != "" {
		t.Fatalf("batch: %d nodes, %d edges, cursor %q; want 49, 47, none", len(page.Nodes), len(page.Edges), page.NextCursor)
	}
	for _, node := range page.Nodes {
		if node.Context {
			t.Fatalf("node %s is context, want in-request", node.ID)
		}
	}
	existing := readRelations(t, root, "?depth=2")
	t.Logf("existing endpoint for the same data: %d nodes (batch: %d)", len(existing.Nodes), len(page.Nodes))
	if len(existing.Nodes) != 40 {
		t.Fatalf("existing endpoint: %d nodes, want the 40 cap", len(existing.Nodes))
	}
}

// X2: 2,500 edges at limit 1000 → 3 pages, union is the full set, no repeats.
func TestRelationsBatchPagesWithoutSilentCap(t *testing.T) {
	ids := make([]string, 51)
	for index := range ids {
		ids[index] = dbfx.Issue(t, fmt.Sprintf("batch paging %d", index))
	}
	dbfx.Exec(t, `
INSERT INTO issue_dependency(issue_id,depends_on_issue_id,type)
SELECT a,b,'blocks' FROM unnest($1::uuid[]) a CROSS JOIN unnest($1::uuid[]) b WHERE a<>b ORDER BY a,b LIMIT 2500`, ids)
	seen := map[string]bool{}
	pages, cursor := 0, ""
	for {
		body := map[string]any{"issue_ids": ids, "types": []string{"dependency"}, "page": map[string]any{"limit": 1000}}
		if cursor != "" {
			body["page"] = map[string]any{"limit": 1000, "cursor": cursor}
		}
		page := readRelationsBatch(t, body)
		pages++
		for _, edge := range page.Edges {
			key := edge.Type + "/" + edge.Source + "/" + edge.Target
			if seen[key] {
				t.Fatalf("edge %s repeats on page %d", key, pages)
			}
			seen[key] = true
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
		if pages > 5 {
			t.Fatal("cursor never ends")
		}
	}
	if pages != 3 || len(seen) != 2500 {
		t.Fatalf("got %d pages, %d edges; want 3, 2500", pages, len(seen))
	}
}

// X4 and N6: an in-workspace endpoint outside issue_ids is one context node;
// a foreign-workspace endpoint is omitted with its edge.
func TestRelationsBatchContextNodeAndForeignEdge(t *testing.T) {
	inside := dbfx.Issue(t, "batch inside")
	outside := dbfx.Issue(t, "batch outside", testutil.Cols{"parent_issue_id": inside})
	foreign := dbfx.Issue(t, "batch foreign", testutil.Cols{"workspace_id": dbfx.Workspace(t, "batch foreign", "batch-foreign")})
	dbfx.Insert(t, "issue_dependency", testutil.Cols{"issue_id": inside, "depends_on_issue_id": foreign, "type": "blocks"})
	page := readRelationsBatch(t, map[string]any{"issue_ids": []string{inside}})
	if len(page.Edges) != 1 || page.Edges[0].Target != outside {
		t.Fatalf("edges: %+v", page.Edges)
	}
	contexts := 0
	for _, node := range page.Nodes {
		if node.ID == foreign {
			t.Fatal("foreign node leaked")
		}
		if node.Context {
			contexts++
			if node.ID != outside {
				t.Fatalf("context node %s, want %s", node.ID, outside)
			}
		}
	}
	if contexts != 1 || len(page.Nodes) != 2 {
		t.Fatalf("nodes: %+v", page.Nodes)
	}
}

// F2: a parent in another workspace never leaks its ID through parent_issue_id;
// an in-workspace parent still does.
func TestRelationsBatchNullsForeignParentID(t *testing.T) {
	foreignParent := dbfx.Issue(t, "batch foreign parent", testutil.Cols{"workspace_id": dbfx.Workspace(t, "batch foreign parent", "batch-foreign-parent")})
	orphan := dbfx.Issue(t, "batch orphan", testutil.Cols{"parent_issue_id": foreignParent})
	parent := dbfx.Issue(t, "batch parent")
	child := dbfx.Issue(t, "batch child", testutil.Cols{"parent_issue_id": parent})
	dbfx.Insert(t, "issue_dependency", testutil.Cols{"issue_id": orphan, "depends_on_issue_id": child, "type": "blocks"})
	page := readRelationsBatch(t, map[string]any{"issue_ids": []string{orphan, child}})
	seen := 0
	defer func() {
		if seen != 2 {
			t.Fatalf("saw %d of the 2 expected nodes", seen)
		}
	}()
	for _, node := range page.Nodes {
		switch node.ID {
		case orphan:
			seen++
			if node.ParentIssueID != nil {
				t.Fatalf("orphan parent_issue_id = %s, want null", *node.ParentIssueID)
			}
		case child:
			seen++
			if node.ParentIssueID == nil || *node.ParentIssueID != parent {
				t.Fatalf("child parent_issue_id = %v, want %s", node.ParentIssueID, parent)
			}
		}
	}
}

func TestRelationsBatchRejectsBadInputAndForeignIDs(t *testing.T) {
	own := dbfx.Issue(t, "batch own")
	foreign := dbfx.Issue(t, "batch foreign id", testutil.Cols{"workspace_id": dbfx.Workspace(t, "batch foreign id", "batch-foreign-id")})
	tooMany := make([]string, 201)
	for index := range tooMany {
		tooMany[index] = own
	}
	for name, body := range map[string]map[string]any{
		"empty ids":     {"issue_ids": []string{}},
		"bad uuid":      {"issue_ids": []string{"nope"}},
		"over 200":      {"issue_ids": tooMany},
		"unknown type":  {"issue_ids": []string{own}, "types": []string{"nope"}},
		"limit too big": {"issue_ids": []string{own}, "page": map[string]any{"limit": 2001}},
		"bad cursor":    {"issue_ids": []string{own}, "page": map[string]any{"cursor": "x"}},
	} {
		t.Run(name, func(t *testing.T) { postRelationsBatch(t, body).Want(http.StatusBadRequest) })
	}
	// N6: one foreign ID fails the whole request.
	postRelationsBatch(t, map[string]any{"issue_ids": []string{own, foreign}}).Want(http.StatusNotFound)
	outsider := dbfx.User(t, "batch outsider", "batch-outsider@test.local")
	req := newRequest("POST", "/api/issues/relations/batch", map[string]any{"issue_ids": []string{own}})
	req.Header.Set("X-User-ID", outsider)
	testutil.Call(t, testHandler.GetIssueRelationsBatch, req).Want(http.StatusNotFound)
}
