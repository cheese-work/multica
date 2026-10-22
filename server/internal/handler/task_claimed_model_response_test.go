package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// claimedModelRow decodes one run row from a raw response body and reports
// whether the claimed_model key is on the wire at all (an old server omits it),
// and its value (nil = stored/unset NULL).
func claimedModelRow(t *testing.T, body []byte, taskID string) (present bool, value any) {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode runs: %v\n%s", err, body)
	}
	for _, row := range rows {
		if row["id"] == taskID {
			value, present = row["claimed_model"]
			return present, value
		}
	}
	t.Fatalf("task %s missing from response: %s", taskID, body)
	return false, nil
}

// TestTaskReadsExposeClaimedModel pins the CHE-1962 read contract for
// `multica agent tasks` and `multica issue runs`: each run carries the exact
// persisted claimed_model. JSON null = no value stored (unclaimed, or the claim
// could not record one); "" = an empty string was stored; a missing key can then
// only mean a server that predates the field. The value is never derived from
// the agent's current model.
func TestTaskReadsExposeClaimedModel(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	agentID := createHandlerTestAgent(t, "ClaimedModelAgent", []byte("[]"))
	// A current agent model that must never leak into a run's claimed_model.
	if _, err := testPool.Exec(ctx, `UPDATE agent SET model = 'current-agent-model' WHERE id = $1`, agentID); err != nil {
		t.Fatalf("set agent model: %v", err)
	}

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
		VALUES ($1, 'claimed-model-issue', 'todo', 'medium', $2, 'member', 92778, 0)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	newTask := func(claimed any) string {
		return dbfx.Task(t, agentID, testutil.Cols{
			"runtime_id":    handlerTestRuntimeID(t),
			"issue_id":      issueID,
			"status":        "completed",
			"claimed_model": claimed,
		})
	}
	claimed := newTask("claude-sonnet-5-5")
	unclaimed := newTask(nil)
	emptyStored := newTask("")

	want := map[string]any{claimed: "claude-sonnet-5-5", unclaimed: nil, emptyStored: ""}

	reads := map[string]func() []byte{
		"agent tasks": func() []byte {
			req := withURLParam(newRequest(http.MethodGet, "/api/agents/"+agentID+"/tasks", nil), "id", agentID)
			return testutil.Call(t, testHandler.ListAgentTasks, req).Want(http.StatusOK).Body.Bytes()
		},
		"issue runs": func() []byte {
			req := withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID+"/task-runs", nil), "id", issueID)
			return testutil.Call(t, testHandler.ListTasksByIssue, req).Want(http.StatusOK).Body.Bytes()
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			body := read()
			for id, wantValue := range want {
				present, got := claimedModelRow(t, body, id)
				if !present {
					t.Errorf("task %s: claimed_model key missing", id)
				} else if got != wantValue {
					t.Errorf("task %s: claimed_model = %#v, want %#v", id, got, wantValue)
				}
			}
			if strings.Contains(string(body), "current-agent-model") {
				t.Errorf("claimed_model must not be synthesized from the agent's current model: %s", body)
			}
		})
	}
}

// TestTaskReadsClaimedModelKeepAuthorizationBoundaries: the new field rides the
// existing membership and agent-access checks, so a non-member and a plain
// member of a private agent get an error body with no claimed model in it.
func TestTaskReadsClaimedModelKeepAuthorizationBoundaries(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const secretModel = "secret-claimed-model-che1962"

	agentID, _, memberID := privateAgentTestFixture(t)
	dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":    handlerTestRuntimeID(t),
		"status":        "completed",
		"claimed_model": secretModel,
	})

	var outsiderID string
	dbfx.QueryRow(t, `INSERT INTO "user" (name, email) VALUES ('Outsider', 'claimed-model-outsider@multica.test') RETURNING id`).Scan(&outsiderID)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, outsiderID) })

	for name, userID := range map[string]string{"plain member": memberID, "non-member": outsiderID} {
		t.Run("agent tasks/"+name, func(t *testing.T) {
			req := withURLParam(newRequestAs(userID, http.MethodGet, "/api/agents/"+agentID+"/tasks", nil), "id", agentID)
			w := httptest.NewRecorder()
			testHandler.ListAgentTasks(w, req)
			if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 403/404: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), secretModel) {
				t.Fatalf("denied response leaked claimed model: %s", w.Body.String())
			}
		})
	}

	// Foreign workspace issue: a non-member cannot read its runs.
	var foreignWS, foreignIssue string
	dbfx.QueryRow(t, `INSERT INTO workspace (name, slug, issue_prefix) VALUES ('Claimed Model Foreign', 'claimed-model-foreign', 'CMF') RETURNING id`).Scan(&foreignWS)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, foreignWS) })
	dbfx.QueryRow(t, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
		VALUES ($1, 'foreign', 'todo', 'medium', $2, 'member', 1, 0) RETURNING id`, foreignWS, outsiderID).Scan(&foreignIssue)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, foreignIssue) })

	dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":    handlerTestRuntimeID(t),
		"issue_id":      foreignIssue,
		"status":        "completed",
		"claimed_model": secretModel,
	})

	req := withURLParam(newRequestAs(testUserID, http.MethodGet, "/api/issues/"+foreignIssue+"/task-runs", nil), "id", foreignIssue)
	w := httptest.NewRecorder()
	testHandler.ListTasksByIssue(w, req)
	if w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
		t.Fatalf("foreign issue runs: status = %d, want 403/404: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secretModel) {
		t.Fatalf("foreign issue runs leaked claimed model: %s", w.Body.String())
	}
}

// The daemon claim response is built before the claim writes claimed_model, so
// it must report the value this claim stores, not the pre-claim null.
func TestClaimResponseReportsStoredClaimedModel(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	for name, model := range map[string]string{"model set": "claim-response-model", "agent model empty": ""} {
		t.Run(name, func(t *testing.T) {
			agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "CHE-1962 claim response")
			dbfx.Exec(t, `UPDATE agent SET model = $1 WHERE id = $2`, model, agentID)
			sendDirectChat(t, ctx, agentID, sessionID, "hello")
			claimed := claimForkTask(t, runtimeID, daemonID)

			var stored *string
			dbfx.QueryRow(t, `SELECT claimed_model FROM agent_task_queue WHERE id = $1`, claimed.ID).Scan(&stored)
			if stored == nil || *stored != model {
				t.Fatalf("stored claimed_model = %v, want %q", stored, model)
			}
			if claimed.ClaimedModel == nil || *claimed.ClaimedModel != model {
				t.Fatalf("claim response claimed_model = %v, want the stored %q", claimed.ClaimedModel, model)
			}
		})
	}
}
