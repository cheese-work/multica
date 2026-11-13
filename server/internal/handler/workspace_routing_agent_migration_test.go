package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4116: the routing setting lives in background_agents.routing. The old
// route stays as an alias for one release, and a startup copy moves a value
// stored under settings.routing_agent.

func workspaceSettingsMap(t *testing.T, wsID string) map[string]any {
	t.Helper()
	var raw []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id=$1`, wsID).Scan(&raw)
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestRoutingAgentOldRouteAndNewKeyReturnTheSameChain(t *testing.T) {
	wsID, primary, fallback, _ := routingFixture(t, "ra-alias")
	putRoutingAgent(t, wsID, map[string]any{"agent_id": primary, "fallback_agent_id": fallback}, nil).Want(http.StatusOK)

	settings := workspaceSettingsMap(t, wsID)
	if _, old := settings[routingAgentKey]; old {
		t.Fatalf("old key still written: %v", settings)
	}
	stored, _ := settings[backgroundAgentsKey].(map[string]any)
	chain, _ := stored["routing"].(map[string]any)
	if chain["agent_id"] != primary || chain["fallback_agent_id"] != fallback {
		t.Fatalf("background_agents.routing = %v, want primary %s fallback %s", chain, primary, fallback)
	}

	var viaOld, viaNew map[string]any
	req := withURLParam(newRequest("GET", "/api/workspaces/"+wsID+"/routing-agent", nil), "id", wsID)
	testutil.Call(t, testHandler.GetRoutingAgent, req).Want(http.StatusOK).JSON(&viaOld)
	var all map[string]map[string]any
	req = withURLParam(newRequest("GET", "/api/workspaces/"+wsID+"/background-agents", nil), "id", wsID)
	testutil.Call(t, testHandler.GetBackgroundAgents, req).Want(http.StatusOK).JSON(&all)
	viaNew = all["routing"]
	if viaOld["agent_id"] != viaNew["agent_id"] || viaOld["fallback_agent_id"] != viaNew["fallback_agent_id"] {
		t.Fatalf("old route %v and new key %v differ", viaOld, viaNew)
	}
	if got := routedAgent(t, wsID, "todo"); got != primary {
		t.Fatalf("todo routed to %q, want %q", got, primary)
	}
}

func TestRoutingAgentWriteKeepsOtherBackgroundJobs(t *testing.T) {
	wsID, primary, _, other := routingFixture(t, "ra-keeps-jobs")
	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_build_object('background_agents', jsonb_build_object('dm_title', jsonb_build_object('agent_id', $1::text))) WHERE id=$2`, other, wsID)
	putRoutingAgent(t, wsID, map[string]any{"agent_id": primary}, nil).Want(http.StatusOK)
	stored, _ := workspaceSettingsMap(t, wsID)[backgroundAgentsKey].(map[string]any)
	if dm, _ := stored["dm_title"].(map[string]any); dm["agent_id"] != other {
		t.Fatalf("routing write dropped dm_title: %v", stored)
	}
	// Turning routing off removes the job, not the whole value.
	putRoutingAgent(t, wsID, map[string]any{}, nil).Want(http.StatusOK)
	stored, _ = workspaceSettingsMap(t, wsID)[backgroundAgentsKey].(map[string]any)
	if _, ok := stored["routing"]; ok || stored["dm_title"] == nil {
		t.Fatalf("clearing routing left %v", stored)
	}
}

func TestRoutingAgentStartupCopyRunsTwiceWithOneResult(t *testing.T) {
	wsID, primary, fallback, other := routingFixture(t, "ra-copy")
	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_build_object(
		'routing_agent', jsonb_build_object('agent_id', $1::text, 'fallback_agent_id', $2::text, 'statuses', jsonb_build_array('todo')),
		'background_agents', jsonb_build_object('dm_title', jsonb_build_object('agent_id', $3::text))) WHERE id=$4`, primary, fallback, other, wsID)

	n, err := testHandler.MigrateRoutingAgentSetting(context.Background())
	if err != nil || n < 1 {
		t.Fatalf("first copy = %d, %v; want at least this workspace", n, err)
	}
	first := workspaceSettingsMap(t, wsID)
	if _, old := first[routingAgentKey]; old {
		t.Fatalf("old key not cleared: %v", first)
	}
	stored, _ := first[backgroundAgentsKey].(map[string]any)
	chain, _ := stored["routing"].(map[string]any)
	if chain["agent_id"] != primary || chain["fallback_agent_id"] != fallback || stored["dm_title"] == nil {
		t.Fatalf("copy result = %v", first)
	}
	if got := routedAgent(t, wsID, "todo"); got != primary {
		t.Fatalf("todo routed to %q after copy, want %q", got, primary)
	}

	if n, err := testHandler.MigrateRoutingAgentSetting(context.Background()); err != nil || n != 0 {
		t.Fatalf("second copy = %d, %v; want 0", n, err)
	}
	if second := workspaceSettingsMap(t, wsID); !reflect.DeepEqual(first, second) {
		t.Fatalf("second copy changed settings:\n%v\n%v", first, second)
	}
}

func TestRoutingAgentStartupCopyKeepsAnExistingNewValue(t *testing.T) {
	wsID, primary, _, other := routingFixture(t, "ra-copy-wins")
	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_build_object(
		'routing_agent', jsonb_build_object('agent_id', $1::text),
		'background_agents', jsonb_build_object('routing', jsonb_build_object('agent_id', $2::text))) WHERE id=$3`, primary, other, wsID)
	if _, err := testHandler.MigrateRoutingAgentSetting(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := routedAgent(t, wsID, "todo"); got != other {
		t.Fatalf("copy overwrote the new key: routed to %q, want %q", got, other)
	}
	if _, old := workspaceSettingsMap(t, wsID)[routingAgentKey]; old {
		t.Fatal("old key not cleared")
	}
}
