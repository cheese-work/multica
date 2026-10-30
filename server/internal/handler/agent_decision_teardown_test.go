package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestAgentDecisionWorkspaceDeletion(t *testing.T) {
	t.Setenv("MULTICA_VOTE_GATE_LABS", voteLabs)
	workspaceID := dbfx.Workspace(t, "agent decision teardown", "decision-teardown-"+uuid.NewString())
	scoped := testutil.New(testPool, workspaceID, testUserID)
	scoped.Member(t, workspaceID, testUserID, "owner")
	f := agentDecisionFixture{voteFixture: voteFixture{issueID: scoped.Issue(t, "agent decision teardown"), tasks: map[string]string{}}, instance: uuid.NewString(), workspaceID: workspaceID}
	claude := scoped.Runtime(t, "teardown claude", testutil.Cols{"provider": "claude"})
	codex := scoped.Runtime(t, "teardown codex", testutil.Cols{"provider": "codex"})
	agentOn := func(name, runtimeID, model string) string {
		id := scoped.Agent(t, name, runtimeID, testutil.Cols{"model": model})
		f.tasks[id] = scoped.Task(t, id, testutil.Cols{"runtime_id": runtimeID, "claimed_model": model, "issue_id": f.issueID, "status": "running"})
		return id
	}
	f.writerID = agentOn("teardown writer", claude, "claude-test")
	f.leaderID = agentOn("teardown leader", codex, "gpt-test")
	f.peerID = agentOn("teardown peer", claude, "claude-test")
	f.taskID = f.tasks[f.writerID]
	f.scope = map[string]any{
		"work_author_id": f.writerID, "waived_gate": "screenshot_evidence", "route": "cosign",
		"signers":  []string{f.leaderID, f.peerID},
		"evidence": []map[string]string{{"url": "https://example.com/frozen.md", "sha256": boundAfter}},
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM issue WHERE id=$1`, f.issueID)
		dbfx.Exec(t, `DELETE FROM governed_vote_record WHERE issue_id=$1`, f.issueID)
		dbfx.Exec(t, `DELETE FROM decision_agent_scope WHERE instance_id=$1`, f.instance)
		dbfx.Exec(t, `DELETE FROM decision_request WHERE id=$1`, f.instance)
	})
	f.propose(t)
	f.commit(t, f.leaderID, f.opinion(f.leaderID, "PASS"))
	if got := dbfx.Count(t, `SELECT count(*) FROM governed_vote_record WHERE decision_instance_id=$1`, f.instance); got != 2 {
		t.Fatalf("proposal and commitment snapshots = %d, want 2", got)
	}

	w := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+workspaceID, nil), "id", workspaceID)
	testHandler.DeleteWorkspace(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace = %d, want 204: %s", w.Code, w.Body.String())
	}
	resetDecisionLease(t)
	_, runner := newDecisionRunnerFixture(t)
	decisionPass(t, runner)
	for _, table := range []string{"governed_vote_record", "decision_agent_scope", "decision_request"} {
		if got := dbfx.Count(t, "SELECT count(*) FROM "+table+" WHERE workspace_id=$1", workspaceID); got != 0 {
			t.Errorf("%s has %d rows after workspace deletion and worker cleanup", table, got)
		}
	}
}

func TestAgentDecisionTeardownModeOnlyAllowsDeletes(t *testing.T) {
	f := newAgentDecisionFixture(t)
	f.propose(t)
	f.commit(t, f.leaderID, f.opinion(f.leaderID, "PASS"))
	for _, row := range []struct{ table, key, field string }{
		{"decision_agent_scope", "instance_id", "scope"},
		{"decision_request", "id", "ask"},
		{"governed_vote_record", "decision_instance_id", "full_body"},
	} {
		for _, operation := range []string{"update", "delete"} {
			t.Run(row.table+"/"+operation, func(t *testing.T) {
				tx, err := testPool.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				if _, err := tx.Exec(context.Background(), `SET LOCAL multica.workspace_teardown = 'on'`); err != nil {
					t.Fatal(err)
				}
				statement := "DELETE FROM " + row.table + " WHERE " + row.key + "=$1"
				if operation == "update" {
					statement = "UPDATE " + row.table + " SET " + row.field + "=" + row.field + " WHERE " + row.key + "=$1"
				}
				_, err = tx.Exec(context.Background(), statement, f.instance)
				if operation == "delete" {
					if err != nil {
						t.Fatalf("teardown delete: %v", err)
					}
				} else {
					var pgErr *pgconn.PgError
					if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
						t.Fatalf("teardown mode allowed an immutable update: %v", err)
					}
				}
			})
		}
	}
}
