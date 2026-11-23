package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9075 (C1, R12): with workspace setting quick_create_discovery on, a
// quick-create run that asks for `investigating` gets it only when the workflow
// the server picks for the issue lists the step; otherwise the issue starts as
// it does today. With the setting off, nothing changes.
func TestQuickCreateDiscoveryRouting(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	withFeatureFlag(t, testHandler, featureflags.ProjectWorkflowsV1, true)

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx,
		`SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND name = $2`,
		testWorkspaceID, "Handler Test Agent",
	).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("find test agent: %v", err)
	}
	var taskID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, originator_user_id, accountable_user_id, context)
		 VALUES ($1, $2, 'running', 0, $3, $3, jsonb_build_object('type', 'quick_create', 'workspace_id', $4::text)) RETURNING id`,
		agentID, runtimeID, testUserID, testWorkspaceID,
	).Scan(&taskID); err != nil {
		t.Fatalf("seed quick-create task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	// Discovery is a custom status of the workspace catalog; a workflow may list it.
	insertCustomStatus(t, "investigating", "started", 900, false)
	withDiscovery := createTestWorkflow(t, "Discovery on "+workflowTestSuffix(), "todo", "investigating")
	withoutDiscovery := createTestWorkflow(t, "Discovery off "+workflowTestSuffix(), "todo")
	projectWith := createWorkflowTestProject(t, "Discovery project with")
	projectWithout := createWorkflowTestProject(t, "Discovery project without")
	for project, wf := range map[string]string{projectWith: withDiscovery.ID, projectWithout: withoutDiscovery.ID} {
		setProjectWorkflow(t, project, map[string]any{"workflow_id": wf}).Want(http.StatusOK)
	}

	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&previous)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, string(previous))
	})
	setSettings := func(discovery string, byType string) {
		dbfx.Exec(t, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID,
			`{"quick_create_discovery":"`+discovery+`","workflow_by_type":`+byType+`}`)
	}

	create := func(body map[string]any, asQuickCreate bool) *testutil.Response {
		if asQuickCreate {
			body["origin_type"], body["origin_id"] = "quick_create", taskID
		}
		req := workflowRequest("POST", "/api/issues", body)
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("X-Task-ID", taskID)
		return testutil.Call(t, testHandler.CreateIssue, req)
	}
	var lastIssueID string
	status := func(r *testutil.Response) string {
		t.Helper()
		var got IssueResponse
		r.Want(http.StatusCreated).JSON(&got)
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, got.ID)
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, got.ID)
		})
		lastIssueID = got.ID
		return got.Status
	}
	// comments lists the comments of the issue the last status() call filed.
	comments := func() (contents []string, authors []string) {
		t.Helper()
		rows, err := testPool.Query(ctx,
			`SELECT content, author_type || ':' || author_id::text FROM comment WHERE issue_id = $1 ORDER BY created_at`, lastIssueID)
		if err != nil {
			t.Fatalf("list comments: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c, a string
			if err := rows.Scan(&c, &a); err != nil {
				t.Fatal(err)
			}
			contents, authors = append(contents, c), append(authors, a)
		}
		return contents, authors
	}
	wantNoComment := func() {
		t.Helper()
		if got, _ := comments(); len(got) != 0 {
			t.Fatalf("comments = %q, want none", got)
		}
	}
	unclear := func(project string, title string) map[string]any {
		return map[string]any{
			"title": title + " " + workflowTestSuffix(), "project_id": project, "status": "investigating",
			"discovery_reason": "no checkable done-when",
		}
	}

	toDiscovery := `{"design":"` + withDiscovery.ID + `"}`
	toNoDiscovery := `{"design":"` + withoutDiscovery.ID + `"}`

	t.Run("on: workflow with the step keeps an unclear request in Discovery", func(t *testing.T) {
		setSettings("on", `{}`)
		if got := status(create(unclear(projectWith, "feat(x): vague"), true)); got != "investigating" {
			t.Fatalf("status = %q, want investigating", got)
		}
		contents, authors := comments()
		if len(contents) != 1 || contents[0] != "Discovery: no checkable done-when. Reply to veto." {
			t.Fatalf("comments = %q, want exactly the one-line Discovery note", contents)
		}
		if want := "agent:" + agentID; authors[0] != want {
			t.Fatalf("comment author = %q, want %q", authors[0], want)
		}
	})
	t.Run("on: a missing reason gets the default one-line reason", func(t *testing.T) {
		setSettings("on", `{}`)
		body := unclear(projectWith, "feat(x): vague")
		delete(body, "discovery_reason")
		if got := status(create(body, true)); got != "investigating" {
			t.Fatalf("status = %q, want investigating", got)
		}
		contents, _ := comments()
		if len(contents) != 1 || contents[0] != "Discovery: the request is not clear enough to start. Reply to veto." {
			t.Fatalf("comments = %q, want the default Discovery note", contents)
		}
	})
	t.Run("on: a reason the comment guard would refuse gets the default reason", func(t *testing.T) {
		setSettings("on", `{}`)
		for _, reason := range []string{
			"[@Cheese](mention://member/" + testUserID + ") please confirm the scope",
			"run `multica issue update CHE-1 --status todo` to continue",
		} {
			body := unclear(projectWith, "feat(x): vague")
			body["discovery_reason"] = reason
			if got := status(create(body, true)); got != "investigating" {
				t.Fatalf("reason %q: status = %q, want investigating", reason, got)
			}
			contents, authors := comments()
			if len(contents) != 1 || contents[0] != "Discovery: the request is not clear enough to start. Reply to veto." {
				t.Fatalf("reason %q: comments = %q, want the default Discovery note", reason, contents)
			}
			if want := "agent:" + agentID; authors[0] != want {
				t.Fatalf("comment author = %q, want %q", authors[0], want)
			}
		}
		var events int
		dbfx.QueryRow(t, `SELECT count(*) FROM ask_guard_event WHERE workspace_id=$1 AND issue_id=$2`, testWorkspaceID, lastIssueID).Scan(&events)
		if events != 0 {
			t.Fatalf("guard events = %d, want 0: the note must pass the guard, not trip it", events)
		}
	})
	t.Run("on: workflow without the step starts the issue as today, with no note", func(t *testing.T) {
		setSettings("on", `{}`)
		if got := status(create(unclear(projectWithout, "feat(x): vague"), true)); got != "todo" {
			t.Fatalf("status = %q, want todo", got)
		}
		wantNoComment()
	})
	t.Run("on: a clear request keeps the default status", func(t *testing.T) {
		setSettings("on", `{}`)
		body := unclear(projectWith, "feat(x): clear")
		delete(body, "status")
		if got := status(create(body, true)); got == "investigating" {
			t.Fatalf("clear request was filed in Discovery")
		}
		wantNoComment()
	})
	t.Run("on: mixed title types judge the workflow the title picks", func(t *testing.T) {
		// Project workflow lacks the step, the title-type workflow has it.
		setSettings("on", toDiscovery)
		if got := status(create(unclear(projectWithout, "design(x): vague"), true)); got != "investigating" {
			t.Fatalf("routed to a workflow with the step: status = %q, want investigating", got)
		}
		// Project workflow has the step, the title-type workflow lacks it.
		setSettings("on", toNoDiscovery)
		if got := status(create(unclear(projectWith, "design(x): vague"), true)); got != "todo" {
			t.Fatalf("routed to a workflow without the step: status = %q, want todo", got)
		}
		wantNoComment()
		// An unrouted title in the same workspace still follows its project.
		if got := status(create(unclear(projectWith, "feat(x): vague"), true)); got != "investigating" {
			t.Fatalf("unrouted title: status = %q, want investigating", got)
		}
	})
	t.Run("off: nothing changes", func(t *testing.T) {
		setSettings("off", `{}`)
		if got := status(create(unclear(projectWith, "feat(x): vague"), true)); got != "investigating" {
			t.Fatalf("explicit status on a workflow with the step: status = %q, want investigating", got)
		}
		wantNoComment()
		create(unclear(projectWithout, "feat(x): vague"), true).Want(http.StatusBadRequest)
	})
	t.Run("on: only a quick-create origin is downgraded or noted", func(t *testing.T) {
		setSettings("on", `{}`)
		create(unclear(projectWithout, "feat(x): vague"), false).Want(http.StatusBadRequest)
		if got := status(create(unclear(projectWith, "feat(x): vague"), false)); got != "investigating" {
			t.Fatalf("status = %q, want investigating", got)
		}
		wantNoComment()
	})
}

// CHE-9075: the Discovery note goes through the agent comment guard. The reason
// is sanitised before it gets there, so only a direct call can hand the guard
// content it refuses; this test does, and fails if the guard calls are removed.
func TestPostGuardedCommentHonorsTheAgentCommentGuard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setAskGuardMode(t, "block")
	f := newAskGuardCommentFixture(t)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(f.issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	post := func(content string) {
		req := newRequest(http.MethodPost, "/api/issues", nil)
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Agent-ID", f.agentID)
		req.Header.Set("X-Task-ID", f.taskID)
		testHandler.postGuardedComment(req, issue, "agent", f.agentID, content)
	}

	post("Discovery: " + f.mention() + " the scope is unclear. Reply to veto.")
	if n := f.comments(t); n != 0 {
		t.Fatalf("a note the guard refuses saved %d comment rows, want 0", n)
	}
	if n := f.events(t, "block"); n != 1 {
		t.Fatalf("block events = %d, want 1: the guard was not asked", n)
	}

	post("Discovery: no checkable done-when. Reply to veto.")
	if n := f.comments(t); n != 1 {
		t.Fatalf("a note the guard allows saved %d comment rows, want 1", n)
	}
}
