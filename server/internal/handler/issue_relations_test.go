package handler

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type relationsGraph struct {
	Nodes []struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		Status     string `json:"status"`
	} `json:"nodes"`
	Edges []struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Type   string `json:"type"`
	} `json:"edges"`
	OmittedCount int `json:"omitted_count"`
}

func readRelations(t *testing.T, id, query string) relationsGraph {
	t.Helper()
	var graph relationsGraph
	req := withURLParam(newRequest("GET", "/api/issues/"+id+"/relations"+query, nil), "id", id)
	testutil.Call(t, testHandler.GetIssueRelations, req).Want(http.StatusOK).JSON(&graph)
	return graph
}

func TestIssueRelationsDepthCapAndWorkspaceIsolation(t *testing.T) {
	root := dbfx.Issue(t, "relations root")
	child := dbfx.Issue(t, "relations child", testutil.Cols{"parent_issue_id": root})
	grandchild := dbfx.Issue(t, "relations grandchild", testutil.Cols{"parent_issue_id": child})
	graph := readRelations(t, root, "")
	if len(graph.Nodes) != 2 || len(graph.Edges) != 1 {
		t.Fatalf("depth 1: %+v", graph)
	}
	graph = readRelations(t, root, "?depth=2")
	if len(graph.Nodes) != 3 || len(graph.Edges) != 2 {
		t.Fatalf("depth 2 missing %s: %+v", grandchild, graph)
	}
	foreignWS := dbfx.Workspace(t, "relations foreign", "relations-foreign")
	foreign := dbfx.Issue(t, "foreign child", testutil.Cols{"workspace_id": foreignWS, "parent_issue_id": root})
	dbfx.Exec(t, `UPDATE issue SET description=$2 WHERE id=$1`, root, "[secret](mention://issue/"+foreign+")")
	for index := 0; index < 45; index++ {
		dbfx.Issue(t, fmt.Sprintf("relations sibling %d", index), testutil.Cols{"parent_issue_id": root})
	}
	graph = readRelations(t, root, "?depth=2")
	if len(graph.Nodes) != 40 || graph.OmittedCount != 8 {
		t.Fatalf("cap: got %d nodes, %d omitted, want 40 and 8", len(graph.Nodes), graph.OmittedCount)
	}
	visible := map[string]bool{}
	for _, node := range graph.Nodes {
		visible[node.ID] = true
		if node.ID == foreign || node.Identifier == "" || node.Status == "" {
			t.Fatalf("inaccessible or incomplete node: %+v", node)
		}
	}
	if !visible[root] {
		t.Fatal("root omitted")
	}
	for _, edge := range graph.Edges {
		if !visible[edge.Source] || !visible[edge.Target] {
			t.Fatalf("dangling edge: %+v", edge)
		}
	}
}

func TestIssueRelationsTypesAndCycles(t *testing.T) {
	root := dbfx.Issue(t, "relations root")
	child := dbfx.Issue(t, "relations child", testutil.Cols{"parent_issue_id": root})
	duplicate := dbfx.Issue(t, "relations duplicate", testutil.Cols{"status": "cancelled", "duplicate_of_issue_id": root})
	mentioned := dbfx.Issue(t, "mentioned", testutil.Cols{"description": "[root](mention://issue/" + root + ")"})
	dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": root, "author_type": "member", "author_id": testUserID, "content": "[mentioned](mention://issue/" + mentioned + ")"})
	commentMentioning := dbfx.Issue(t, "comment mentions root")
	dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": commentMentioning, "author_type": "member", "author_id": testUserID, "content": "[root](mention://issue/" + root + ")"})
	agent := dbfx.Agent(t, "relations agent", "")
	waiting := dbfx.Issue(t, "waiting")
	for _, spec := range []struct{ issue, condition string }{
		{waiting, fmt.Sprintf(`{"type":"other_issue","issue_id":%q,"state":"done"}`, root)},
		{root, `{"type":"children_done"}`},
	} {
		dbfx.Insert(t, "issue_wakeup", testutil.Cols{"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID, "issue_id": spec.issue, "agent_id": agent, "created_by": testUserID, "instruction": "resume", "kind": "event", "mode": "continuous", "event_types": testutil.Raw(`ARRAY['issue.status_changed']`), "condition": spec.condition})
	}
	pr := dbfx.Insert(t, "github_pull_request", testutil.Cols{"workspace_id": testWorkspaceID, "installation_id": int64(1), "repo_owner": "test", "repo_name": "relations", "pr_number": 1, "title": "shared", "state": "open", "html_url": "https://github.com/test/relations/pull/1", "pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()")})
	for _, id := range []string{root, child} {
		dbfx.Exec(t, `INSERT INTO issue_pull_request(issue_id,pull_request_id) VALUES($1,$2)`, id, pr)
		t.Cleanup(func() {
			dbfx.Exec(t, `DELETE FROM issue_pull_request WHERE issue_id=$1 AND pull_request_id=$2`, id, pr)
		})
	}
	graph := readRelations(t, root, "?depth=2")
	if len(graph.Nodes) != 6 {
		t.Fatalf("nodes: %+v", graph)
	}
	seen := map[string]bool{}
	reverseMentions := map[string]bool{}
	for _, edge := range graph.Edges {
		seen[edge.Type] = true
		if edge.Type == "mention" && edge.Target == root {
			reverseMentions[edge.Source] = true
		}
		if edge.Type == "duplicate" && (edge.Source != duplicate || edge.Target != root) {
			t.Fatalf("duplicate direction: %+v", edge)
		}
		if edge.Type == "wakeup" && (edge.Source != root || edge.Target != waiting) {
			t.Fatalf("wakeup direction: %+v", edge)
		}
		if edge.Type == "children_done" && (edge.Source != child || edge.Target != root) {
			t.Fatalf("children direction: %+v", edge)
		}
	}
	if !reverseMentions[mentioned] || !reverseMentions[commentMentioning] {
		t.Fatalf("missing reverse description or comment mention: %+v", graph.Edges)
	}
	for _, kind := range []string{"parent", "duplicate", "mention", "shared_pr", "wakeup", "children_done"} {
		if !seen[kind] {
			t.Errorf("missing %s edge: %+v", kind, graph.Edges)
		}
	}
}

func TestIssueRelationsRejectsInvalidDepthAndForeignRoot(t *testing.T) {
	root := dbfx.Issue(t, "relations root")
	for _, depth := range []string{"0", "3", "-1", "no", "2&depth=1"} {
		req := withURLParam(newRequest("GET", "/api/issues/"+root+"/relations?depth="+depth, nil), "id", root)
		testutil.Call(t, testHandler.GetIssueRelations, req).Want(http.StatusBadRequest)
	}
	foreign := dbfx.Issue(t, "relations foreign root", testutil.Cols{"workspace_id": dbfx.Workspace(t, "relations foreign", "relations-foreign-root")})
	req := withURLParam(newRequest("GET", "/api/issues/"+foreign+"/relations", nil), "id", foreign)
	testutil.Call(t, testHandler.GetIssueRelations, req).Want(http.StatusNotFound)
}

func TestIssueRelationsHidesPrivateWakeupsAndRejectsNonmembers(t *testing.T) {
	root := dbfx.Issue(t, "relations root")
	waiting := dbfx.Issue(t, "private waiting")
	agent := dbfx.Agent(t, "private relations agent", "")
	dbfx.Insert(t, "issue_wakeup", testutil.Cols{"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID, "issue_id": waiting, "agent_id": agent, "created_by": testUserID, "instruction": "private instruction", "kind": "event", "mode": "continuous", "condition": fmt.Sprintf(`{"type":"other_issue","issue_id":%q,"state":"ended"}`, root)})
	viewer := dbfx.User(t, "relations viewer", "relations-viewer@test.local")
	dbfx.Member(t, testWorkspaceID, viewer, "member")
	req := withURLParam(newRequest("GET", "/api/issues/"+root+"/relations", nil), "id", root)
	req.Header.Set("X-User-ID", viewer)
	var graph relationsGraph
	testutil.Call(t, testHandler.GetIssueRelations, req).Want(http.StatusOK).JSON(&graph)
	if len(graph.Nodes) != 1 || len(graph.Edges) != 0 || graph.OmittedCount != 0 {
		t.Fatalf("private wakeup leaked: %+v", graph)
	}
	outsider := dbfx.User(t, "relations outsider", "relations-outsider@test.local")
	req = withURLParam(newRequest("GET", "/api/issues/"+root+"/relations", nil), "id", root)
	req.Header.Set("X-User-ID", outsider)
	testutil.Call(t, testHandler.GetIssueRelations, req).Want(http.StatusNotFound)
}
