package handler

import (
	"net/http"
	"strconv"
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

// An agent proposes a workspace or project rule; the server stores it disabled
// and no one below it can switch it on until a human admin enables it. An agent
// cannot edit, delete or write at issue scope (CHE-1082 15 Oct scope amendment,
// built in CHE-3671).
func TestWakeupDefinitionAgentProposalIsStoredDisabledUntilAnAdminEnables(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	restoreWorkspaceSettings(t)
	agent := k.asAgentRun()
	member := wdMember(t, "WD Qualification Member", "member")
	body := cfg(map[string]any{"enabled": true, "trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x", "target": map[string]any{"type": "agent", "id": handlerTestAgentID(t)}})
	for _, s := range []wdScope{k.workspace(), k.project()} {
		wipeWakeupDefinitions(t)
		root := decodeDefinition(t, s.create(body, agent).Want(http.StatusCreated))
		if enabled := string(configKeys(t, root.Config)["enabled"]); enabled != "false" {
			t.Fatalf("%s: agent proposal stored enabled = %s, want false", s.kind, enabled)
		}
		if n := wdCount(t); n != 1 {
			t.Fatalf("%s: %d definitions stored, want 1", s.kind, n)
		}
		// The agent cannot edit or delete its proposal, or write at issue scope.
		s.put(root.RuleKey, root.Revision, body, agent).Want(http.StatusForbidden)
		s.del(root.RuleKey, strconv.FormatInt(int64(root.Revision), 10), agent).Want(http.StatusForbidden)
		k.issue().put(root.RuleKey, 0, cfg(map[string]any{"enabled": true}), agent).Want(http.StatusForbidden)
		// A member's issue override does not switch the proposal on.
		k.issue().put(root.RuleKey, 0, cfg(map[string]any{"enabled": true}), wdAsUser(member)).Want(http.StatusOK)
		if eff := k.issue().effective(root.RuleKey); eff.Enabled {
			t.Fatalf("%s: proposal ran before an admin enabled it: %+v", s.kind, eff)
		}
		// The admin enables the root; the chain now reports it enabled.
		s.put(root.RuleKey, root.Revision, body).Want(http.StatusOK)
		if eff := k.issue().effective(root.RuleKey); !eff.Enabled {
			t.Fatalf("%s: admin-enabled rule stayed disabled: %+v", s.kind, eff)
		}
	}
}
