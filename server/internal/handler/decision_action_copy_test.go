package handler

import (
	"net/http"
	"strings"
	"testing"
)

// newPendingCopy asks to copy the fixture agent onto its own runtime, shared
// with the workspace so the approver may use it.
func newPendingCopy(test *testing.T, name string, extra map[string]any) pendingAction {
	test.Helper()
	return newPendingCopyOn(test, name, extra, "public")
}

func newPendingCopyOn(test *testing.T, name string, extra map[string]any, visibility string) pendingAction {
	test.Helper()
	fixture := newActionFixture(test)
	dbfx.Exec(test, `UPDATE agent SET instructions = 'be careful' WHERE id = $1`, fixture.agentID)
	var runtimeID, was string
	dbfx.QueryRow(test, `SELECT runtime_id::text FROM agent WHERE id = $1`, fixture.agentID).Scan(&runtimeID)
	dbfx.QueryRow(test, `SELECT visibility FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&was)
	dbfx.Exec(test, `UPDATE agent_runtime SET visibility = $2 WHERE id = $1`, runtimeID, visibility)
	dbfx.Cleanup(test, `UPDATE agent_runtime SET visibility = '`+was+`' WHERE id = '`+runtimeID+`'`)
	params := map[string]any{"source_agent_id": fixture.agentID, "name": name, "runtime_id": runtimeID}
	for key, value := range extra {
		params[key] = value
	}
	var created actionResponse
	fixture.respond(test, fixture.withAction(map[string]any{"type": "agent.copy", "version": 1, "params": params}), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	dbfx.Cleanup(test, `DELETE FROM agent WHERE name = $1`, name)
	placeDecisionHuman(test, fixture.issueID, fixture.humanID, "member", true)
	return pendingAction{fixture, created.ID, created.Action.ID}
}

func TestApproveAgentCopyCreatesTheAgentAsTheApprover(test *testing.T) {
	action := newPendingCopy(test, "che2233 copy verbatim", nil)
	var out decisionActionResponse
	raw := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	decodeJSON(test, raw, &out)
	if out.State != "executed" {
		test.Fatalf("response state = %s: %s", out.State, raw)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE name = 'che2233 copy verbatim' AND owner_id = $1 AND instructions = 'be careful' AND archived_at IS NULL`, action.humanID); n != 1 {
		test.Fatal("copy is not one live agent owned by the approver with the source instructions")
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE id = $1 AND state = 'executed' AND result->>'agent_id' IS NOT NULL AND post_commit_done_at IS NOT NULL`, action.actionID); n != 1 {
		test.Fatal("result does not name the new agent")
	}
}

func TestApproveAgentCopyAppliesReplacementInstructions(test *testing.T) {
	action := newPendingCopy(test, "che2233 copy replaced", map[string]any{"instructions": "new text"})
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE name = 'che2233 copy replaced' AND instructions = 'new text'`); n != 1 {
		test.Fatal("copy does not carry the approved replacement instructions")
	}
}

func TestApproveAgentCopyRefusesWhenTheSourceInstructionsDrifted(test *testing.T) {
	action := newPendingCopy(test, "che2233 copy drifted", nil)
	dbfx.Exec(test, `UPDATE agent SET instructions = 'changed after the ask' WHERE id = $1`, action.agentID)
	body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusConflict)
	if !strings.Contains(body, "precondition_drift") {
		test.Fatalf("body %q lacks precondition_drift", body)
	}
	action.assertUntouched(test)
	if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE name = 'che2233 copy drifted'`); n != 0 {
		test.Fatal("a copy was created from drifted instructions")
	}
}

func TestApproveAgentCopyNameTakenFailsAndKeepsTheClick(test *testing.T) {
	action := newPendingCopy(test, "che2233 copy taken", nil)
	dbfx.Exec(test, `INSERT INTO agent (workspace_id, name, runtime_mode, runtime_id, owner_id) SELECT workspace_id, 'che2233 copy taken', runtime_mode, runtime_id, owner_id FROM agent WHERE id = $1`, action.agentID)
	var out decisionActionResponse
	decodeJSON(test, action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK), &out)
	if out.State != "failed" {
		test.Fatalf("response state = %s, want failed", out.State)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE id = $1 AND state = 'failed' AND error_code = 'name_taken'`, action.actionID); n != 1 {
		test.Fatal("failure is not recorded as name_taken")
	}
}

func TestApproveAgentCopyOnAPrivateRuntimeOfAnotherMemberFails(test *testing.T) {
	action := newPendingCopyOn(test, "che2233 copy private runtime", nil, "private")
	var out decisionActionResponse
	decodeJSON(test, action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK), &out)
	if out.State != "failed" {
		test.Fatalf("response state = %s, want failed", out.State)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE name = 'che2233 copy private runtime'`); n != 0 {
		test.Fatal("the approver created an agent on a runtime private to someone else")
	}
}

func TestApproveAgentCopyRefusesASourceTheApproverCannotSee(test *testing.T) {
	action := newPendingCopy(test, "che2233 copy hidden source", nil)
	dbfx.Exec(test, `UPDATE agent SET owner_id = $2, permission_mode = 'private' WHERE id = $1`, action.agentID, testUserID)
	dbfx.Exec(test, `UPDATE member SET role = 'member' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusForbidden)
	if !strings.Contains(body, "source_not_visible") {
		test.Fatalf("body %q lacks source_not_visible", body)
	}
	action.assertUntouched(test)
	if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE name = 'che2233 copy hidden source'`); n != 0 {
		test.Fatal("a copy of a private source the approver cannot view was created")
	}
}

func TestApproveAgentCopyAllowsAWorkspaceAdminToCopyASourceOwnedByAnotherMember(test *testing.T) {
	action := newPendingCopy(test, "che2233 copy admin source", nil)
	dbfx.Exec(test, `UPDATE agent SET owner_id = $2 WHERE id = $1`, action.agentID, testUserID)
	dbfx.Exec(test, `UPDATE member SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)
	if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE name = 'che2233 copy admin source' AND owner_id = $1`, action.humanID); n != 1 {
		test.Fatal("an admin's approved copy was not created")
	}
}

func TestApproveAgentCopyRefusesWhenTheSourcePermissionsDrifted(test *testing.T) {
	action := newPendingCopy(test, "che2233 copy widened", nil)
	dbfx.Exec(test, `UPDATE member SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	dbfx.Exec(test, `UPDATE agent SET permission_mode = 'private' WHERE id = $1`, action.agentID)
	body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusConflict)
	if !strings.Contains(body, "precondition_drift") {
		test.Fatalf("body %q lacks precondition_drift", body)
	}
	action.assertUntouched(test)
	if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE name = 'che2233 copy widened'`); n != 0 {
		test.Fatal("a copy carried permissions the approver never saw")
	}
}

func TestApproveAgentCopyRefusesWhenTheSourceInvocationTargetsDrifted(test *testing.T) {
	action := newPendingCopy(test, "che2233 copy retargeted", nil)
	dbfx.Exec(test, `UPDATE member SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, action.humanID)
	dbfx.Exec(test, `DELETE FROM agent_invocation_target WHERE agent_id = $1`, action.agentID)
	body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusConflict)
	if !strings.Contains(body, "precondition_drift") {
		test.Fatalf("body %q lacks precondition_drift", body)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE name = 'che2233 copy retargeted'`); n != 0 {
		test.Fatal("a copy carried invocation targets the approver never saw")
	}
}
