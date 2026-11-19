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

// setServerActions stores decision_server_actions for the test and restores the key after it.
func setServerActions(t *testing.T, value string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings = COALESCE(settings, '{}') || $1::jsonb WHERE id = $2`, `{"decision_server_actions":"`+value+`"}`, testWorkspaceID); err != nil {
		t.Fatalf("set decision_server_actions: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = settings - 'decision_server_actions' WHERE id = $1`, testWorkspaceID)
	})
}

// typedAsk is a two-option card whose first option is a server_action that sets the issue status.
func typedAsk(fixture requestCoreFixture) map[string]any {
	return cannotDoAsk(fixture, map[string]any{
		"after_choice": "server_action",
		"action":       map[string]any{"type": "issue.status.set", "params": map[string]any{"status": " In_Review "}},
	})
}

func postTyped(t *testing.T, fixture requestCoreFixture, ask map[string]any, extra map[string]any) *testutil.Response {
	t.Helper()
	body := map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "escalation_class": "reversible", "ask": ask}
	for key, value := range extra {
		body[key] = value
	}
	request := newRequest(http.MethodPost, "/api/decisions", body)
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	return testutil.Call(t, testHandler.RequestDecision, request)
}

func requestCount(t *testing.T, fixture requestCoreFixture) int {
	t.Helper()
	var count int
	dbfx.QueryRow(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueID).Scan(&count)
	return count
}

// With the setting on, a server_action option is stored normalized with the effect line.
func TestTypedOptionIsStoredNormalizedWithEffectLine(t *testing.T) {
	setServerActions(t, "on")
	fixture := newRequestCoreFixture(t, false)
	var created map[string]any
	postTyped(t, fixture, typedAsk(fixture), nil).Want(http.StatusCreated).JSON(&created)
	var askJSON []byte
	dbfx.QueryRow(t, `SELECT ask_json FROM decision_request WHERE id = $1`, created["id"]).Scan(&askJSON)
	var stored struct {
		Options []struct {
			WhatHappens string `json:"what_happens"`
			AfterChoice string `json:"after_choice"`
			Action      struct {
				Type   string            `json:"type"`
				Params map[string]string `json:"params"`
			} `json:"action"`
		} `json:"options"`
	}
	if err := json.Unmarshal(askJSON, &stored); err != nil {
		t.Fatal(err)
	}
	first := stored.Options[0]
	if first.AfterChoice != "server_action" || first.Action.Type != "issue.status.set" || first.Action.Params["status"] != "in_review" {
		t.Fatalf("stored option = %+v, want server_action issue.status.set in_review", first)
	}
	if want := "On pick: the server moves "; strings.Count(first.WhatHappens, want) != 1 || !strings.Contains(first.WhatHappens, " to in_review as you.") {
		t.Fatalf("what_happens = %q, want one effect line naming in_review", first.WhatHappens)
	}
}

// With the setting off (the default) a typed option is refused with server_actions_off and the field path.
func TestTypedOptionIsRefusedWhileSettingIsOff(t *testing.T) {
	for _, value := range []string{"", "off", "yes"} { // "" is the unset default; "yes" is malformed and reads as off
		t.Run("setting "+value, func(t *testing.T) {
			if value != "" {
				setServerActions(t, value)
			}
			fixture := newRequestCoreFixture(t, false)
			res := postTyped(t, fixture, typedAsk(fixture), nil).Want(http.StatusBadRequest)
			if !strings.Contains(res.Body.String(), `"code":"server_actions_off"`) || !strings.Contains(res.Body.String(), "ask.options[0].after_choice") {
				t.Fatalf("body = %s, want code server_actions_off and the field path", res.Body.String())
			}
			if n := requestCount(t, fixture); n != 0 {
				t.Fatalf("refused ask stored %d request(s)", n)
			}
		})
	}
}

// A typed option is refused, naming its field path, on every path that cannot carry one, even with the setting on.
func TestTypedOptionIsRefusedOnPathsThatCannotCarryIt(t *testing.T) {
	setServerActions(t, "on")
	t.Run("topic ask", func(t *testing.T) {
		fixture := newRequestCoreFixture(t, false)
		ask := waiverAsk(t, fixture, nil)
		first := ask["options"].([]map[string]any)[0]
		first["after_choice"] = "server_action"
		first["action"] = map[string]any{"type": "issue.status.set", "params": map[string]any{"status": "done"}}
		res := postRawAsk(t, fixture, ask).Want(http.StatusBadRequest)
		if !strings.Contains(res.Body.String(), ".after_choice is not allowed on a topic ask") {
			t.Fatalf("body = %s", res.Body.String())
		}
		if n := requestCount(t, fixture); n != 0 {
			t.Fatalf("stored %d request(s)", n)
		}
	})
	t.Run("batch card", func(t *testing.T) {
		fixture := newRequestCoreFixture(t, false)
		res := postTyped(t, fixture, typedAsk(fixture), map[string]any{"covers_request_ids": []string{"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"}}).Want(http.StatusBadRequest)
		if !strings.Contains(res.Body.String(), ".after_choice is not allowed on a batch card") {
			t.Fatalf("body = %s", res.Body.String())
		}
	})
	t.Run("after_wait on a topic ask", func(t *testing.T) {
		fixture := newRequestCoreFixture(t, false)
		ask := waiverAsk(t, fixture, nil)
		first := ask["options"].([]map[string]any)[0]
		first["after_choice"] = "human_steps"
		first["after_wait"] = map[string]any{"until_status": map[string]any{"status": "done"}}
		res := postRawAsk(t, fixture, ask).Want(http.StatusBadRequest)
		if !strings.Contains(res.Body.String(), ".after_wait is not allowed on a topic ask") {
			t.Fatalf("body = %s", res.Body.String())
		}
	})
	t.Run("kind action", func(t *testing.T) {
		enableActionRequests(t, true)
		fixture := newRequestCoreFixture(t, false)
		body := fixture.actionBody()
		body["ask"].(map[string]any)["options"] = typedAsk(fixture)["options"]
		res := testutil.Call(t, testHandler.RequestDecision, fixture.postAction(body)).Want(http.StatusBadRequest)
		if !strings.Contains(res.Body.String(), "ask.options") {
			t.Fatalf("body = %s, want a refusal naming ask.options", res.Body.String())
		}
	})
	t.Run("CI card", func(t *testing.T) {
		fixture := newSingleCallFixture(t)
		response := ciCardCall(t, ciCard, fixture, func(body map[string]any) {
			ask := fixture.details
			ask.Options = append(ask.Options[:0:0], ask.Options...)
			ask.Options[0].AfterChoice = "server_action"
			ask.Options[0].Action = &decisiontx.OptionAction{Type: "issue.status.set", Params: decisiontx.ActionParams{Status: "done"}}
			body["ask"] = ask
		})
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), ".after_choice is not allowed on a CI card") {
			t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
		}
	})
}

// Only an owner changes the two settings; a malformed value is refused on write.
func TestServerActionSettingsAreOwnerOnly(t *testing.T) {
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
	adminID := createPlainMember(t, "che7358-admin-"+safeTestName(t)+"@multica.test")
	dbfx.Exec(t, `UPDATE member SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, adminID)

	for name, change := range map[string]map[string]any{
		"decision_server_actions": {"decision_server_actions": "on"},
		"standing_answers":        {"standing_answers": map[string]any{"review_cap": "split"}},
	} {
		t.Run(name, func(t *testing.T) {
			ownerCatalogPatch(t, adminID, with(change)).Want(http.StatusForbidden)
			ownerCatalogPatch(t, testUserID, with(change)).Want(http.StatusOK)
			ownerCatalogPatch(t, adminID, base).Want(http.StatusForbidden) // drops the stored value
			ownerCatalogPatch(t, testUserID, base).Want(http.StatusOK)
		})
	}
	ownerCatalogPatch(t, testUserID, with(map[string]any{"decision_server_actions": "maybe"})).Want(http.StatusBadRequest)
	ownerCatalogPatch(t, testUserID, with(map[string]any{"standing_answers": map[string]any{"review_cap": "merge"}})).Want(http.StatusBadRequest)
}
