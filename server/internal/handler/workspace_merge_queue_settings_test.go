package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/mergequeue"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-7408 (B1 of CHE-7148, plan 4.6): the three merge-queue settings go
// through one validator on every write path. Until B11, routing "block" is
// refused with routing-not-available on all of them.

const mqIssue = "01a1f14c-4bf5-7a02-9021-decf51944c61"

var mqValid = map[string]any{
	mergequeue.RoutingKey:       "warn",
	mergequeue.ActorBindingsKey: map[string]any{"agents": []any{"congvc-bot-x99"}, "people": []any{"congvc-dev"}},
	mergequeue.WatchIssueKey:    mqIssue,
}

var mqRefused = []struct {
	name, key, want string
	value           any
}{
	{"block", mergequeue.RoutingKey, "routing-not-available", "block"},
	{"unknown routing", mergequeue.RoutingKey, mergequeue.RoutingKey, "ON"},
	{"login in both lists", mergequeue.ActorBindingsKey, "both", map[string]any{"agents": []any{"x"}, "people": []any{"X"}}},
	{"bindings not an object", mergequeue.ActorBindingsKey, mergequeue.ActorBindingsKey, []any{"x"}},
	{"watch issue not an id", mergequeue.WatchIssueKey, mergequeue.WatchIssueKey, "CHE-7408"},
}

func mqSettings(t *testing.T) map[string]any {
	t.Helper()
	stored, err := testHandler.Queries.GetWorkspace(context.Background(), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(stored.Settings, &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

func mqUpdate(t *testing.T, body map[string]any) *testutil.Response {
	t.Helper()
	request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, body), "id", testWorkspaceID)
	return testutil.Call(t, testHandler.UpdateWorkspace, request)
}

// Path 1: the owner flag fields, one settings key each, nothing else touched.
func TestWorkspaceMergeQueueOwnerFields(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setWorkspaceSettings(t, `{"compact_prompt":"Keep SHAs.","ask_guard_mode":"warn"}`)
	for key, value := range mqValid {
		mqUpdate(t, map[string]any{key: value}).Want(http.StatusOK)
		if got, _ := json.Marshal(mqSettings(t)[key]); string(got) != mqJSON(t, value) {
			t.Errorf("%s stored %s, want %s", key, got, mqJSON(t, value))
		}
	}
	if s := mqSettings(t); s["compact_prompt"] != "Keep SHAs." || s["ask_guard_mode"] != "warn" {
		t.Errorf("a merge-queue field changed another key: %v", s)
	}
	for _, tc := range mqRefused {
		before := mqSettings(t)[tc.key]
		body := mqUpdate(t, map[string]any{tc.key: tc.value}).Want(http.StatusBadRequest).Body.String()
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s: body %q does not contain %q", tc.name, body, tc.want)
		}
		if got := mqSettings(t)[tc.key]; mqJSON(t, got) != mqJSON(t, before) {
			t.Errorf("%s: a refused write changed %s to %v", tc.name, tc.key, got)
		}
	}
	// An admin is not the owner, on every field.
	setRuntimeTestMemberRole(t, testUserID, "admin")
	t.Cleanup(func() { setRuntimeTestMemberRole(t, testUserID, "owner") })
	for key := range mqValid {
		mqUpdate(t, map[string]any{key: map[string]any{mergequeue.RoutingKey: "off"}[key]}).Want(http.StatusForbidden)
	}
}

// Path 2: the generic settings save, owner only for all three keys.
func TestGenericSettingsSaveValidatesMergeQueueKeys(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setWorkspaceSettings(t, `{"compact_prompt":"keep"}`)
	mqUpdate(t, map[string]any{"settings": map[string]any{"compact_prompt": "keep", mergequeue.RoutingKey: "warn"}}).Want(http.StatusOK)
	if got := mqSettings(t)[mergequeue.RoutingKey]; got != "warn" {
		t.Fatalf("valid routing not stored: %v", got)
	}
	for _, tc := range mqRefused {
		body := mqUpdate(t, map[string]any{"settings": map[string]any{tc.key: tc.value}}).Want(http.StatusBadRequest).Body.String()
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s: body %q does not contain %q", tc.name, body, tc.want)
		}
	}
	if got := mqSettings(t)[mergequeue.RoutingKey]; got != "warn" {
		t.Errorf("a refused save changed routing: %v", got)
	}
	// An admin may save the blob unchanged but not move a merge-queue key.
	setRuntimeTestMemberRole(t, testUserID, "admin")
	t.Cleanup(func() { setRuntimeTestMemberRole(t, testUserID, "owner") })
	same := map[string]any{"compact_prompt": "keep", mergequeue.RoutingKey: "warn"}
	mqUpdate(t, map[string]any{"settings": same}).Want(http.StatusOK)
	for key, value := range mqValid {
		moved := map[string]any{"compact_prompt": "keep", mergequeue.RoutingKey: "warn", key: value}
		if key == mergequeue.RoutingKey {
			moved[key] = "off"
		}
		mqUpdate(t, map[string]any{"settings": moved}).Want(http.StatusForbidden)
	}
}

// Path 3: the config propose action. The keys are on the allow table, the
// request runs the same validator, and approval writes only the named keys.
func TestConfigProposeMergeQueueKeys(t *testing.T) {
	for _, tc := range mqRefused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			_, body := requestSettings(t, http.StatusBadRequest, map[string]any{tc.key: tc.value}, nil)
			mustContain(t, body, "invalid_params")
			mustContain(t, body, tc.want)
			if got := storedSettings(t); got[tc.key] != nil {
				t.Fatalf("a refused request stored %s", got[tc.key])
			}
		})
	}
	request, _ := requestSettings(t, http.StatusCreated, mqValid, nil)
	_, keyID := request.action.enrollPasskey(t)
	verify := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) { return parseUUID(keyID), nil }
	request.approveOwner(t, withVerifier(verify), http.StatusOK)
	got := storedSettings(t)
	for key, value := range mqValid {
		var stored, want any
		_ = json.Unmarshal(got[key], &stored)
		_ = json.Unmarshal([]byte(mqJSON(t, value)), &want)
		if mqJSON(t, stored) != mqJSON(t, want) {
			t.Errorf("%s stored %s, want %v", key, got[key], value)
		}
	}
	if string(got["compact_prompt"]) != `"old"` {
		t.Errorf("an unnamed key changed: %s", got["compact_prompt"])
	}
}

func mqJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
