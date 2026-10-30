package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
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

func TestIssueRelationsMentionsAreOptIn(t *testing.T) {
	root := dbfx.Issue(t, "opt-in root")
	child := dbfx.Issue(t, "opt-in child", testutil.Cols{"parent_issue_id": root})
	mentioned := dbfx.Issue(t, "opt-in mention")
	dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": mentioned, "author_type": "member", "author_id": testUserID, "content": "[root](mention://issue/" + root + ")"})
	for _, query := range []string{"", "?include=mentions"} {
		graph := readRelations(t, root, query)
		want := 2
		if query != "" {
			want = 3
		}
		if len(graph.Nodes) != want {
			t.Fatalf("query %q: got %d nodes, want %d (root and %s without mentions)", query, len(graph.Nodes), want, child)
		}
	}
}

func TestIssueRelationsWithoutMentionsDoesNotTouchComments(t *testing.T) {
	root := dbfx.Issue(t, "no comment scan root")
	for _, includeMentions := range []bool{false, true} {
		var plan []byte
		err := testPool.QueryRow(context.Background(), "EXPLAIN (FORMAT JSON) "+issueRelationsSQL(includeMentions),
			parseUUID(testWorkspaceID), parseUUID(root), 1, []pgtype.UUID{}, "TEST").Scan(&plan)
		if err != nil {
			t.Fatal(err)
		}
		touchesComments := strings.Contains(string(plan), `"Relation Name": "comment"`)
		if touchesComments != includeMentions {
			t.Fatalf("includeMentions=%t: comment in plan=%t", includeMentions, touchesComments)
		}
		t.Logf("includeMentions=%t: comment in EXPLAIN plan=%t", includeMentions, touchesComments)
	}
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
	graph := readRelations(t, root, "?depth=2&include=mentions")
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
	for _, include := range []string{"", "comments", "mentions&include=mentions"} {
		req := withURLParam(newRequest("GET", "/api/issues/"+root+"/relations?include="+include, nil), "id", root)
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

func TestIssueRelationsMentionWorkDoesNotGrowWithWorkspace(t *testing.T) {
	root := dbfx.Issue(t, "bounded relations root")
	neighbor := dbfx.Issue(t, "bounded relations neighbor", testutil.Cols{"description": "[root](mention://issue/" + root + ")"})
	dbfx.Exec(t, `UPDATE issue SET description=$2 WHERE id=$1`, root, "[neighbor](mention://issue/"+neighbor+")")
	dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": neighbor, "author_type": "member", "author_id": testUserID, "content": "[root](mention://issue/" + root + ")"})
	dbfx.Exec(t, `INSERT INTO issue(workspace_id,title,creator_type,creator_id,number,description)
		SELECT $1,'bounded relations background','member',$2,
		 (SELECT coalesce(max(number),0) FROM issue WHERE workspace_id=$1)+counter,repeat('unrelated text ',64)
		FROM generate_series(1,2000) counter`, testWorkspaceID, testUserID)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE workspace_id=$1 AND title='bounded relations background'`, testWorkspaceID)
	dbfx.Exec(t, `INSERT INTO comment(workspace_id,issue_id,author_type,author_id,content)
		SELECT $1,background.id,'member',$2,repeat('unrelated text ',64)
		FROM issue background CROSS JOIN generate_series(1,20)
		WHERE background.workspace_id=$1 AND background.title='bounded relations background'`, testWorkspaceID, testUserID)
	dbfx.Cleanup(t, `DELETE FROM comment WHERE workspace_id=$1 AND issue_id IN
		(SELECT id FROM issue WHERE workspace_id=$1 AND title='bounded relations background')`, testWorkspaceID)
	dbfx.Exec(t, "ANALYZE issue")
	dbfx.Exec(t, "ANALYZE comment")
	t.Log("baseline: 2000 unrelated issues and 40000 unrelated comments; default planner settings")
	type mentionWork struct {
		Regex              int
		DescriptionIndexes int
		CommentIndexes     int
	}
	measure := func(depth int) mentionWork {
		t.Helper()
		var planJSON []byte
		err := testPool.QueryRow(context.Background(), "EXPLAIN (ANALYZE, FORMAT JSON) "+issueRelationsSQL(true),
			parseUUID(testWorkspaceID), parseUUID(root), depth, []pgtype.UUID{}, "TEST").Scan(&planJSON)
		if err != nil {
			t.Fatal(err)
		}
		type queryPlan struct {
			NodeType       string      `json:"Node Type"`
			Function       string      `json:"Function Name"`
			IndexCondition string      `json:"Index Cond"`
			Loops          int         `json:"Actual Loops"`
			Plans          []queryPlan `json:"Plans"`
		}
		var plans []struct{ Plan queryPlan }
		if err := json.Unmarshal(planJSON, &plans); err != nil {
			t.Fatal(err)
		}
		var work mentionWork
		var count func(queryPlan, bool)
		count = func(plan queryPlan, recursive bool) {
			recursive = recursive || plan.NodeType == "Recursive Union"
			if plan.Function == "regexp_matches" {
				work.Regex += plan.Loops
			}
			if recursive && strings.Contains(plan.IndexCondition, "mention://issue/") {
				if strings.Contains(plan.IndexCondition, "description") {
					work.DescriptionIndexes += plan.Loops
				}
				if strings.Contains(plan.IndexCondition, "content") {
					work.CommentIndexes += plan.Loops
				}
			}
			for _, child := range plan.Plans {
				count(child, recursive)
			}
		}
		count(plans[0].Plan, false)
		if work.Regex == 0 {
			t.Fatal("EXPLAIN did not observe mention extraction work")
		}
		if work.DescriptionIndexes == 0 || work.CommentIndexes == 0 {
			t.Errorf("depth %d: reverse mentions must use description and comment text indexes; observed description=%d comment=%d", depth, work.DescriptionIndexes, work.CommentIndexes)
		}
		return work
	}
	before := []mentionWork{measure(1), measure(2)}
	for index := 0; index < 80; index++ {
		unrelated := dbfx.Issue(t, fmt.Sprintf("unrelated relations %d", index))
		content := "[self](mention://issue/" + unrelated + ")"
		dbfx.Exec(t, `UPDATE issue SET description=$2 WHERE id=$1`, unrelated, content)
		dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": unrelated, "author_type": "member", "author_id": testUserID, "content": content})
	}
	for depth := 1; depth <= 2; depth++ {
		after := measure(depth)
		t.Logf("depth %d: mention work before=%+v after=%+v with 80 unrelated issues/comments", depth, before[depth-1], after)
		if after.Regex > before[depth-1].Regex+8 {
			t.Errorf("depth %d: mention extraction work grew with workspace size: %d -> %d", depth, before[depth-1].Regex, after.Regex)
		}
		if after.DescriptionIndexes != before[depth-1].DescriptionIndexes || after.CommentIndexes != before[depth-1].CommentIndexes {
			t.Errorf("depth %d: reverse mention index lookups changed with unrelated workspace content: %+v -> %+v", depth, before[depth-1], after)
		}
	}
}

func TestIssueRelationsKeepsBoundaryEdgesAndMixedCaseMentions(t *testing.T) {
	root := dbfx.Issue(t, "boundary root")
	child := dbfx.Issue(t, "boundary child", testutil.Cols{"parent_issue_id": root})
	first := dbfx.Issue(t, "boundary first", testutil.Cols{"parent_issue_id": child})
	second := dbfx.Issue(t, "boundary second", testutil.Cols{"parent_issue_id": child})
	dbfx.Exec(t, `UPDATE issue SET description=$2 WHERE id=$1`, first, "[second](mention://issue/"+strings.ToUpper(second)+")")
	dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": second, "author_type": "member", "author_id": testUserID, "content": "[first](mention://issue/" + strings.ToUpper(first) + ")"})
	for _, rootID := range []string{root, first, second} {
		graph := readRelations(t, rootID, "?depth=2&include=mentions")
		mentions := map[string]bool{}
		for _, edge := range graph.Edges {
			if edge.Type == "mention" {
				mentions[edge.Source+"/"+edge.Target] = true
			}
		}
		if !mentions[first+"/"+second] || !mentions[second+"/"+first] {
			t.Fatalf("root %s: missing edges between boundary nodes or mixed-case reverse mentions: %+v", rootID, graph)
		}
	}
}
