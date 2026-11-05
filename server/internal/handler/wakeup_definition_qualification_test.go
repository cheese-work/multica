package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// restoreWorkspaceSettings undoes the settings aliases a built-in rule's
// workspace write leaves behind.
func restoreWorkspaceSettings(t *testing.T) {
	t.Helper()
	var settings string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, settings, testWorkspaceID)
	})
}

// The workspace -> project -> issue chain, read the way every client reads it
// (CHE-1082 L15): inherited, overridden, disabled, then the runtime instance's
// pause state on the same response.
func TestWakeupDefinitionChainReportsInheritanceOverrideDisableAndRuntime(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	const rule = service.SystemRulePRMerged
	restoreWorkspaceSettings(t)
	instruction := func() string { return string(configKeys(t, k.issue().effective(rule).Config)["instruction"]) }

	// Workspace default only: the issue inherits every field from it.
	k.workspace().put(rule, 0, cfg(map[string]any{"enabled": true, "instruction": "workspace text"})).Want(http.StatusOK)
	eff := k.issue().effective(rule)
	if !eff.Enabled || eff.Sources["instruction"] != "workspace" || len(eff.Overrides) != 0 || eff.Execution != nil {
		t.Fatalf("inherited: %+v", eff)
	}

	// A project override beats the workspace for that field only.
	k.project().put(rule, 0, cfg(map[string]any{"instruction": "project text"})).Want(http.StatusOK)
	eff = k.issue().effective(rule)
	if eff.Sources["instruction"] != "project" || eff.Sources["enabled"] != "workspace" || !eff.Enabled || instruction() != `"project text"` {
		t.Fatalf("project override: %+v", eff)
	}

	// An issue override beats both; disabling it turns the rule off here alone.
	k.issue().put(rule, 0, cfg(map[string]any{"enabled": false})).Want(http.StatusOK)
	eff = k.issue().effective(rule)
	if eff.Enabled || eff.Sources["enabled"] != "issue" || eff.Sources["instruction"] != "project" {
		t.Fatalf("issue disable: %+v", eff)
	}
	if ws := k.workspace().effective(rule); !ws.Enabled {
		t.Fatalf("the issue's disable leaked up to the workspace: %+v", ws)
	}

	// The runtime instance's pause state rides on the issue-scope response only.
	dbfx.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,instruction,kind,mode,event_types,enabled,paused_reason,system_rule)
 VALUES(gen_random_uuid(),$1,$2,'','event','continuous','{pr.merged}',true,'loop',$3)`, testWorkspaceID, k.issueID, rule)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM issue_wakeup WHERE issue_id=$1`, k.issueID) })
	eff = k.issue().effective(rule)
	if eff.Execution == nil || !eff.Execution.Enabled || eff.Execution.PausedReason == nil || *eff.Execution.PausedReason != "loop" || eff.Execution.InstanceID == "" {
		t.Fatalf("runtime state: %+v", eff.Execution)
	}
	if proj := k.project().effective(rule); proj.Execution != nil {
		t.Fatalf("project scope reported a per-issue runtime instance: %+v", proj.Execution)
	}

	// Reset removes the issue override: the chain's value comes back, the
	// runtime instance keeps its identity.
	id := eff.Execution.InstanceID
	k.issue().del(rule, "1").Want(http.StatusNoContent)
	eff = k.issue().effective(rule)
	if !eff.Enabled || eff.Sources["enabled"] != "workspace" || eff.Execution == nil || eff.Execution.InstanceID != id {
		t.Fatalf("after reset: %+v", eff)
	}
}

// Agents never author a definition, so none can broaden a default; an admin
// session writes the same rule right after (CHE-1082 15 Oct scope amendment).
func TestWakeupDefinitionAgentWriteIsRefusedAndStoresNothing(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	restoreWorkspaceSettings(t)
	agent := k.asAgentRun()
	for _, s := range []wdScope{k.workspace(), k.project(), k.issue()} {
		s.put(service.SystemRulePRMerged, 0, cfg(map[string]any{"enabled": true}), agent).Want(http.StatusForbidden)
	}
	if n := wdCount(t); n != 0 {
		t.Fatalf("a refused agent write stored %d definitions", n)
	}
	k.workspace().put(service.SystemRulePRMerged, 0, cfg(map[string]any{"enabled": true})).Want(http.StatusOK)
}
