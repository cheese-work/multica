package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A full default pool must not fail the API call that creates a local wakeup,
// and the local ceiling still answers with its own actionable error.
func TestLocalWakeupCreationIgnoresFullDefaultPool(t *testing.T) {
	issue := dbfx.Issue(t, "default pool full")
	agent := dbfx.Agent(t, "default pool target", testRuntimeID)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
	dbfx.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,event_types,default_rule_key,default_scope_kind,default_scope_id)
 SELECT gen_random_uuid(),$1,$2,$3,'default','event','continuous',ARRAY['comment.created'],'7f1d6a2e-3b44-4d8e-9a55-0c6b1f2e8d10','workspace',$1 FROM generate_series(1,32)`, testWorkspaceID, issue, agent)
	create := func() *httptest.ResponseRecorder {
		req := withURLParam(newRequest("POST", "/", map[string]any{"agent_id": agent, "kind": "at", "after_seconds": 600, "instruction": "check"}), "id", issue)
		rec := httptest.NewRecorder()
		testHandler.CreateIssueWakeup(rec, req)
		return rec
	}
	if rec := create(); rec.Code != 201 {
		t.Fatalf("local create beside a full default pool %d: %s", rec.Code, rec.Body.String())
	}
	dbfx.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,created_by,instruction,kind,mode,event_types)
 SELECT gen_random_uuid(),$1,$2,$3,$4,'check','event','continuous',ARRAY['comment.created'] FROM generate_series(1,31)`, testWorkspaceID, issue, agent, testUserID)
	if rec := create(); rec.Code != 400 || !strings.Contains(rec.Body.String(), "wakeup_capacity_exceeded") || !strings.Contains(rec.Body.String(), "32") {
		t.Fatalf("local ceiling %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWakeupErrorMapsDefaultCapacity(t *testing.T) {
	rec := httptest.NewRecorder()
	wakeupError(rec, &pgconn.PgError{ConstraintName: "issue_wakeup_default_capacity", Message: "Default capacity reached"})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Default capacity reached") {
		t.Fatalf("default capacity %d: %s", rec.Code, rec.Body.String())
	}
}
