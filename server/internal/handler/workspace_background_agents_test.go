package handler

import (
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const bgActivity = "background_agent_changed"

// bgEvents records background-agent events per workspace. The bus has no
// unsubscribe, so each test filters by its own workspace id.
var (
	bgEventsMu  sync.Mutex
	bgEventLog  = map[string][]map[string]any{}
	bgEventOnce sync.Once
)

func bgEventsFor(wsID string) []map[string]any {
	bgEventOnce.Do(func() {
		testHandler.Bus.Subscribe(protocol.EventWorkspaceBackgroundAgentChanged, func(e events.Event) {
			p, _ := e.Payload.(map[string]any)
			bgEventsMu.Lock()
			bgEventLog[e.WorkspaceID] = append(bgEventLog[e.WorkspaceID], p)
			bgEventsMu.Unlock()
		})
	})
	bgEventsMu.Lock()
	defer bgEventsMu.Unlock()
	return append([]map[string]any(nil), bgEventLog[wsID]...)
}

// bgFixture is a workspace with two workspace-visible agents on a runtime and
// one private agent.
func bgFixture(t *testing.T, slug string) (wsID, a, b, private string) {
	t.Helper()
	wsID = pauseWorkspaceFixture(t, slug)
	bgEventsFor(wsID) // subscribe before any write
	runtime := dbfx.Runtime(t, slug, testutil.Cols{"workspace_id": wsID})
	shared := func(name, vis string) string {
		return dbfx.Agent(t, slug+"-"+name, runtime, testutil.Cols{"workspace_id": wsID, "visibility": vis})
	}
	return wsID, shared("a", "workspace"), shared("b", "workspace"), shared("p", "private")
}

func machine(source string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-Actor-Source", source) }
}

func bgCall(t *testing.T, h http.HandlerFunc, method, wsID string, body any, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest(method, "/api/workspaces/"+wsID+"/background-agents", body), "id", wsID)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, h, req)
}

func putBG(t *testing.T, wsID string, body any, tweak func(*http.Request)) *testutil.Response {
	return bgCall(t, testHandler.UpdateBackgroundAgents, "PUT", wsID, body, tweak)
}

func storedBG(t *testing.T, wsID string) any {
	t.Helper()
	var v any
	dbfx.QueryRow(t, `SELECT settings->'background_agents' FROM workspace WHERE id=$1`, wsID).Scan(&v)
	return v
}

func bgRows(t *testing.T, wsID string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM activity_log WHERE workspace_id=$1 AND action=$2`, wsID, bgActivity).Scan(&n)
	return n
}

func settingsCall(t *testing.T, method, wsID string, settings map[string]any, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest(method, "/api/workspaces/"+wsID, map[string]any{"settings": settings}), "id", wsID)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, testHandler.UpdateWorkspace, req)
}

func wantCode(t *testing.T, r *testutil.Response, status int, code string) {
	t.Helper()
	r.Want(status)
	if got := r.Map()["code"]; got != code {
		t.Fatalf("code = %v, want %s: %s", got, code, r.Text())
	}
}

func TestBackgroundAgentsHumanWriteStoresAuditsAndPublishes(t *testing.T) {
	wsID, a, b, _ := bgFixture(t, "bg-human")
	putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
	if got := bgRows(t, wsID); got != 1 {
		t.Fatalf("rows after first write = %d, want 1", got)
	}
	if ev := bgEventsFor(wsID); len(ev) != 1 || ev[0]["job"] != "dm_title" {
		t.Fatalf("events = %v, want one for dm_title", ev)
	}
	// Same value again: no change, no row, no event.
	putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
	if bgRows(t, wsID) != 1 || len(bgEventsFor(wsID)) != 1 {
		t.Fatal("an unchanged write left a row or an event")
	}
	// Two jobs change: one row and one event each.
	putBG(t, wsID, map[string]any{
		"dm_title":      map[string]any{"agent_id": b},
		"quick_actions": map[string]any{"agent_id": a},
	}, nil).Want(http.StatusOK)
	if got := bgRows(t, wsID); got != 3 {
		t.Fatalf("rows = %d, want 3", got)
	}
	if got := len(bgEventsFor(wsID)); got != 3 {
		t.Fatalf("events = %d, want 3", got)
	}
	var out map[string]any
	bgCall(t, testHandler.GetBackgroundAgents, "GET", wsID, nil, nil).Want(http.StatusOK).JSON(&out)
	if _, ok := out["dm_title"]; !ok || len(out) != 2 {
		t.Fatalf("GET = %v, want dm_title and quick_actions", out)
	}
}

func TestBackgroundAgentsRefusesMachineOnEveryPath(t *testing.T) {
	wsID, a, b, _ := bgFixture(t, "bg-machine")
	start := map[string]any{"dm_title": map[string]any{"agent_id": a}}
	putBG(t, wsID, start, nil).Want(http.StatusOK)
	rows, events := bgRows(t, wsID), len(bgEventsFor(wsID))
	changed := map[string]any{"dm_title": map[string]any{"agent_id": b}}
	const code = "background_agents_human_only"

	for _, source := range []string{"task_token", "cloud_pat"} {
		m := machine(source)
		paths := map[string]*testutil.Response{
			"route PUT":      putBG(t, wsID, changed, m),
			"route DELETE":   bgCall(t, testHandler.ResetBackgroundAgents, "DELETE", wsID, nil, m),
			"nested PATCH":   settingsCall(t, "PATCH", wsID, map[string]any{"background_agents": changed}, m),
			"nested PUT":     settingsCall(t, "PUT", wsID, map[string]any{"background_agents": changed}, m),
			"key omitted":    settingsCall(t, "PUT", wsID, map[string]any{"other": 1}, m),
			"key null":       settingsCall(t, "PATCH", wsID, map[string]any{"background_agents": nil}, m),
			"first-time add": nil,
		}
		delete(paths, "first-time add")
		for name, r := range paths {
			t.Run(source+"/"+name, func(t *testing.T) { wantCode(t, r, http.StatusForbidden, code) })
		}
	}
	if got := storedBG(t, wsID); !reflect.DeepEqual(got, map[string]any{"dm_title": map[string]any{"agent_id": a}}) {
		t.Fatalf("a refused write changed the setting: %v", got)
	}
	if bgRows(t, wsID) != rows || len(bgEventsFor(wsID)) != events {
		t.Fatal("a refused write left a row or an event")
	}
}

func TestBackgroundAgentsAcceptsUnchangedMachineWrite(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bg-unchanged")
	val := map[string]any{"dm_title": map[string]any{"agent_id": a}}
	putBG(t, wsID, val, nil).Want(http.StatusOK)
	rows, events := bgRows(t, wsID), len(bgEventsFor(wsID))
	m := machine("task_token")
	putBG(t, wsID, val, m).Want(http.StatusOK)
	settingsCall(t, "PATCH", wsID, map[string]any{"background_agents": val, "other": 1}, m).Want(http.StatusOK)
	settingsCall(t, "PUT", wsID, map[string]any{"background_agents": val}, m).Want(http.StatusOK)
	if bgRows(t, wsID) != rows || len(bgEventsFor(wsID)) != events {
		t.Fatal("an unchanged machine write left a row or an event")
	}
	// A machine write that never had the key and does not add it is fine too.
	other, _, _, _ := bgFixture(t, "bg-unchanged-empty")
	settingsCall(t, "PATCH", other, map[string]any{"other": 1}, m).Want(http.StatusOK)
}

func TestBackgroundAgentsMachineCannotAddFirstValue(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bg-first")
	r := settingsCall(t, "PATCH", wsID, map[string]any{"background_agents": map[string]any{"dm_title": map[string]any{"agent_id": a}}}, machine("task_token"))
	wantCode(t, r, http.StatusForbidden, "background_agents_human_only")
	if storedBG(t, wsID) != nil {
		t.Fatal("machine added the key")
	}
}

func TestBackgroundAgentsHumanNestedWriteKeepsStoredValue(t *testing.T) {
	wsID, a, b, _ := bgFixture(t, "bg-nested")
	putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
	rows := bgRows(t, wsID)
	settingsCall(t, "PATCH", wsID, map[string]any{"background_agents": map[string]any{"dm_title": map[string]any{"agent_id": b}}}, nil).Want(http.StatusOK)
	settingsCall(t, "PUT", wsID, map[string]any{"other": 1}, nil).Want(http.StatusOK)
	if got := storedBG(t, wsID); !reflect.DeepEqual(got, map[string]any{"dm_title": map[string]any{"agent_id": a}}) {
		t.Fatalf("nested write changed the setting: %v", got)
	}
	if bgRows(t, wsID) != rows {
		t.Fatal("a nested write left an activity row")
	}
}

func TestBackgroundAgentsResetClearsAndAudits(t *testing.T) {
	wsID, a, b, _ := bgFixture(t, "bg-reset")
	putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": a}, "routing": map[string]any{"agent_id": b}}, nil).Want(http.StatusOK)
	rows, events := bgRows(t, wsID), len(bgEventsFor(wsID))
	bgCall(t, testHandler.ResetBackgroundAgents, "DELETE", wsID, nil, nil).Want(http.StatusOK)
	if storedBG(t, wsID) != nil {
		t.Fatal("reset left the key")
	}
	if bgRows(t, wsID) != rows+2 || len(bgEventsFor(wsID)) != events+2 {
		t.Fatal("reset must audit and publish once per cleared job")
	}
	// Resetting an empty setting changes nothing.
	bgCall(t, testHandler.ResetBackgroundAgents, "DELETE", wsID, nil, nil).Want(http.StatusOK)
	if bgRows(t, wsID) != rows+2 {
		t.Fatal("an empty reset left a row")
	}
}

func TestBackgroundAgentsValidation(t *testing.T) {
	wsID, a, _, private := bgFixture(t, "bg-validate")
	otherWS, otherAgent, _, _ := bgFixture(t, "bg-validate-other")
	_ = otherWS
	cases := []struct {
		name, code string
		body       any
	}{
		{"unknown job", "unknown_job", map[string]any{"nope": map[string]any{"agent_id": a}}},
		{"unknown field", "unknown_field", map[string]any{"dm_title": map[string]any{"statuses": []string{"todo"}}}},
		{"bad id", "invalid_agent_id", map[string]any{"dm_title": map[string]any{"agent_id": "x"}}},
		{"private agent", "agent_not_eligible", map[string]any{"dm_title": map[string]any{"agent_id": private}}},
		{"foreign agent", "agent_not_eligible", map[string]any{"dm_title": map[string]any{"agent_id": otherAgent}}},
		{"private fallback", "agent_not_eligible", map[string]any{"dm_title": map[string]any{"agent_id": a, "fallback_agent_id": private}}},
		{"not an object", "invalid_shape", []string{"x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantCode(t, putBG(t, wsID, c.body, nil), http.StatusBadRequest, c.code) })
	}
	if storedBG(t, wsID) != nil || bgRows(t, wsID) != 0 {
		t.Fatal("a refused body wrote something")
	}
}

func TestBackgroundAgentsOwnerAdminWriteMemberRead(t *testing.T) {
	wsID, a, _, _ := bgFixture(t, "bg-role")
	dbfx.Exec(t, `UPDATE member SET role='member' WHERE workspace_id=$1`, wsID)
	putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": a}}, nil).Want(http.StatusForbidden)
	bgCall(t, testHandler.ResetBackgroundAgents, "DELETE", wsID, nil, nil).Want(http.StatusForbidden)
	bgCall(t, testHandler.GetBackgroundAgents, "GET", wsID, nil, nil).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE member SET role='admin' WHERE workspace_id=$1`, wsID)
	putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
}

// An agent archived after the last human save must not turn an unchanged
// machine write into a 400; a changed machine write is still refused.
func TestBackgroundAgentsUnchangedMachineWriteSkipsEligibility(t *testing.T) {
	wsID, a, b, _ := bgFixture(t, "bg-archived")
	val := map[string]any{"dm_title": map[string]any{"agent_id": a}}
	putBG(t, wsID, val, nil).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, a)
	rows, events := bgRows(t, wsID), len(bgEventsFor(wsID))
	m := machine("task_token")
	putBG(t, wsID, val, m).Want(http.StatusOK)
	wantCode(t, putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": b}}, m), http.StatusForbidden, "background_agents_human_only")
	if bgRows(t, wsID) != rows || len(bgEventsFor(wsID)) != events {
		t.Fatal("machine write left a row or an event")
	}
	// A human still gets the eligibility check.
	wantCode(t, putBG(t, wsID, val, nil), http.StatusBadRequest, "agent_not_eligible")
}
