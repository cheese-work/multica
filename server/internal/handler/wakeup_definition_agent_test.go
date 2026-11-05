package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A task-token run owned by a plain workspace member may propose a workspace or
// project rule. It is stored disabled; the member's own role never gates the
// agent create path, and the run still cannot edit, delete or preview.
func TestWakeupDefinitionMemberOwnedRunCreatesDisabledRule(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	member := wdMember(t, "WD Agent Owner", "member")
	agentID := dbfx.Agent(t, "wd member agent", handlerTestRuntimeID(t))
	taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": handlerTestRuntimeID(t), "originator_user_id": member, "accountable_user_id": member})
	run := func(r *http.Request) *http.Request {
		r.Header.Set("X-User-ID", member)
		r.Header.Set("X-Actor-Source", "task_token")
		return asRun(r, agentID, taskID)
	}
	for _, s := range []wdScope{k.workspace(), k.project()} {
		wipeWakeupDefinitions(t)
		body := cfg(map[string]any{"enabled": true, "trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x", "target": map[string]any{"type": "agent", "id": handlerTestAgentID(t)}})
		created := decodeDefinition(t, s.create(body, run).Want(http.StatusCreated))
		if enabled := string(configKeys(t, created.Config)["enabled"]); enabled != "false" {
			t.Fatalf("%s: member-owned rule enabled = %s, want false", s.kind, enabled)
		}
		s.put(created.RuleKey, created.Revision, body, run).Want(http.StatusForbidden)
		s.preview("", body, run).Want(http.StatusForbidden)
		// The member alone, without a run, is still refused.
		s.create(body, asUser(member)).Want(http.StatusForbidden)
	}
}

// The same create through the real auth middleware with a real task token: the
// token row, not a stamped header, makes the caller an agent run.
func TestWakeupDefinitionRealTaskTokenCreatesDisabledRule(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	member := wdMember(t, "WD Token Owner", "member")
	agentID := dbfx.Agent(t, "wd token run agent", handlerTestRuntimeID(t))
	taskID := dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": handlerTestRuntimeID(t), "originator_user_id": member, "accountable_user_id": member})
	raw := "mat_" + uuid.NewString()
	if _, err := testHandler.Queries.CreateTaskToken(context.Background(), db.CreateTaskTokenParams{
		TokenHash: auth.HashToken(raw), TaskID: parseUUID(taskID), AgentID: parseUUID(agentID),
		WorkspaceID: parseUUID(testWorkspaceID), UserID: parseUUID(member),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Use(middleware.Auth(testHandler.Queries, nil, nil, nil))
	ws, pr := k.workspace().api(), k.project().api()
	router.Post("/api/wakeup-definitions", ws.Create)
	router.Put("/api/wakeup-definitions/{rule}", ws.Put)
	router.Post("/api/projects/{id}/wakeup-definitions", pr.Create)
	call := func(method, path string, body any) *testutil.Response {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+raw)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		return testutil.Call(t, router.ServeHTTP, req)
	}
	body := wdBody(0, cfg(map[string]any{"enabled": true, "trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x", "target": map[string]any{"type": "agent", "id": handlerTestAgentID(t)}}))
	for _, path := range []string{"/api/wakeup-definitions", "/api/projects/" + k.projectID + "/wakeup-definitions"} {
		wipeWakeupDefinitions(t)
		created := decodeDefinition(t, call("POST", path, body).Want(http.StatusCreated))
		if enabled := string(configKeys(t, created.Config)["enabled"]); enabled != "false" {
			t.Fatalf("%s: token-created rule enabled = %s, want false", path, enabled)
		}
	}
	wipeWakeupDefinitions(t)
	created := decodeDefinition(t, call("POST", "/api/wakeup-definitions", body).Want(http.StatusCreated))
	call("PUT", "/api/wakeup-definitions/"+created.RuleKey, wdBody(created.Revision, cfg(map[string]any{"enabled": true}))).Want(http.StatusForbidden)
}

// An agent-created root waits for a human admin. A member's issue override must
// not switch it on in the meantime; the admin's own write lifts the veto, and a
// root a human disabled keeps the documented re-enable-below behaviour.
func TestWakeupDefinitionAgentRootVetoesIssueOverride(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	member := wdMember(t, "WD Override Member", "member")
	body := cfg(map[string]any{"enabled": true, "trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x", "target": map[string]any{"type": "agent", "id": handlerTestAgentID(t)}})
	override := cfg(map[string]any{"enabled": true})
	agentRun := k.asAgentRun()
	for _, s := range []wdScope{k.workspace(), k.project()} {
		wipeWakeupDefinitions(t)
		root := decodeDefinition(t, s.create(body, agentRun).Want(http.StatusCreated))
		k.issue().put(root.RuleKey, 0, override, asUser(member)).Want(http.StatusOK)
		if eff := k.issue().effective(root.RuleKey); eff.Enabled {
			t.Fatalf("%s: a member's issue override enabled an unapproved agent rule: %+v", s.kind, eff)
		}
		// The human admin enables the root; the override below it now counts.
		s.put(root.RuleKey, root.Revision, body).Want(http.StatusOK)
		if eff := k.issue().effective(root.RuleKey); !eff.Enabled {
			t.Fatalf("%s: admin-approved rule stayed disabled: %+v", s.kind, eff)
		}
	}
	// A human-created disabled root is not an agent proposal: unchanged.
	wipeWakeupDefinitions(t)
	off := cfg(map[string]any{"enabled": false, "trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x", "target": map[string]any{"type": "agent", "id": handlerTestAgentID(t)}})
	human := decodeDefinition(t, k.workspace().create(off).Want(http.StatusCreated))
	k.issue().put(human.RuleKey, 0, override, asUser(member)).Want(http.StatusOK)
	if eff := k.issue().effective(human.RuleKey); !eff.Enabled {
		t.Fatalf("human-disabled root no longer re-enabled by an issue override: %+v", eff)
	}
}

// A pending root still has a target. A member who may not use that private
// target must not save an instruction below it: once an admin approves the
// root, the text would run on an agent the member cannot reach.
func TestWakeupDefinitionPendingRootStillAuthorizesInheritedTarget(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	member := wdMember(t, "WD Blocked Member", "member")
	private := dbfx.Agent(t, "wd pending private agent", handlerTestRuntimeID(t), testutil.Cols{"owner_id": testUserID})
	body := cfg(map[string]any{"enabled": true, "trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x", "target": map[string]any{"type": "agent", "id": private}})
	agentRun := k.asAgentRun()
	for _, s := range []wdScope{k.workspace(), k.project()} {
		wipeWakeupDefinitions(t)
		root := decodeDefinition(t, s.create(body, agentRun).Want(http.StatusCreated))
		k.issue().put(root.RuleKey, 0, cfg(map[string]any{"instruction": "run my text"}), asUser(member)).Want(http.StatusForbidden)
		k.issue().preview(root.RuleKey, cfg(map[string]any{"instruction": "run my text"}), asUser(member)).Want(http.StatusForbidden)
		// After the admin approves, the refused text is not what runs.
		s.put(root.RuleKey, root.Revision, body).Want(http.StatusOK)
		if eff := k.issue().effective(root.RuleKey); !eff.Enabled || strings.Contains(string(eff.Config), "run my text") {
			t.Fatalf("%s: refused instruction leaked into the approved rule: %+v", s.kind, eff)
		}
		// A member who may use the target is unaffected.
		k.issue().put(root.RuleKey, 0, cfg(map[string]any{"instruction": "owner text"})).Want(http.StatusOK)
	}
}
