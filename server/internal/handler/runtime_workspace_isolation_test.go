package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// runtimeIsolationFixture is a second workspace B next to the suite's
// workspace A (testWorkspaceID): its own admin, runtime, agent, issue and
// queued task. Workspace A gets the same shape through the suite fixtures.
type runtimeIsolationFixture struct {
	workspaceB, userB, runtimeB, agentB, issueB string
	runtimeA                                    string
}

func newRuntimeIsolationFixture(t *testing.T) runtimeIsolationFixture {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	f := runtimeIsolationFixture{}
	f.workspaceB = dbfx.Workspace(t, "Isolation B", "che3415-b-"+suffix)
	f.userB = dbfx.User(t, "Isolation B admin", "che3415-b-"+suffix+"@example.com")
	dbfx.Member(t, f.workspaceB, f.userB, "admin")
	// Both runtimes sit on the same daemon and owner, so the workspace check is
	// the only thing that can keep workspace A's daemon off runtime B.
	f.runtimeA = dbfx.Runtime(t, "isolation runtime A "+suffix, testutil.Cols{"daemon_id": batchClaimTestDaemonID})
	f.runtimeB = dbfx.Runtime(t, "isolation runtime B "+suffix, testutil.Cols{"workspace_id": f.workspaceB, "daemon_id": batchClaimTestDaemonID})
	f.agentB = dbfx.Agent(t, "isolation agent B "+suffix, f.runtimeB, testutil.Cols{"workspace_id": f.workspaceB, "owner_id": f.userB})
	f.issueB = dbfx.Issue(t, "isolation issue B", testutil.Cols{"workspace_id": f.workspaceB, "creator_id": f.userB})
	return f
}

func asWorkspaceUser(req *http.Request, userID, workspaceID string) *http.Request {
	req.Header.Set("X-User-ID", userID)
	req.Header.Set("X-Workspace-ID", workspaceID)
	return req
}

func TestRuntimeWorkspaceIsolation_TaskClaim(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	claimOverHTTP := func(newReq func(body map[string]any) *http.Request) func(*testing.T, []string) []string {
		return func(t *testing.T, runtimeIDs []string) []string {
			t.Helper()
			resp := testutil.Decode[batchClaimResponse](t, testHandler.ClaimTasksByRuntime,
				newReq(map[string]any{"daemon_id": batchClaimTestDaemonID, "runtime_ids": runtimeIDs, "max_tasks": 10}), http.StatusOK)
			ids := make([]string, 0, len(resp.Tasks))
			for _, task := range resp.Tasks {
				ids = append(ids, task.ID)
			}
			return ids
		}
	}
	claimOverWS := func(t *testing.T, runtimeIDs []string) []string {
		t.Helper()
		identity := daemonws.ClientIdentity{
			DaemonID:           batchClaimTestDaemonID,
			DaemonTokenOwnerID: testUserID,
			WorkspaceID:        testWorkspaceID,
			WorkspaceIDs:       []string{testWorkspaceID},
			RuntimeIDs:         runtimeIDs,
		}
		body, _ := json.Marshal(map[string]any{"daemon_id": batchClaimTestDaemonID, "runtime_ids": runtimeIDs, "max_tasks": 10})
		status, respBody, err := testHandler.DaemonRPCHandler(context.Background(), identity, "tasks.claim", body)
		if err != nil || status != http.StatusOK {
			t.Fatalf("rpc claim: status=%d err=%v body=%s", status, err, respBody)
		}
		var resp batchClaimResponse
		if err := json.Unmarshal(respBody, &resp); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(resp.Tasks))
		for _, task := range resp.Tasks {
			ids = append(ids, task.ID)
		}
		return ids
	}

	for name, claim := range map[string]func(*testing.T, []string) []string{
		"http-user": claimOverHTTP(func(b map[string]any) *http.Request {
			return newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/claim", b)
		}),
		"http-daemon-token": claimOverHTTP(func(b map[string]any) *http.Request {
			return newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/claim", b, testWorkspaceID, batchClaimTestDaemonID)
		}),
		"ws": claimOverWS,
	} {
		t.Run(name, func(t *testing.T) {
			f := newRuntimeIsolationFixture(t)
			agentA := dbfx.Agent(t, "isolation agent A "+name, f.runtimeA)
			issueA := dbfx.Issue(t, "isolation issue A")
			taskA := dbfx.Task(t, agentA, testutil.Cols{"runtime_id": f.runtimeA, "issue_id": issueA})
			// B's own task on B's runtime, and a B task misrouted onto A's runtime.
			// Both agents are owned by the runtime owner, so the private-runtime
			// ownership gate passes and only the workspace check can refuse them.
			agentBOnB := dbfx.Agent(t, "isolation agent B on B "+name, f.runtimeB, testutil.Cols{"workspace_id": f.workspaceB})
			taskB := dbfx.Task(t, agentBOnB, testutil.Cols{"runtime_id": f.runtimeB, "issue_id": f.issueB})
			agentBOnA := dbfx.Agent(t, "isolation agent B on A "+name, f.runtimeA, testutil.Cols{"workspace_id": f.workspaceB})
			taskBOnA := dbfx.Task(t, agentBOnA, testutil.Cols{"runtime_id": f.runtimeA, "issue_id": f.issueB})

			// A's daemon also asks for B's runtime id: it must be ignored.
			got := claim(t, []string{f.runtimeA, f.runtimeB})

			if !slices.Contains(got, taskA) {
				t.Fatalf("positive control: runtime A did not claim workspace A task %s; got %v", taskA, got)
			}
			for _, foreign := range []string{taskB, taskBOnA} {
				if slices.Contains(got, foreign) {
					t.Errorf("workspace A runtime claimed workspace B task %s; got %v", foreign, got)
				}
			}
		})
	}
}

func TestRuntimeWorkspaceIsolation_AgentRuntimeBinding(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	t.Run("create", func(t *testing.T) {
		f := newRuntimeIsolationFixture(t)
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		// CreateAgent inserts outside dbfx; drop its agent before the runtime.
		dbfx.Cleanup(t, `DELETE FROM agent WHERE runtime_id = $1`, f.runtimeA)

		testutil.Call(t, testHandler.CreateAgent, newRequest(http.MethodPost, "/api/agents", map[string]any{
			"name": "isolation create A " + suffix, "runtime_id": f.runtimeA,
		})).Want(http.StatusCreated)

		testutil.Call(t, testHandler.CreateAgent, asWorkspaceUser(newRequest(http.MethodPost, "/api/agents", map[string]any{
			"name": "isolation create B " + suffix, "runtime_id": f.runtimeA,
		}), f.userB, f.workspaceB)).Want(http.StatusBadRequest)
		if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE workspace_id = $1 AND runtime_id = $2`, f.workspaceB, f.runtimeA); n != 0 {
			t.Fatalf("workspace B holds %d agents bound to workspace A runtime", n)
		}
	})

	t.Run("update", func(t *testing.T) {
		f := newRuntimeIsolationFixture(t)
		agentA := dbfx.Agent(t, "isolation update A", f.runtimeA)
		otherRuntimeA := dbfx.Runtime(t, "isolation runtime A2")

		testutil.Call(t, testHandler.UpdateAgent, withURLParam(newRequest(http.MethodPut, "/api/agents/"+agentA, map[string]any{
			"runtime_id": otherRuntimeA,
		}), "id", agentA)).Want(http.StatusOK)

		testutil.Call(t, testHandler.UpdateAgent, withURLParam(asWorkspaceUser(newRequest(http.MethodPut, "/api/agents/"+f.agentB, map[string]any{
			"runtime_id": f.runtimeA,
		}), f.userB, f.workspaceB), "id", f.agentB)).Want(http.StatusBadRequest)
		var bound string
		dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, f.agentB).Scan(&bound)
		if bound != f.runtimeB {
			t.Fatalf("workspace B agent runtime = %s, want unchanged %s", bound, f.runtimeB)
		}
	})
}

func TestRuntimeWorkspaceIsolation_RuntimeList(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	f := newRuntimeIsolationFixture(t)

	list := func(req *http.Request) []string {
		t.Helper()
		rows := testutil.Decode[[]struct {
			ID string `json:"id"`
		}](t, testHandler.ListAgentRuntimes, req, http.StatusOK)
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		return ids
	}

	asA := list(newRequest(http.MethodGet, "/api/runtimes", nil))
	if !slices.Contains(asA, f.runtimeA) {
		t.Fatalf("positive control: workspace A admin does not see runtime A; got %v", asA)
	}
	if slices.Contains(asA, f.runtimeB) {
		t.Errorf("workspace A admin sees workspace B runtime %s", f.runtimeB)
	}

	asB := list(asWorkspaceUser(newRequest(http.MethodGet, "/api/runtimes", nil), f.userB, f.workspaceB))
	if !slices.Contains(asB, f.runtimeB) {
		t.Fatalf("positive control: workspace B admin does not see runtime B; got %v", asB)
	}
	if slices.Contains(asB, f.runtimeA) {
		t.Errorf("workspace B admin sees workspace A runtime %s", f.runtimeA)
	}
}
