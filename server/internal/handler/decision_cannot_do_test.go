package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// setCannotDo stores the CHE-5902 settings; catalog is raw JSON or "" for the built-in entries.
func setCannotDo(t *testing.T, mode, catalog string) {
	t.Helper()
	ctx := context.Background()
	patch := `{"agent_cannot_do_mode":"` + mode + `"`
	if catalog != "" {
		patch += `,"agent_cannot_do":` + catalog
	}
	patch += `}`
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings = COALESCE(settings, '{}') || $1::jsonb WHERE id = $2`, patch, testWorkspaceID); err != nil {
		t.Fatalf("set agent_cannot_do: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = settings - 'agent_cannot_do_mode' - 'agent_cannot_do' WHERE id = $1`, testWorkspaceID)
	})
}

// cannotDoAsk is the CHE-4297 card: the first option promises an agent run.
func cannotDoAsk(fixture requestCoreFixture, first map[string]any) map[string]any {
	ask := fixture.newShapeAsk()
	options := ask["options"].([]any)
	options[0].(map[string]any)["label"] = "Yes, reload"
	ask["recommended_option"], ask["recommendation"], ask["default_if_no_reply"] = "Yes, reload", "Yes, reload", "Yes, reload"
	for key, value := range first {
		options[0].(map[string]any)[key] = value
	}
	options[1].(map[string]any)["after_choice"] = "record_only"
	return ask
}

func agentRunOption(argv ...string) map[string]any {
	return map[string]any{"after_choice": "agent_run", "agent_commands": []any{argv}}
}

func TestDecisionRequestBlocksOptionThatPromisesWhatAnAgentRunCannotDo(t *testing.T) {
	setCannotDo(t, "block", "")
	for name, argv := range map[string][]string{
		"CHE-4297 card":   {"$MULTICA_CLI_PATH", "daemon", "restart"},
		"plain name":      {"multica", "daemon", "restart"},
		"quotes kept":     {`"$MULTICA_CLI_PATH"`, "daemon", "restart"},
		"path and global": {"/home/u/.local/bin/multica", "--profile", "dev", "daemon", "restart"},
		"exe stop":        {"multica.exe", "daemon", "stop"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRequestCoreFixture(t, false)
			res := testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, cannotDoAsk(fixture, agentRunOption(argv...)))).Want(http.StatusBadRequest)
			for _, want := range []string{"Yes, reload", "daemon_", "multica daemon"} {
				if !strings.Contains(res.Text(), want) {
					t.Errorf("400 body lacks %q: %s", want, res.Text())
				}
			}
			fixture.assertCounts(t, 0)
		})
	}
}

func TestDecisionRequestBlockAcceptsWhatAnAgentRunCanDo(t *testing.T) {
	setCannotDo(t, "block", "")
	for name, option := range map[string]map[string]any{
		"status":             agentRunOption("$MULTICA_CLI_PATH", "daemon", "status"),
		"restarts":           agentRunOption("multica", "daemon", "restarts"),
		"human steps option": {"after_choice": "human_steps"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRequestCoreFixture(t, false)
			testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, cannotDoAsk(fixture, option))).Want(http.StatusCreated)
		})
	}
}

func TestDecisionRequestBlockRefusesWrapperAndStringCommand(t *testing.T) {
	setCannotDo(t, "block", "")
	fixture := newRequestCoreFixture(t, false)
	res := testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, cannotDoAsk(fixture, agentRunOption("env", "X=1", "multica", "daemon", "restart")))).Want(http.StatusBadRequest)
	if !strings.Contains(res.Text(), "give the command itself as argv") {
		t.Errorf("wrapper body = %s", res.Text())
	}
	str := cannotDoAsk(fixture, map[string]any{"after_choice": "agent_run", "agent_commands": []any{"multica daemon restart"}})
	res = testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, str)).Want(http.StatusBadRequest)
	if !strings.Contains(res.Text(), "ask.options[0].agent_commands[0]") {
		t.Errorf("string body = %s", res.Text())
	}
	fixture.assertCounts(t, 0)
}

func TestDecisionRequestBlockRefusesMissingAfterChoice(t *testing.T) {
	setCannotDo(t, "block", "")
	fixture := newRequestCoreFixture(t, false)
	res := testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, fixture.newShapeAsk())).Want(http.StatusBadRequest)
	if !strings.Contains(res.Text(), "ask.options[0].after_choice") {
		t.Errorf("body = %s", res.Text())
	}
	fixture.assertCounts(t, 0)
}

func TestDecisionRequestWarnCreatesCardWithOneHintAndOffNone(t *testing.T) {
	for mode, wantHints := range map[string]int{"warn": 1, "off": 0} {
		t.Run(mode, func(t *testing.T) {
			setCannotDo(t, mode, "")
			fixture := newRequestCoreFixture(t, false)
			var created struct {
				Hints []string `json:"hints"`
			}
			testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, cannotDoAsk(fixture, agentRunOption("multica", "daemon", "restart")))).Want(http.StatusCreated).JSON(&created)
			got := 0
			for _, hint := range created.Hints {
				if strings.Contains(hint, "daemon_restart") {
					got++
				}
			}
			if got != wantHints {
				t.Errorf("hints = %q, want %d about daemon_restart", created.Hints, wantHints)
			}
		})
	}
}

func TestDecisionRequestCannotDoLeavesActionAndWaiverCardsAlone(t *testing.T) {
	setCannotDo(t, "block", "")
	enableActionRequests(t, true)
	fixture := newRequestCoreFixture(t, false)
	testutil.Call(t, testHandler.RequestDecision, fixture.postAction(fixture.actionBody())).Want(http.StatusCreated)
	waiver := newRequestCoreFixture(t, false)
	postRawAsk(t, waiver, waiverAsk(t, waiver, nil)).Want(http.StatusCreated)
}

func TestWorkspaceSettingsValidateCannotDo(t *testing.T) {
	good := map[string]any{"agent_cannot_do_mode": "warn", "agent_cannot_do": []any{map[string]any{"key": "k", "tool": "gh", "subcommand": []any{"pr", "merge"}, "human_step": "Merge it."}}}
	if err := validateAgentCannotDoSetting(good); err != nil {
		t.Fatalf("good settings refused: %v", err)
	}
	for name, bad := range map[string]map[string]any{
		"mode":    {"agent_cannot_do_mode": "strict"},
		"null":    {"agent_cannot_do_mode": nil},
		"catalog": {"agent_cannot_do": []any{map[string]any{"key": "k"}}},
	} {
		if err := validateAgentCannotDoSetting(bad); err == nil {
			t.Errorf("%s settings accepted", name)
		}
	}
}

// ownerCatalogPatch saves settings as the given member with the route's own handler.
func ownerCatalogPatch(t *testing.T, userID string, settings map[string]any) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{"settings": settings}), "id", testWorkspaceID)
	req.Header.Set("X-User-ID", userID)
	return testutil.Call(t, testHandler.UpdateWorkspace, req)
}

func TestWorkspaceCannotDoCatalogIsOwnerOnly(t *testing.T) {
	var stored []byte
	if err := testPool.QueryRow(context.Background(), `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = $1 WHERE id = $2`, stored, testWorkspaceID)
	})
	var base map[string]any
	if err := json.Unmarshal(stored, &base); err != nil || base == nil {
		base = map[string]any{}
	}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	catalog := []any{map[string]any{"key": "k", "tool": "gh", "subcommand": []any{"pr", "merge"}, "human_step": "Merge it."}}

	adminID := createPlainMember(t, "che5902-admin-"+safeTestName(t)+"@multica.test")
	dbfx.Exec(t, `UPDATE member SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, adminID)

	// An admin cannot add, change or drop the catalog.
	ownerCatalogPatch(t, adminID, with(map[string]any{"agent_cannot_do": catalog})).Want(http.StatusForbidden)
	ownerCatalogPatch(t, testUserID, with(map[string]any{"agent_cannot_do": catalog})).Want(http.StatusOK)
	ownerCatalogPatch(t, adminID, with(map[string]any{"agent_cannot_do": []any{}})).Want(http.StatusForbidden)
	ownerCatalogPatch(t, adminID, base).Want(http.StatusForbidden) // drops the stored catalog

	// An admin may save the other settings while the catalog stays as stored.
	ownerCatalogPatch(t, adminID, with(map[string]any{"agent_cannot_do": catalog, "agent_cannot_do_mode": "warn"})).Want(http.StatusOK)
}

func TestCIDecisionReturnsCannotDoHintsInWarnMode(test *testing.T) {
	setCannotDo(test, "warn", "")
	fixture := newSingleCallFixture(test)
	response := ciCardCall(test, ciCard, fixture, func(body map[string]any) {
		ask := fixture.details
		ask.Options = append([]decisiontx.AskOption(nil), ask.Options...)
		ask.Options[0].AfterChoice = decisiontx.AfterAgentRun
		ask.Options[0].AgentCommands = [][]string{{"multica", "daemon", "restart"}}
		for i := 1; i < len(ask.Options); i++ {
			ask.Options[i].AfterChoice = decisiontx.AfterRecordOnly
		}
		body["ask"] = ask
	})
	if response.Code != http.StatusCreated {
		test.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		Hints []string `json:"hints"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		test.Fatal(err)
	}
	for _, hint := range created.Hints {
		if strings.Contains(hint, "daemon_restart") {
			return
		}
	}
	test.Fatalf("hints = %q, want one about daemon_restart", created.Hints)
}

func TestDecisionRequestBlockRefusesRelayOnlyCommands(t *testing.T) {
	setCannotDo(t, "block", "")
	for name, tc := range map[string]struct {
		argv []string
		want string
	}{
		"status":  {[]string{"$MULTICA_CLI_PATH", "issue", "status", "CHE-1", "done"}, "issue.status.set"},
		"propose": {[]string{"multica", "decision", "propose"}, "--action-file"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRequestCoreFixture(t, false)
			res := testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, cannotDoAsk(fixture, agentRunOption(tc.argv...)))).Want(http.StatusBadRequest)
			if !strings.Contains(res.Text(), tc.want) {
				t.Errorf("400 body lacks %q: %s", tc.want, res.Text())
			}
			fixture.assertCounts(t, 0)
		})
	}
	fixture := newRequestCoreFixture(t, false)
	testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, cannotDoAsk(fixture, agentRunOption("multica", "issue", "get", "CHE-1")))).Want(http.StatusCreated)
}

func TestDecisionRequestAllowedAgentsMatchTheAsker(t *testing.T) {
	fixture := newRequestCoreFixture(t, false)
	catalog := func(id string) string {
		return `[{"key":"c00_restart","tool":"docker","subcommand":["compose","restart"],"human_step":"Run it on C00.","allowed_agents":["` + id + `"]}]`
	}
	ask := func() map[string]any { return cannotDoAsk(fixture, agentRunOption("docker", "compose", "restart", "doop")) }

	setCannotDo(t, "block", catalog(createHandlerTestAgent(t, "other c00 agent", []byte("[]"))))
	res := testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, ask())).Want(http.StatusBadRequest)
	if !strings.Contains(res.Text(), "c00_restart") || !strings.Contains(res.Text(), "issue.assign") {
		t.Errorf("400 body = %s", res.Text())
	}
	setCannotDo(t, "block", catalog(fixture.agentID))
	testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, ask())).Want(http.StatusCreated)
}

func TestWorkspaceSettingsRefuseAllowedAgentsOutsideTheWorkspace(t *testing.T) {
	entry := func(id string) map[string]any {
		return map[string]any{"agent_cannot_do": []any{map[string]any{"key": "k", "tool": "gh", "human_step": "x", "allowed_agents": []any{id}}}}
	}
	if err := testHandler.validateCannotDoAgents(context.Background(), parseUUID(testWorkspaceID), entry("33333333-3333-4333-8333-333333333333")); err == nil {
		t.Error("unknown agent accepted")
	}
	if err := testHandler.validateCannotDoAgents(context.Background(), parseUUID(testWorkspaceID), entry(createHandlerTestAgent(t, "listed agent", []byte("[]")))); err != nil {
		t.Errorf("workspace agent refused: %v", err)
	}
}
