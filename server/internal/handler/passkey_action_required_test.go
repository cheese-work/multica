package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-3482 (CHE-3348 step 9): an agent identity never writes a covered field
// through an older route. Every such write is refused 403 with the code
// passkey_action_required and the action type that owns the change; the stored
// value does not move. project.description and the human owner CAS stay as they
// were.

func wantPasskeyRequired(t *testing.T, w *httptest.ResponseRecorder, actionType string) {
	t.Helper()
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if body["code"] != "passkey_action_required" || body["action_type"] != actionType {
		t.Fatalf("want passkey_action_required + %s, got %v", actionType, body)
	}
}

var coveredActionTypes = map[string]string{
	"workspace.context":  "workspace.context.set",
	"squad.instructions": "squad.instructions.set",
	"agent.instructions": "agent.instructions.set",
}

// An agent that holds no receipt of any kind is refused the same way, and so is
// a cloud-node PAT.
func TestPasskeyRequired_PlainAgentWriteRefused(t *testing.T) {
	eachOwnerCASTarget(t, func(t *testing.T, tg ownerCASTarget) {
		f := newVoteFixture(t)
		id := tg.seed(t, "initial")
		for name, decorate := range map[string]func(*http.Request) *http.Request{
			"agent":      func(r *http.Request) *http.Request { return asAgentActor(r, f.writerID, f.taskID) },
			"cloud node": asCloudNodeActor,
		} {
			w := httptest.NewRecorder()
			tg.handle(w, decorate(tg.req(testUserID, id, map[string]any{tg.field: "agent text"})))
			t.Run(name, func(t *testing.T) { wantPasskeyRequired(t, w, coveredActionTypes[tg.name]) })
			if got := tg.read(t, id); got != "initial" {
				t.Fatalf("value must stay initial, got %q", got)
			}
		}
	})
}

// Regression: the agent is on the delegated-writes allowlist, the owner
// answered the approval card, and no passkey was used.
func TestPasskeyRequired_OwnerCardAnswerWithoutPasskeyRefused(t *testing.T) {
	initial, next := "initial", "next"
	eachDecisionCaseOf(t, voteTargets(), initial, next, func(t *testing.T, tg ownerCASTarget, f decisionWriteFixture, id string) {
		w := f.write(tg, id, next, testDigest(initial))
		if tg.name == "project.description" { // out of scope: unchanged
			if w.Code != http.StatusOK || tg.read(t, id) != next {
				t.Fatalf("project.description must still write: %d %s", w.Code, w.Body.String())
			}
			return
		}
		wantPasskeyRequired(t, w, coveredActionTypes[tg.name])
		if got := tg.read(t, id); got != initial {
			t.Fatalf("value must stay %q, got %q", initial, got)
		}
		if f.used(t) != 0 {
			t.Fatal("the refused write must not spend the decision")
		}
	})
}

// Regression: a valid vote receipt.
func TestPasskeyRequired_ValidVoteReceiptRefused(t *testing.T) {
	initial, next := voteAgent("before")+"\n"+voteHuman+"\n", voteAgent("after")+"\n"+voteHuman+"\n"
	eachVoteCaseOf(t, voteTargets(), initial, next, func(t *testing.T, tg ownerCASTarget, f voteFixture, id, label string) {
		f.approve(t, label, testDigest(next))
		w := f.write(tg, id, next, testDigest(initial))
		if tg.name == "project.description" { // out of scope: unchanged
			if w.Code != http.StatusOK || tg.read(t, id) != next {
				t.Fatalf("project.description must still write: %d %s", w.Code, w.Body.String())
			}
			return
		}
		wantPasskeyRequired(t, w, coveredActionTypes[tg.name])
		if got := tg.read(t, id); got != initial {
			t.Fatalf("value must stay %q, got %q", initial, got)
		}
	})
}

// The CHE-764 Hermes exception no longer lets an agent through.
func TestPasskeyRequired_HermesExceptionRefused(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, agentID, taskID := hermesExceptionFixture(t, "che3482")
	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, hermesWorkspaceReq(agentID, taskID, map[string]any{
		"context": "agent text", "expected_before_digest": testDigest("original workspace context"),
	}))
	wantPasskeyRequired(t, w, "workspace.context.set")
	var stored string
	dbfx.QueryRow(t, `SELECT context FROM workspace WHERE id = $1`, hermesExceptionWorkspaceID).Scan(&stored)
	if stored != "original workspace context" {
		t.Fatalf("context moved: %q", stored)
	}
}

// Create routes carry instructions too.
func TestPasskeyRequired_CreateRoutesRefuseInstructions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newVoteFixture(t)

	w := httptest.NewRecorder()
	testHandler.CreateAgent(w, asAgentActor(newRequestAs(testUserID, http.MethodPost, "/api/agents", map[string]any{
		"name": "che3482-create-agent", "runtime_id": handlerTestRuntimeID(t), "instructions": "smuggled",
	}), f.writerID, f.taskID))
	wantPasskeyRequired(t, w, "agent.instructions.set")

	w = httptest.NewRecorder()
	testHandler.CreateSquad(w, asAgentActor(squadScopeReq(testUserID, http.MethodPost, "/api/squads", map[string]any{
		"name": "che3482-create-squad", "leader_id": f.leaderID, "instructions": "smuggled",
	}, nil), f.writerID, f.taskID))
	wantPasskeyRequired(t, w, "squad.instructions.set")
	if n := dbfx.Count(t, `SELECT count(*) FROM squad WHERE name = 'che3482-create-squad'`); n != 0 {
		t.Fatalf("squad was created: %d", n)
	}
}

// Settings keys on the allow table: every request field that writes one.
func TestPasskeyRequired_SettingsAllowKeysRefused(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newVoteFixture(t)
	for name, body := range map[string]map[string]any{
		"settings blob changes an allow key": {"settings": map[string]any{"mermaid_orientation": "TD"}},
		"settings blob drops an allow key":   {"settings": map[string]any{}},
		"compact_prompt":                     {"compact_prompt": "agent prompt"},
		"auto_compact":                       {"auto_compact": map[string]any{"enabled": true}},
		"wakeups_paused":                     {"wakeups_paused": true},
		"idle_started_guard":                 {"idle_started_guard": map[string]any{"mode": "act"}},
	} {
		t.Run(name, func(t *testing.T) {
			var previous []byte
			dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
			dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_build_object('mermaid_orientation', 'LR', 'github_enabled', true) WHERE id = $1`, testWorkspaceID)
			t.Cleanup(func() {
				dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(previous))
			})
			var seeded []byte
			dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&seeded)

			w := httptest.NewRecorder()
			testHandler.UpdateWorkspace(w, asAgentActor(withURLParam(newRequestAs(testUserID, http.MethodPatch, "/api/workspaces/"+testWorkspaceID, body), "id", testWorkspaceID), f.writerID, f.taskID))
			wantPasskeyRequired(t, w, "workspace.settings.patch")
			var after []byte
			dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&after)
			if string(after) != string(seeded) {
				t.Fatalf("settings moved: %s -> %s", seeded, after)
			}
		})
	}
}

// A settings blob that leaves every allow key as it is, and the fields outside
// the table, still work for an agent.
func TestPasskeyRequired_NonCoveredWorkspaceWritesStillWork(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newVoteFixture(t)
	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_build_object('mermaid_orientation', 'LR') WHERE id = $1`, testWorkspaceID)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(previous))
	})

	w := httptest.NewRecorder()
	testHandler.UpdateWorkspace(w, asAgentActor(withURLParam(newRequestAs(testUserID, http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{
		"settings": map[string]any{"mermaid_orientation": "LR", "unrelated_key": 1},
	}), "id", testWorkspaceID), f.writerID, f.taskID))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// Workflow routes: steps, step instructions, handoffs, and creating a workflow
// with steps. A decision-bound write and a PASS receipt no longer pass.
func TestPasskeyRequired_WorkflowRoutesRefuseMachineCredentials(t *testing.T) {
	edit := setInstructions("agent text")
	write := newBoundWorkflowWrite(t, "workflow.instructions", edit, "workflow.instructions")
	steps := write.steps(t, edit)

	w := write.patch(t, write.body(t, steps))
	wantPasskeyRequired(t, w.ResponseRecorder, "workflow.steps.set")
	if got := write.storedInstructions(t, "in_progress"); got != "" {
		t.Fatalf("stored instructions moved: %q", got)
	}
	if write.auditRows(t) != 0 {
		t.Fatal("the refused write must not spend the decision")
	}

	w = write.putHandoff(t, "in_progress", write.bind(map[string]any{"handler": map[string]any{"type": "member", "id": testUserID}}, write.before, write.before))
	wantPasskeyRequired(t, w.ResponseRecorder, "workflow.steps.set")

	create := write.asAgent(workflowRequest("POST", "/api/issue-workflows", map[string]any{
		"name": "che3482 agent workflow", "steps": []map[string]any{{"status_key": "backlog"}},
	}))
	rec := httptest.NewRecorder()
	testHandler.CreateIssueWorkflow(rec, create)
	wantPasskeyRequired(t, rec, "workflow.steps.set")
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_workflow WHERE name = 'che3482 agent workflow'`); n != 0 {
		t.Fatalf("workflow was created: %d", n)
	}
}

// The legacy child-done default writer sets two allow-table keys. A machine
// credential (task token or cloud-node PAT) is refused before persistence; the
// human owner still writes it.
func TestPasskeyRequired_ChildDoneDefaultWriterRefusesMachineCredentials(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newVoteFixture(t)
	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_build_object('system_wakeup_child_done', true) WHERE id = $1`, testWorkspaceID)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(previous))
	})
	var seeded []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&seeded)

	put := func(decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
		req := withURLParam(newRequestAs(testUserID, http.MethodPut, "/api/system-wakeups/child_done", map[string]any{
			"enabled": false, "instruction": "machine text",
		}), "rule", "child_done")
		w := httptest.NewRecorder()
		testHandler.UpdateWorkspaceSystemWakeup(w, decorate(req))
		return w
	}
	for name, decorate := range map[string]func(*http.Request) *http.Request{
		"agent":      func(r *http.Request) *http.Request { return asAgentActor(r, f.writerID, f.taskID) },
		"task token": func(r *http.Request) *http.Request { r.Header.Set("X-Actor-Source", "task_token"); return r },
		"cloud node": asCloudNodeActor,
	} {
		t.Run(name, func(t *testing.T) {
			wantPasskeyRequired(t, put(decorate), "workspace.settings.patch")
			var after []byte
			dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&after)
			if string(after) != string(seeded) {
				t.Fatalf("settings moved: %s -> %s", seeded, after)
			}
		})
	}
	t.Run("human control", func(t *testing.T) {
		if w := put(func(r *http.Request) *http.Request { return r }); w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// Archiving an unused custom status removes its step, with any handoff and
// instructions, from the Default workflow. A machine credential is refused
// before persistence; the human owner still archives.
func TestPasskeyRequired_ArchiveStatusRefusesMachineCredentials(t *testing.T) {
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	withFeatureFlag(t, testHandler, featureflags.ProjectWorkflowsV1, true)
	entry := createTestCustomStatus(t, "che3482_arc_"+workflowTestSuffix()[10:], "started")
	var list struct {
		Workflows []IssueWorkflowResponse `json:"workflows"`
	}
	testutil.Call(t, testHandler.ListIssueWorkflows, workflowRequest("GET", "/api/issue-workflows", nil)).Want(http.StatusOK).JSON(&list)
	def := list.Workflows[0]
	patch := withURLParam(workflowRequest("PATCH", "/api/issue-workflows/"+def.ID, map[string]any{
		"steps": statusSteps("in_progress", entry.Key),
	}), "id", def.ID)
	testutil.Call(t, testHandler.UpdateIssueWorkflow, patch).Want(http.StatusOK)
	var seeded string
	dbfx.QueryRow(t, `SELECT steps::text FROM issue_workflow WHERE id = $1`, def.ID).Scan(&seeded)

	id := util.UUIDToString(entry.ID)
	archive := func(decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
		req := withURLParam(newRequest(http.MethodDelete, "/api/issue-statuses/"+id, nil), "id", id)
		w := httptest.NewRecorder()
		testHandler.ArchiveIssueStatus(w, decorate(req))
		return w
	}
	for name, decorate := range map[string]func(*http.Request) *http.Request{
		"task token": func(r *http.Request) *http.Request { r.Header.Set("X-Actor-Source", "task_token"); return r },
		"cloud node": asCloudNodeActor,
	} {
		t.Run(name, func(t *testing.T) {
			wantPasskeyRequired(t, archive(decorate), "workflow.steps.set")
			var after string
			dbfx.QueryRow(t, `SELECT steps::text FROM issue_workflow WHERE id = $1`, def.ID).Scan(&after)
			if after != seeded {
				t.Fatalf("Default steps moved: %s -> %s", seeded, after)
			}
		})
	}
	t.Run("human control", func(t *testing.T) {
		if w := archive(func(r *http.Request) *http.Request { return r }); w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})
}
