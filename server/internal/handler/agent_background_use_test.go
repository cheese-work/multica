package handler

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type bgUse struct {
	Scope string `json:"scope"`
	Job   string `json:"job"`
}

// shareWithWorkspace makes the agent viewable by every member (public_to).
func (f memberBG) shareWithWorkspace(t *testing.T, agentIDs ...string) {
	t.Helper()
	for _, id := range agentIDs {
		dbfx.Exec(t, `UPDATE agent SET permission_mode='public_to' WHERE id=$1`, id)
		dbfx.Insert(t, "agent_invocation_target", testutil.Cols{"agent_id": id, "target_type": "workspace", "target_id": f.ws})
	}
}

func (f memberBG) getAgent(t *testing.T, user, agentID string) []bgUse {
	t.Helper()
	req := newRequestAs(user, "GET", "/api/agents/"+agentID, nil)
	req.Header.Set("X-Workspace-ID", f.ws)
	req = withURLParam(req, "id", agentID)
	var out struct {
		BackgroundUse []bgUse `json:"background_use"`
	}
	testutil.Call(t, testHandler.GetAgent, req).Want(http.StatusOK).JSON(&out)
	return out.BackgroundUse
}

// CHE-5439: member B reading agent X gets the workspace uses and B's own
// personal uses, never member A's personal pick.
func TestGetAgentBackgroundUseNeverLeaksAnotherMembersPersonalPick(t *testing.T) {
	f := memberBGFixture(t, "bguse-leak")
	f.shareWithWorkspace(t, f.a1, f.a2)
	f.setWorkspace(t, map[string]any{
		"dm_title":      map[string]any{"agent_id": f.a1},
		"quick_actions": map[string]any{"agent_id": f.a2, "fallback_agent_id": f.a1},
	})
	f.setUser(t, testUserID, map[string]any{"compact": map[string]any{"agent_id": f.a1}}) // A's personal pick
	f.setUser(t, f.memberB, map[string]any{"quick_actions": map[string]any{"agent_id": f.a1}})

	gotB := f.getAgent(t, f.memberB, f.a1)
	wantB := []bgUse{{"workspace", "dm_title"}, {"workspace", "quick_actions"}, {"personal", "quick_actions"}}
	if !reflect.DeepEqual(gotB, wantB) {
		t.Fatalf("B read %v, want %v (A's compact pick must not appear)", gotB, wantB)
	}
	gotA := f.getAgent(t, testUserID, f.a1)
	wantA := []bgUse{{"workspace", "dm_title"}, {"workspace", "quick_actions"}, {"personal", "compact"}}
	if !reflect.DeepEqual(gotA, wantA) {
		t.Fatalf("A read %v, want %v", gotA, wantA)
	}
}

func TestGetAgentBackgroundUseEmptyWhenAgentServesNoJob(t *testing.T) {
	f := memberBGFixture(t, "bguse-empty")
	f.shareWithWorkspace(t, f.a2)
	f.setWorkspace(t, map[string]any{"dm_title": map[string]any{"agent_id": f.a1}})
	if got := f.getAgent(t, f.memberB, f.a2); len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}
