package handler

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// CHE-6950: the local pool filled at 1,000 and blocked every new continuation.
// The workspace ceiling is 5,000; the 5,001st local wakeup is still refused.
func TestLocalWakeupWorkspaceCeilingIs5000(t *testing.T) {
	agent := dbfx.Agent(t, "workspace ceiling target", testRuntimeID)
	count := func() (n int) {
		dbfx.QueryRow(t, `SELECT count(*) FROM issue_wakeup WHERE workspace_id=$1 AND enabled AND system_rule IS NULL AND default_rule_key IS NULL`, testWorkspaceID).Scan(&n)
		return n
	}
	have := count()
	// Fill with enabled local wakeups, 32 per issue, up to `target`.
	fill := func(target int) {
		have = count()
		for ; have < target; have += 32 {
			issue := dbfx.Issue(t, "workspace ceiling fill")
			dbfx.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
			n := min(32, target-have)
			dbfx.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,created_by,instruction,kind,mode,event_types)
 SELECT gen_random_uuid(),$1,$2,$3,$4,'fill','event','continuous',ARRAY['comment.created'] FROM generate_series(1,$5::int)`, testWorkspaceID, issue, agent, testUserID, n)
		}
		have = target
	}
	create := func() *httptest.ResponseRecorder {
		issue := dbfx.Issue(t, "workspace ceiling create")
		dbfx.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
		req := withURLParam(newRequest("POST", "/", map[string]any{"agent_id": agent, "kind": "at", "after_seconds": 600, "instruction": "check"}), "id", issue)
		rec := httptest.NewRecorder()
		testHandler.CreateIssueWakeup(rec, req)
		return rec
	}
	if have > 4900 {
		t.Skipf("workspace already holds %d local wakeups", have)
	}
	fill(1000)
	if rec := create(); rec.Code != 201 {
		t.Fatalf("create at 1000 local wakeups %d: %s", rec.Code, rec.Body.String())
	}
	fill(5000)
	if rec := create(); rec.Code != 400 || !strings.Contains(rec.Body.String(), "wakeup_capacity_exceeded") || !strings.Contains(rec.Body.String(), "5000") {
		t.Fatalf("create at 5000 local wakeups %d: %s", rec.Code, rec.Body.String())
	}
}
