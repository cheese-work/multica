package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// A review request is judged like an assignment: an agent that names an
// out-of-scope reviewer is refused with the capability_routing reviewers; a
// person is not.
func TestAgentRoleScopeReviewRequest_CHE9192(t *testing.T) {
	author := reviewerAgent(t, "claude", "claude-sonnet-5-5")
	terra := reviewerAgent(t, "codex", "gpt-5.5")
	sol := reviewerAgent(t, "codex", "gpt-5.5")
	c := newReviewerCase(t, author, terra)
	setWorkspaceSettings(t, mustJSON(t, map[string]any{
		"capability_routing": map[string]any{"feat": map[string]any{"author": author, "reviewers": []string{terra}}},
		"agent_role_scope":   map[string]any{"agents": map[string]any{sol: []string{"plan", "plan_review", "decision", "debate", "security_cosign"}}},
	}))
	issueID := c.issue(t, author)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, author).Scan(&runtimeID)
	taskID := dbfx.Task(t, author, testutil.Cols{
		"issue_id": issueID, "runtime_id": runtimeID, "status": "running",
		"started_at": testutil.Raw("now()"), "applied_model": "claude-sonnet-5-5",
	})
	move := map[string]any{"status": "in_review", "handoff_assignee": overrideTo(sol)}

	req := withURLParam(newRequest("PUT", "/api/issues/"+issueID, move), "id", issueID)
	req.Header.Set("X-Agent-ID", author)
	req.Header.Set("X-Task-ID", taskID)
	resp := testutil.Call(t, testHandler.UpdateIssue, req)
	resp.Want(http.StatusForbidden)
	body := resp.Body.String()
	for _, want := range []string{"agent_role_scope", "code_review", "capability_routing[feat].reviewers"} {
		if !strings.Contains(body, want) {
			t.Fatalf("refusal %s must name %q", body, want)
		}
	}
	if status, _, _ := issueStatusAndAssignee(t, issueID); status != "in_progress" {
		t.Fatalf("status = %q after the refusal, want in_progress", status)
	}

	// A person may still pick Sol.
	updateIssueForTest(t, issueID, move).Want(http.StatusOK)
	if _, _, id := issueStatusAndAssignee(t, issueID); id != sol {
		t.Fatalf("assignee = %s, want Sol after a person's request", id)
	}
}

// CHE-9192 (CHE-4592 D7): an agent never hands code-review or execution work to
// an agent whose agent_role_scope excludes it. A person still can; the issue
// keeps an audit note.
func TestAgentRoleScopeAssign_CHE9192(t *testing.T) {
	seedTestCatalog(t)
	authorID := createHandlerTestAgent(t, "scope-author", nil)
	reviewerID := createHandlerTestAgent(t, "scope-reviewer", nil)
	solID := createHandlerTestAgent(t, "scope-sol", nil)
	otherID := createHandlerTestAgent(t, "scope-other", nil)
	solScope := []string{"plan", "plan_review", "decision", "debate", "security_cosign"}
	settings, _ := json.Marshal(map[string]any{
		"capability_routing": map[string]any{"feat": map[string]any{"author": authorID, "reviewers": []string{reviewerID}}},
		"agent_role_scope": map[string]any{
			"agents": map[string]any{solID: solScope},
		},
	})
	setWorkspaceSettings(t, string(settings))

	toSol := map[string]any{"assignee_type": "agent", "assignee_id": solID}
	toOther := map[string]any{"assignee_type": "agent", "assignee_id": otherID}
	newIssue := func(t *testing.T, title string) (actingAgentID, issueID string) {
		actingAgentID = createHandlerTestAgent(t, "scope-actor", nil)
		issueID = dbfx.Issue(t, title, testutil.Cols{
			"status": "in_progress", "assignee_type": "agent", "assignee_id": actingAgentID,
		})
		dbfx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id = $1", issueID)
		return actingAgentID, issueID
	}
	agentHeaders := func(t *testing.T, agentID, issueID string) map[string]string {
		taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issueID)
		return map[string]string{"X-Agent-ID": agentID, "X-Task-ID": taskID}
	}
	assignee := func(t *testing.T, issueID string) string {
		_, _, id := issueStatusAndAssignee(t, issueID)
		return id
	}

	for _, writer := range issueWriters {
		t.Run(writer.name+"/agent assigning a feat issue to Sol is refused with the reroute", func(t *testing.T) {
			actor, issueID := newIssue(t, "feat(x): ship the thing")
			resp := writer.write(t, issueID, toSol, agentHeaders(t, actor, issueID))
			if writer.name == "single" {
				resp.Want(http.StatusForbidden)
				var body struct {
					Error string `json:"error"`
				}
				resp.JSON(&body)
				for _, want := range []string{"agent_role_scope", "scope-sol", "scope-author"} {
					if !strings.Contains(body.Error, want) {
						t.Fatalf("refusal %q must name %q", body.Error, want)
					}
				}
			}
			if got := assignee(t, issueID); got != actor {
				t.Fatalf("assignee = %s, want the issue to stay with %s", got, actor)
			}
		})

		for title, wantHint := range map[string]string{
			// A path in a title is execution work, not a D8 co-sign.
			"fix: touch server/migrations/001_x.sql": "scope-author",
			// A code review is routed to the capability_routing reviewers.
			"review(code): review PR 2121": "scope-reviewer",
			"review: review PR 2121":       "scope-reviewer",
		} {
			t.Run(writer.name+"/agent assigning "+title+" to Sol is refused", func(t *testing.T) {
				actor, issueID := newIssue(t, title)
				resp := writer.write(t, issueID, toSol, agentHeaders(t, actor, issueID))
				if writer.name == "single" {
					resp.Want(http.StatusForbidden)
					var body struct {
						Error string `json:"error"`
					}
					resp.JSON(&body)
					for _, want := range []string{"agent_role_scope", "scope-sol", wantHint} {
						if !strings.Contains(body.Error, want) {
							t.Fatalf("refusal %q must name %q", body.Error, want)
						}
					}
				}
				if got := assignee(t, issueID); got != actor {
					t.Fatalf("assignee = %s, want the issue to stay with %s", got, actor)
				}
			})
		}

		t.Run(writer.name+"/human assigning the same issue to Sol succeeds", func(t *testing.T) {
			_, issueID := newIssue(t, "feat(x): ship the thing")
			writer.write(t, issueID, toSol, nil).Want(http.StatusOK)
			if got := assignee(t, issueID); got != solID {
				t.Fatalf("assignee = %s, want Sol", got)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'agent_role_scope_override'`, issueID); n != 1 {
				t.Fatalf("audit notes = %d, want 1 on the issue a person gave to an out-of-scope agent", n)
			}
		})

		for _, title := range []string{
			"plan(x): shape the thing",
			"decision(x): pick one",
			"debate(x): weigh it",
			"security(auth): co-sign the change",
		} {
			t.Run(writer.name+"/agent assigning "+title+" to Sol succeeds", func(t *testing.T) {
				actor, issueID := newIssue(t, title)
				writer.write(t, issueID, toSol, agentHeaders(t, actor, issueID)).Want(http.StatusOK)
				if got := assignee(t, issueID); got != solID {
					t.Fatalf("assignee = %s, want Sol", got)
				}
			})
		}

		t.Run(writer.name+"/an agent outside the scope map stays unrestricted", func(t *testing.T) {
			actor, issueID := newIssue(t, "feat(x): ship the thing")
			writer.write(t, issueID, toOther, agentHeaders(t, actor, issueID)).Want(http.StatusOK)
			if got := assignee(t, issueID); got != otherID {
				t.Fatalf("assignee = %s, want the unrestricted agent", got)
			}
		})
	}
}

// The scope shows on the single-agent read and only for an agent that has one.
func TestAgentGetShowsRoleScope_CHE9192(t *testing.T) {
	solID := createHandlerTestAgent(t, "scope-get-sol", nil)
	otherID := createHandlerTestAgent(t, "scope-get-other", nil)
	setWorkspaceSettings(t, mustJSON(t, map[string]any{
		"agent_role_scope": map[string]any{"agents": map[string]any{solID: []string{"plan", "decision"}}},
	}))
	get := func(id string) map[string]any {
		req := withURLParam(newRequest("GET", "/api/agents/"+id, nil), "id", id)
		var out map[string]any
		testutil.Call(t, testHandler.GetAgent, req).Want(http.StatusOK).JSON(&out)
		return out
	}
	if got, _ := get(solID)["role_scope"].([]any); len(got) != 2 {
		t.Fatalf("role_scope = %v, want [decision plan]", got)
	}
	if _, has := get(otherID)["role_scope"]; has {
		t.Fatal("an unrestricted agent must not carry role_scope")
	}
}

// The setting changes only through the settings patch, which refuses a bad
// shape and an id that is not a workspace agent.
func TestAgentRoleScopeSettingValidation_CHE9192(t *testing.T) {
	if err := checkAgentRoleScopeAgents(t.Context(), testHandler.Queries, parseUUID(testWorkspaceID), map[string]json.RawMessage{
		agentRoleScopeKey: []byte(`{"agents":{"33333333-3333-4333-8333-333333333333":["plan"]}}`),
	}); err == nil {
		t.Fatal("an id that is not a workspace agent must be refused")
	}
	if err := checkAgentRoleScopeAgents(t.Context(), testHandler.Queries, parseUUID(testWorkspaceID), map[string]json.RawMessage{
		agentRoleScopeKey: []byte(`{"agents":{"x":["plan"]}}`),
	}); err == nil {
		t.Fatal("a malformed id must be refused")
	}
}
