package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

const jevKey = "agent_decisions_jev"

func storedJev(t *testing.T, wsID string) (agentID, mode string, present bool) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT settings ? 'agent_decisions_jev', coalesce(settings->'agent_decisions_jev'->>'agent_id',''), coalesce(settings->'agent_decisions_jev'->>'mode','') FROM workspace WHERE id=$1`, wsID).Scan(&present, &agentID, &mode)
	return
}

// The Jev registration is owner-only through its own route. The generic
// settings save carries a whole blob and is open to agent actors, so it must
// neither set, change nor erase the key.
func TestJevSettingGenericSettingsSaveCannotSet(t *testing.T) {
	wsID := pauseWorkspaceFixture(t, "jev-generic-set")
	saveWorkspaceSettings(t, wsID, map[string]any{jevKey: map[string]any{"agent_id": "00000000-0000-0000-0000-000000000009", "mode": "live"}})
	if _, _, present := storedJev(t, wsID); present {
		t.Fatal("a generic settings save registered a Jev agent")
	}
}

func TestJevSettingGenericSettingsSaveCannotChangeOrErase(t *testing.T) {
	wsID := pauseWorkspaceFixture(t, "jev-generic-keep")
	const owner = "00000000-0000-0000-0000-00000000000a"
	dbfx.Exec(t, `UPDATE workspace SET settings=jsonb_build_object('agent_decisions_jev',jsonb_build_object('agent_id',$2::text,'mode','shadow')) WHERE id=$1`, wsID, owner)
	for name, settings := range map[string]map[string]any{
		"overwrite":    {jevKey: map[string]any{"agent_id": "00000000-0000-0000-0000-00000000000b", "mode": "live"}},
		"empty_object": {jevKey: map[string]any{}},
		"null":         {jevKey: nil},
		"stale_blob":   {"github_pr_sidebar_enabled": false},
		"empty_blob":   {},
	} {
		saveWorkspaceSettings(t, wsID, settings)
		if agentID, mode, present := storedJev(t, wsID); !present || agentID != owner || mode != "shadow" {
			t.Fatalf("%s: stored Jev = (%q, %q, present=%v), want the owner's shadow registration unchanged", name, agentID, mode, present)
		}
	}
}

func TestJevSettingNonObjectSettingsSaveRefused(t *testing.T) {
	wsID := pauseWorkspaceFixture(t, "jev-generic-nonobject")
	dbfx.Exec(t, `UPDATE workspace SET settings=jsonb_build_object('agent_decisions_jev',jsonb_build_object('agent_id','00000000-0000-0000-0000-00000000000a','mode','live')) WHERE id=$1`, wsID)
	req := withURLParam(newRequest("PATCH", "/api/workspaces/"+wsID, map[string]any{"settings": "wiped"}), "id", wsID)
	testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusBadRequest)
	if _, mode, present := storedJev(t, wsID); !present || mode != "live" {
		t.Fatal("a non-object settings save erased the Jev registration")
	}
}

// The guard does not depend on who calls: a task token or cloud PAT carries the
// owner's user id, so the generic save must drop the key for them too.
func TestJevSettingGenericSettingsSaveByMachineActorCannotSet(t *testing.T) {
	for _, source := range []string{"task_token", "cloud_pat"} {
		wsID := pauseWorkspaceFixture(t, "jev-generic-"+source)
		req := withURLParam(newRequest("PATCH", "/api/workspaces/"+wsID, map[string]any{"settings": map[string]any{
			jevKey: map[string]any{"agent_id": "00000000-0000-0000-0000-000000000009", "mode": "live"},
		}}), "id", wsID)
		req.Header.Set("X-Actor-Source", source)
		testutil.Call(t, testHandler.UpdateWorkspace, req)
		if _, _, present := storedJev(t, wsID); present {
			t.Fatalf("%s registered a Jev agent through the generic settings save", source)
		}
	}
}
