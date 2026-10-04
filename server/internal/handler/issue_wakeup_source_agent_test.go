package handler

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The per-issue wakeup list names the agent that created a wakeup so an agent
// can recognise its own wakeups without a local ledger.
func TestIssueWakeupListExposesSourceAgent(t *testing.T) {
	issue := dbfx.Issue(t, "wake source agent")
	agent := dbfx.Agent(t, "wake source agent", testRuntimeID)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
	create := func(instruction string) string {
		body := map[string]any{"agent_id": agent, "kind": "at", "after_seconds": 600, "instruction": instruction}
		rec := httptest.NewRecorder()
		testHandler.CreateIssueWakeup(rec, withURLParam(newRequest("POST", "/", body), "id", issue))
		if rec.Code != 201 {
			t.Fatalf("create %d: %s", rec.Code, rec.Body.String())
		}
		var row struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		return row.ID
	}
	memberWakeup := create("member created")
	agentWakeup := create("agent created")
	task := dbfx.Task(t, agent, testutil.Cols{"runtime_id": testRuntimeID, "issue_id": issue})
	dbfx.Exec(t, "UPDATE issue_wakeup SET source_task_id=$2 WHERE id=$1", agentWakeup, task)

	rec := httptest.NewRecorder()
	testHandler.ListIssueWakeups(rec, withURLParam(newRequest("GET", "/", nil), "id", issue))
	if rec.Code != 200 {
		t.Fatalf("list %d: %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 wakeups, got %d", len(rows))
	}
	for _, row := range rows {
		switch row["id"] {
		case agentWakeup:
			if row["created_by_agent"] != true || row["source_agent_id"] != agent || row["source_agent_name"] != "wake source agent" {
				t.Fatalf("agent-created wakeup lost source agent: %v", row)
			}
		case memberWakeup:
			id, ok := row["source_agent_id"]
			if row["created_by_agent"] != false || !ok || id != nil {
				t.Fatalf("member-created wakeup must serialize source_agent_id as null: %v", row)
			}
		default:
			t.Fatalf("unexpected wakeup %v", row["id"])
		}
	}
}
