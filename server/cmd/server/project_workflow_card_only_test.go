package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// CHE-10137: with MULTICA_TASK_WRITE_SCOPE=on a run's task token still reaches
// the project workflow routes to open the owner card for a switch that widens
// the run budget, and every other switch stays a 403 that writes nothing.
func TestTaskTokenOpensProjectWorkflowCardUnderWriteScopeOn(t *testing.T) {
	t.Setenv("MULTICA_TASK_WRITE_SCOPE", "on")
	router, h := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{})
	flags := featureflag.NewStaticProvider()
	flags.Set(featureflags.ProjectWorkflowsV1, featureflag.Rule{Default: true})
	h.FeatureFlags = featureflag.NewService(flags)
	server := httptest.NewServer(router)
	defer server.Close()

	fixture := workspaceFixture(t)
	runtimeID := fixture.Runtime(t, "Card only runtime")
	runAgent := fixture.Agent(t, "Card only agent", runtimeID)
	hostIssue := fixture.Issue(t, "Card only host issue", testutil.Cols{"assignee_type": "agent", "assignee_id": runAgent})
	runTask := fixture.Task(t, runAgent, testutil.Cols{"issue_id": hostIssue, "runtime_id": runtimeID, "status": "running"})
	token := mintAgentTaskToken(t, runAgent, runTask, testUserID)

	workflow := func(name string, since string) string {
		id := fixture.Insert(t, "issue_workflow", testutil.Cols{
			"workspace_id": testWorkspaceID, "name": name, "initial_status_key": "todo",
			"steps":              `[{"status_key":"todo"},{"status_key":"in_progress"},{"status_key":"done"}]`,
			"max_runs_per_issue": 25,
		})
		fixture.Exec(t, `UPDATE issue_workflow SET run_budget_since = `+since+` WHERE id = $1`, id)
		fixture.Cleanup(t, `UPDATE project SET workflow_id = NULL WHERE workflow_id = $1`, id)
		return id
	}
	narrow := workflow("Card only narrow", "now() - interval '1 hour'")
	wide := workflow("Card only wide", "now()")
	project := func(name, workflowID string) string {
		id := fixture.Project(t, name)
		fixture.Exec(t, `UPDATE project SET workflow_id = $1 WHERE id = $2`, workflowID, id)
		return id
	}
	post := func(path string, body map[string]any) int {
		t.Helper()
		raw, _ := json.Marshal(body)
		request, err := http.NewRequest("POST", server.URL+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Workspace-ID", testWorkspaceID)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	unchanged := func(id, workflowID string) {
		t.Helper()
		if fixture.Count(t, `SELECT count(*) FROM project WHERE id = $1 AND workflow_id = $2`, id, workflowID) != 1 {
			t.Errorf("project %s changed workflow", id)
		}
	}

	t.Run("a widening switch opens the card on the single route", func(t *testing.T) {
		p := project("Card only single", narrow)
		if got := post("/api/projects/"+p+"/workflow", map[string]any{"workflow_id": wide}); got != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", got)
		}
		unchanged(p, narrow)
	})
	t.Run("a widening switch opens one card on the batch route", func(t *testing.T) {
		a, b := project("Card only a", narrow), project("Card only b", narrow)
		if got := post("/api/projects/workflow", map[string]any{"project_ids": []string{a, b}, "workflow_id": wide}); got != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", got)
		}
		unchanged(a, narrow)
		unchanged(b, narrow)
	})
	t.Run("a narrowing switch is refused on both routes", func(t *testing.T) {
		p := project("Card only narrowing", wide)
		if got := post("/api/projects/"+p+"/workflow", map[string]any{"workflow_id": narrow}); got != http.StatusForbidden {
			t.Fatalf("single route status = %d, want 403", got)
		}
		if got := post("/api/projects/workflow", map[string]any{"project_ids": []string{p}, "workflow_id": narrow}); got != http.StatusForbidden {
			t.Fatalf("batch route status = %d, want 403", got)
		}
		unchanged(p, wide)
	})
}
