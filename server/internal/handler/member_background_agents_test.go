package handler

import (
	"net/http"
	"reflect"
	"testing"

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
