package handler

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// memberBG is a workspace owned by testUserID (member A) with a second
// member B. Agents a1/a2 are workspace-visible and owned by A, so B cannot
// invoke them; b1 is owned by B; priv is A's private agent.
type memberBG struct {
	ws, memberB, a1, a2, b1, priv string
}

func memberBGFixture(t *testing.T, slug string) memberBG {
	t.Helper()
	f := memberBG{ws: pauseWorkspaceFixture(t, slug)}
	f.memberB = dbfx.User(t, slug+"-b", slug+"-b@example.com")
	dbfx.Member(t, f.ws, f.memberB, "member")
	rt := dbfx.Runtime(t, slug, testutil.Cols{"workspace_id": f.ws})
	agent := func(name, vis, owner string) string {
		return dbfx.Agent(t, slug+"-"+name, rt, testutil.Cols{"workspace_id": f.ws, "visibility": vis, "owner_id": owner})
	}
	f.a1, f.a2 = agent("a1", "workspace", testUserID), agent("a2", "workspace", testUserID)
	f.b1, f.priv = agent("b1", "workspace", f.memberB), agent("priv", "private", testUserID)
	return f
}

func (f memberBG) setUser(t *testing.T, user string, value map[string]any) {
	t.Helper()
	raw := mustJSON(t, value)
	dbfx.Exec(t, `INSERT INTO member_setting (workspace_id, user_id, key, value) VALUES ($1,$2,'background_agents',$3)`, f.ws, user, raw)
}

func (f memberBG) setWorkspace(t *testing.T, value map[string]any) {
	t.Helper()
	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_build_object('background_agents', $2::jsonb) WHERE id=$1`, f.ws, mustJSON(t, value))
}

func (f memberBG) get(t *testing.T, user, query string) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequestAs(user, "GET", "/api/workspaces/"+f.ws+"/me/background-agents"+query, nil), "id", f.ws)
	return testutil.Call(t, testHandler.GetMyBackgroundAgents, req)
}

func (f memberBG) resolve(t *testing.T, user string, job bgagents.Job) bgagents.Resolution {
	t.Helper()
	got, err := testHandler.ResolveBackgroundAgents(t.Context(), util.MustParseUUID(f.ws), util.MustParseUUID(user), job)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func chainIDs(r bgagents.Resolution) []string {
	ids := []string{}
	for _, c := range r.Chain {
		ids = append(ids, c.AgentID)
	}
	return ids
}

func TestMemberBackgroundAgentsReadIsScopedToCaller(t *testing.T) {
	f := memberBGFixture(t, "mbg-read")
	f.setUser(t, testUserID, map[string]any{"dm_title": map[string]any{"agent_id": f.a1}})

	var mine, theirs map[string]any
	f.get(t, testUserID, "").Want(http.StatusOK).JSON(&mine)
	f.get(t, f.memberB, "").Want(http.StatusOK).JSON(&theirs)
	if !reflect.DeepEqual(mine, map[string]any{"dm_title": map[string]any{"agent_id": f.a1}}) {
		t.Fatalf("A read %v, want own row", mine)
	}
	if len(theirs) != 0 {
		t.Fatalf("B read A's row: %v", theirs)
	}
}

func TestMemberBackgroundAgentsReadRejectsOutsiderAndBadJob(t *testing.T) {
	f := memberBGFixture(t, "mbg-400")
	outsider := dbfx.User(t, "mbg-out", "mbg-out@example.com")
	f.get(t, outsider, "").Want(http.StatusNotFound)
	wantCode(t, f.get(t, testUserID, "?job=routing"), http.StatusBadRequest, bgagents.CodeRoutingUserScope)
	wantCode(t, f.get(t, testUserID, "?job=nope"), http.StatusBadRequest, bgagents.CodeUnknownJob)
	f.get(t, testUserID, "?job=compact").Want(http.StatusOK)
}

func TestResolveSkipsUserChainAgentTheUserCannotInvoke(t *testing.T) {
	f := memberBGFixture(t, "mbg-invoke")
	// B's chain: primary is A's agent (B cannot invoke it), fallback is B's own.
	f.setUser(t, f.memberB, map[string]any{"dm_title": map[string]any{"agent_id": f.a1, "fallback_agent_id": f.b1}})
	got := f.resolve(t, f.memberB, bgagents.DMTitle)
	if got.Source != bgagents.UserSource || !reflect.DeepEqual(chainIDs(got), []string{f.b1}) {
		t.Fatalf("B resolved %+v, want user chain [b1] only", got)
	}
	// A's identical chain keeps the agent A owns.
	f.setUser(t, testUserID, map[string]any{"dm_title": map[string]any{"agent_id": f.a1, "fallback_agent_id": f.b1}})
	if got := f.resolve(t, testUserID, bgagents.DMTitle); !reflect.DeepEqual(chainIDs(got), []string{f.a1}) {
		t.Fatalf("A resolved %v, want [a1] (b1 is B's private-mode agent)", chainIDs(got))
	}
}

func TestResolveUserChainThatFailsEntirelyDoesNotFallThrough(t *testing.T) {
	f := memberBGFixture(t, "mbg-nofall")
	f.setWorkspace(t, map[string]any{"dm_title": map[string]any{"agent_id": f.a2}})
	f.setUser(t, f.memberB, map[string]any{"dm_title": map[string]any{"agent_id": f.a1}})
	got := f.resolve(t, f.memberB, bgagents.DMTitle)
	if got.Source != bgagents.UserSource || len(got.Chain) != 0 {
		t.Fatalf("resolved %+v, want empty user chain (no fall-through)", got)
	}
}

func TestResolveAllowsPrivateAgentTheUserOwnsAndFallsToWorkspace(t *testing.T) {
	f := memberBGFixture(t, "mbg-priv")
	f.setWorkspace(t, map[string]any{"compact": map[string]any{"agent_id": f.a2}})
	// No user row: workspace chain.
	if got := f.resolve(t, testUserID, bgagents.Compact); got.Source != bgagents.WorkspaceSource || !reflect.DeepEqual(chainIDs(got), []string{f.a2}) {
		t.Fatalf("no user row: %+v", got)
	}
	// A's own private agent is allowed in A's user chain, not in the workspace's.
	f.setUser(t, testUserID, map[string]any{"compact": map[string]any{"agent_id": f.priv}})
	if got := f.resolve(t, testUserID, bgagents.Compact); got.Source != bgagents.UserSource || !reflect.DeepEqual(chainIDs(got), []string{f.priv}) {
		t.Fatalf("own private agent: %+v", got)
	}
}

func TestResolveIgnoresRoutingInUserRow(t *testing.T) {
	f := memberBGFixture(t, "mbg-route")
	f.setWorkspace(t, map[string]any{"routing": map[string]any{"agent_id": f.a2}})
	f.setUser(t, testUserID, map[string]any{"routing": map[string]any{"agent_id": f.a1}})
	if got := f.resolve(t, testUserID, bgagents.Routing); !reflect.DeepEqual(chainIDs(got), []string{f.a2}) || got.Source != bgagents.WorkspaceSource {
		t.Fatalf("routing resolved %+v, want the workspace chain", got)
	}
}

// --- write paths (CHE-4113) ---

func (f memberBG) put(t *testing.T, user string, body any, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequestAs(user, "PUT", "/api/workspaces/"+f.ws+"/me/background-agents", body), "id", f.ws)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, testHandler.UpdateMyBackgroundAgents, req)
}

func (f memberBG) reset(t *testing.T, user, job string, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParams(newRequestAs(user, "DELETE", "/api/workspaces/"+f.ws+"/me/background-agents/"+job, nil), "id", f.ws, "job", job)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, testHandler.ResetMyBackgroundAgent, req)
}

func (f memberBG) stored(t *testing.T, user string) any {
	t.Helper()
	var v any
	err := dbfx.Pool.QueryRow(t.Context(), `SELECT value FROM member_setting WHERE workspace_id=$1 AND user_id=$2 AND key='background_agents'`, f.ws, user).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f memberBG) activity(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := dbfx.Pool.Query(t.Context(), `SELECT details FROM activity_log WHERE workspace_id=$1 AND action='background_agent_changed' AND details->>'scope'='user' ORDER BY created_at, id`, f.ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var d map[string]any
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestPersonalWriteSetClearResetAndActivity(t *testing.T) {
	f := memberBGFixture(t, "pbg-on")
	var out map[string]any
	f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"agent_id": f.a1, "fallback_agent_id": f.priv}}, nil).Want(http.StatusOK).JSON(&out)
	want := map[string]any{"dm_title": map[string]any{"agent_id": f.a1, "fallback_agent_id": f.priv}}
	if !reflect.DeepEqual(out, want) || !reflect.DeepEqual(f.stored(t, testUserID), any(want)) {
		t.Fatalf("response %v / stored %v, want %v", out, f.stored(t, testUserID), want)
	}
	// null clears one field and leaves the other.
	f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"fallback_agent_id": nil}}, nil).Want(http.StatusOK)
	if got, want := f.stored(t, testUserID), any(map[string]any{"dm_title": map[string]any{"agent_id": f.a1}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("after clearing fallback: %v, want %v", got, want)
	}
	// Another job is independent.
	f.put(t, testUserID, map[string]any{"quick_actions": map[string]any{"agent_id": f.a2}}, nil).Want(http.StatusOK)
	// Reset drops one job only; resetting an unset job is a no-op.
	f.reset(t, testUserID, "dm_title", nil).Want(http.StatusOK)
	f.reset(t, testUserID, "dm_title", nil).Want(http.StatusOK)
	if got, want := f.stored(t, testUserID), any(map[string]any{"quick_actions": map[string]any{"agent_id": f.a2}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("after reset: %v, want %v", got, want)
	}
	// Last job gone: no row left.
	f.reset(t, testUserID, "quick_actions", nil).Want(http.StatusOK)
	if f.stored(t, testUserID) != nil {
		t.Fatalf("row remains: %v", f.stored(t, testUserID))
	}

	// Writes land on the caller's row only.
	if f.stored(t, f.memberB) != nil {
		t.Fatal("member B has a row")
	}
	// Activity: set, clear fallback, set quick_actions, 2 resets = 5, all scope user, with old and new chains.
	acts := f.activity(t)
	if len(acts) != 5 {
		t.Fatalf("activity rows = %d, want 5: %v", len(acts), acts)
	}
	first := acts[0]
	if first["job"] != "dm_title" || first["scope"] != "user" || first["from"] != nil ||
		!reflect.DeepEqual(first["to"], map[string]any{"agent_id": f.a1, "fallback_agent_id": f.priv}) {
		t.Fatalf("first activity = %v", first)
	}
	var actor string
	dbfx.QueryRow(t, `SELECT actor_id::text FROM activity_log WHERE workspace_id=$1 AND action='background_agent_changed' AND details->>'scope'='user' LIMIT 1`, f.ws).Scan(&actor)
	if actor != testUserID {
		t.Fatalf("actor = %s, want %s", actor, testUserID)
	}
}

func TestPersonalWriteMachineActorRefusedUnchangedAccepted(t *testing.T) {
	f := memberBGFixture(t, "pbg-machine")
	for _, source := range []string{"task_token", "cloud_pat"} {
		wantCode(t, f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"agent_id": f.a1}}, machine(source)), http.StatusForbidden, codeBackgroundAgentsHumanOnly)
	}
	f.setUser(t, testUserID, map[string]any{"dm_title": map[string]any{"agent_id": f.a1, "fallback_agent_id": f.a2}})
	m := machine("task_token")
	wantCode(t, f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"agent_id": f.a2}}, m), http.StatusForbidden, codeBackgroundAgentsHumanOnly)
	wantCode(t, f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"fallback_agent_id": nil}}, m), http.StatusForbidden, codeBackgroundAgentsHumanOnly)
	wantCode(t, f.reset(t, testUserID, "dm_title", m), http.StatusForbidden, codeBackgroundAgentsHumanOnly)
	// Unchanged: same value, an empty body, and a reset of an unset job.
	f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"agent_id": f.a1}}, m).Want(http.StatusOK)
	f.put(t, testUserID, map[string]any{}, m).Want(http.StatusOK)
	f.reset(t, testUserID, "quick_actions", m).Want(http.StatusOK)

	if got, want := f.stored(t, testUserID), any(map[string]any{"dm_title": map[string]any{"agent_id": f.a1, "fallback_agent_id": f.a2}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("machine writes changed the row: %v", got)
	}
	if got := f.activity(t); len(got) != 0 {
		t.Fatalf("activity = %v, want none", got)
	}
}

func TestPersonalWriteValidation(t *testing.T) {
	f := memberBGFixture(t, "pbg-bad")
	ok := map[string]any{"agent_id": f.a1}
	wantCode(t, f.put(t, testUserID, map[string]any{"routing": ok}, nil), http.StatusBadRequest, bgagents.CodeRoutingUserScope)
	wantCode(t, f.put(t, testUserID, map[string]any{"nope": ok}, nil), http.StatusBadRequest, bgagents.CodeUnknownJob)
	wantCode(t, f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"statuses": []string{"x"}}}, nil), http.StatusBadRequest, bgagents.CodeUnknownField)
	wantCode(t, f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"agent_id": "not-a-uuid"}}, nil), http.StatusBadRequest, bgagents.CodeInvalidAgentID)
	wantCode(t, f.put(t, testUserID, map[string]any{"dm_title": nil}, nil), http.StatusBadRequest, bgagents.CodeInvalidShape)
	wantCode(t, f.put(t, testUserID, []string{"x"}, nil), http.StatusBadRequest, bgagents.CodeInvalidShape)
	// B cannot invoke A's agent; an unknown agent id is not in the workspace.
	wantCode(t, f.put(t, f.memberB, map[string]any{"dm_title": ok}, nil), http.StatusBadRequest, codeAgentNotEligible)
	wantCode(t, f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"agent_id": "00000000-0000-0000-0000-000000000001"}}, nil), http.StatusBadRequest, codeAgentNotEligible)
	// A's own private agent is fine for A.
	f.put(t, testUserID, map[string]any{"dm_title": map[string]any{"agent_id": f.priv}}, nil).Want(http.StatusOK)
	// Reset: routing and unknown jobs are 400.
	wantCode(t, f.reset(t, testUserID, "routing", nil), http.StatusBadRequest, bgagents.CodeRoutingUserScope)
	wantCode(t, f.reset(t, testUserID, "nope", nil), http.StatusBadRequest, bgagents.CodeUnknownJob)
	// A non-member is refused.
	outsider := dbfx.User(t, "pbg-out", "pbg-out@example.com")
	f.put(t, outsider, map[string]any{"dm_title": ok}, nil).Want(http.StatusNotFound)
	// A bad request left nothing behind except the one good write.
	if got := f.activity(t); len(got) != 1 {
		t.Fatalf("activity rows = %d, want 1", len(got))
	}
}
