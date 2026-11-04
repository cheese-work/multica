package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// seededSettings holds allow-table keys the patches touch or leave alone, the
// delegation setting (denylisted) and a key no table names.
const seededSettings = `{"compact_prompt":"old","github_wake_on_pr_merge":true,"co_authored_by_enabled":true,"delegated_governed_writes":{"allowed_agents":["a"]},"unlisted":"keep"}`

// setWorkspaceSettings replaces the fixture workspace's settings for the test
// and restores the original afterwards.
func setWorkspaceSettings(test *testing.T, settings string) {
	test.Helper()
	var original string
	dbfx.QueryRow(test, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&original)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, original)
	})
	dbfx.Exec(test, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, settings)
}

func storedSettings(test *testing.T) map[string]json.RawMessage {
	test.Helper()
	var raw string
	dbfx.QueryRow(test, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&raw)
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		test.Fatal(err)
	}
	return out
}

// liveSettingsDigest is the digest of the named keys as stored now.
func liveSettingsDigest(test *testing.T, keys ...string) string {
	test.Helper()
	stored := storedSettings(test)
	named := map[string]json.RawMessage{}
	for _, key := range keys {
		named[key] = stored[key]
	}
	digest, err := decisionaction.SettingsDigest(testWorkspaceID, named)
	if err != nil {
		test.Fatal(err)
	}
	return digest
}

// patchDigest is the after digest a correct agent computes for a patch of
// plain (replace) keys: the patched values, null for a removal.
func patchDigest(test *testing.T, patch map[string]any) string {
	test.Helper()
	named := map[string]json.RawMessage{}
	for key, value := range patch {
		raw, err := json.Marshal(value)
		if err != nil {
			test.Fatal(err)
		}
		named[key] = raw
	}
	digest, err := decisionaction.SettingsDigest(testWorkspaceID, named)
	if err != nil {
		test.Fatal(err)
	}
	return digest
}

func patchKeys(patch map[string]any) []string {
	keys := make([]string, 0, len(patch))
	for key := range patch {
		keys = append(keys, key)
	}
	return keys
}

type settingsRequest struct {
	action pendingAction
	base   string
}

// fileSettings asks for the action and expects status. A refused request must
// store nothing.
func fileSettings(test *testing.T, status int, params map[string]any) (settingsRequest, string) {
	test.Helper()
	fixture := newActionFixture(test)
	var created actionResponse
	body := fixture.respond(test, fixture.withAction(map[string]any{"type": settingsPatchAction, "version": 1, "params": params}), status, &created)
	if status != http.StatusCreated {
		if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
			test.Fatalf("refused request stored %d requests and %d actions; want none", requests, actions)
		}
		return settingsRequest{}, body
	}
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	base, _ := params["base_sha256"].(string)
	return settingsRequest{pendingAction{fixture, created.ID, created.Action.ID}, base}, body
}

// requestSettings seeds the settings and files a correct request for the
// patch; edit may break the params first.
func requestSettings(test *testing.T, status int, patch map[string]any, edit func(params map[string]any)) (settingsRequest, string) {
	test.Helper()
	setWorkspaceSettings(test, seededSettings)
	params := map[string]any{
		"workspace_id": testWorkspaceID, "settings": patch,
		"base_sha256": liveSettingsDigest(test, patchKeys(patch)...), "after_sha256": patchDigest(test, patch),
	}
	if edit != nil {
		edit(params)
	}
	return fileSettings(test, status, params)
}

func (request settingsRequest) claim(test *testing.T) map[string]any {
	test.Helper()
	claim := request.action.claim(test, map[string]string{})
	var preview string
	dbfx.QueryRow(test, `SELECT preconditions->>'preview_sha256' FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&preview)
	claim["preview_sha256"] = preview
	return claim
}

func (request settingsRequest) approveOwner(test *testing.T, handler *Handler, status int) string {
	test.Helper()
	makeApproverOwner(test, request.action, "owner")
	return request.action.approve(test, handler, request.claim(test), status)
}

// assertNotApplied proves a refused approval changed no stored key.
func (request settingsRequest) assertNotApplied(test *testing.T) {
	test.Helper()
	request.action.assertUntouched(test)
	if got := storedSettings(test); string(got["compact_prompt"]) != `"old"` || string(got["github_wake_on_pr_merge"]) != `true` || got["mermaid_orientation"] != nil {
		test.Fatalf("settings changed by a refused approval: %v", got)
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 0 {
		test.Fatalf("config_change_applied rows = %d, want 0", n)
	}
}

func TestSettingsPatchAppliesNamedKeysOnly(test *testing.T) {
	patch := map[string]any{"compact_prompt": "new", "mermaid_orientation": "author", "github_wake_on_pr_merge": nil}
	request, _ := requestSettings(test, http.StatusCreated, patch, nil)
	_, keyID := request.action.enrollPasskey(test)
	verify := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) { return parseUUID(keyID), nil }
	request.approveOwner(test, withVerifier(verify), http.StatusOK)

	got := storedSettings(test)
	if string(got["compact_prompt"]) != `"new"` || string(got["mermaid_orientation"]) != `"author"` {
		test.Fatalf("named keys not written: %v", got)
	}
	if _, present := got["github_wake_on_pr_merge"]; present {
		test.Fatalf("a null in the patch must remove the key: %v", got)
	}
	// Keys the patch does not name keep their stored value, allow-listed or not.
	for key, want := range map[string]string{"co_authored_by_enabled": `true`, "unlisted": `"keep"`, "delegated_governed_writes": `{"allowed_agents": ["a"]}`} {
		if string(got[key]) != want {
			test.Fatalf("unnamed key %s = %s, want %s", key, got[key], want)
		}
	}
	wantAfter := patchDigest(test, patch)
	if live := liveSettingsDigest(test, patchKeys(patch)...); live != wantAfter {
		test.Fatalf("stored named keys digest %s, want the agent's after_sha256 %s", live, wantAfter)
	}
	var raw string
	dbfx.QueryRow(test, `SELECT result::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result["readback_sha256"] != wantAfter {
		test.Fatalf("result %s (%v); want readback_sha256 %s", raw, err, wantAfter)
	}
	dbfx.QueryRow(test, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'config_change_applied'`, request.action.issueID).Scan(&raw)
	var details map[string]any
	if err := json.Unmarshal([]byte(raw), &details); err != nil {
		test.Fatal(err)
	}
	if details["field"] != "workspace.settings" || details["decision_action_id"] != request.action.actionID || details["credential_id"] != keyID || details["after_sha256"] != wantAfter {
		test.Fatalf("audit details %s", raw)
	}
}

func TestSettingsPatchMergesObjectKeys(test *testing.T) {
	setWorkspaceSettings(test, `{"idle_started_guard":{"mode":"shadow","after_hours":4,"statuses":["in_progress"]}}`)
	// The merged value is what is stored and digested: statuses survives.
	merged := map[string]any{"idle_started_guard": map[string]any{"mode": "act", "after_hours": 4, "statuses": []string{"in_progress"}}}
	request, _ := fileSettings(test, http.StatusCreated, map[string]any{
		"workspace_id": testWorkspaceID, "settings": map[string]any{"idle_started_guard": map[string]any{"mode": "act"}},
		"base_sha256": liveSettingsDigest(test, "idle_started_guard"), "after_sha256": patchDigest(test, merged),
	})
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	if got := liveSettingsDigest(test, "idle_started_guard"); got != patchDigest(test, merged) {
		test.Fatalf("stored guard %s is not the merge", storedSettings(test)["idle_started_guard"])
	}
}

func TestSettingsPatchRequestRefusals(test *testing.T) {
	ok := map[string]any{"compact_prompt": "new"}
	for _, tc := range []struct {
		name   string
		status int
		want   string
		patch  map[string]any
		edit   func(params map[string]any)
	}{
		{"denylisted key", http.StatusForbidden, "denylisted", map[string]any{"delegated_governed_writes": map[string]any{}}, nil},
		{"unknown key", http.StatusForbidden, "denylisted", map[string]any{"never_heard_of_it": 1}, nil},
		{"server-stamped pause key", http.StatusForbidden, "denylisted", map[string]any{"wakeups_paused_by": "x"}, nil},
		{"pause switch needs its own flow", http.StatusForbidden, "denylisted", map[string]any{"wakeups_paused": true}, nil},
		{"atomic: one denied key among allowed", http.StatusForbidden, "denylisted", map[string]any{"compact_prompt": "new", "delegated_governed_writes": 1}, nil},
		{"atomic: one invalid value among valid", http.StatusBadRequest, "invalid_params", map[string]any{"compact_prompt": "new", "github_wake_on_pr_merge": "yes"}, nil},
		{"compact_prompt wrong type", http.StatusBadRequest, "invalid_params", map[string]any{"compact_prompt": 5}, nil},
		{"mermaid_orientation outside enum", http.StatusBadRequest, "invalid_params", map[string]any{"mermaid_orientation": "sideways"}, nil},
		{"no keys", http.StatusBadRequest, "invalid_params", map[string]any{}, nil},
		{"agent after digest is wrong", http.StatusBadRequest, "after_digest_mismatch", ok, func(params map[string]any) { params["after_sha256"] = strings.Repeat("0", 64) }},
		{"agent base digest is stale", http.StatusConflict, "precondition_drift", ok, func(params map[string]any) { params["base_sha256"] = strings.Repeat("0", 64) }},
		{"workspace of another workspace", http.StatusBadRequest, "target_workspace_mismatch", ok, func(params map[string]any) { params["workspace_id"] = foreignWorkspaceID(test) }},
	} {
		test.Run(tc.name, func(test *testing.T) {
			_, body := requestSettings(test, tc.status, tc.patch, tc.edit)
			mustContain(test, body, tc.want)
			if got := storedSettings(test); string(got["compact_prompt"]) != `"old"` || string(got["github_wake_on_pr_merge"]) != `true` {
				test.Fatalf("a refused request changed settings: %v", got)
			}
		})
	}
}

func TestSettingsPatchApproveRefusals(test *testing.T) {
	patch := map[string]any{"compact_prompt": "new", "mermaid_orientation": "author"}
	test.Run("a named key drifted after the request", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, patch, nil)
		dbfx.Exec(test, `UPDATE workspace SET settings = jsonb_set(settings, '{compact_prompt}', '"edited"') WHERE id = $1`, testWorkspaceID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "precondition_drift")
		request.action.assertUntouched(test)
		if got := storedSettings(test); string(got["compact_prompt"]) != `"edited"` || got["mermaid_orientation"] != nil {
			test.Fatalf("a refused approval changed settings: %v", got)
		}
	})
	test.Run("an unnamed key changed after the request does not drift and is kept", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, patch, nil)
		dbfx.Exec(test, `UPDATE workspace SET settings = jsonb_set(settings, '{unlisted}', '"moved"') WHERE id = $1`, testWorkspaceID)
		request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
		if got := storedSettings(test); string(got["unlisted"]) != `"moved"` || string(got["compact_prompt"]) != `"new"` {
			test.Fatalf("settings after approval: %v", got)
		}
	})
	test.Run("denylisted key slipped into the stored params", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, patch, nil)
		dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, '{settings,delegated_governed_writes}', '{}') WHERE id = $1`, request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "payload_mismatch")
		request.assertNotApplied(test)
	})
	test.Run("altered payload value", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, patch, nil)
		dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, '{settings,compact_prompt}', '"other"') WHERE id = $1`, request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "payload_mismatch")
		request.assertNotApplied(test)
	})
	test.Run("altered preview digest", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, patch, nil)
		makeApproverOwner(test, request.action, "owner")
		claim := request.claim(test)
		claim["preview_sha256"] = strings.Repeat("0", 64)
		request.action.approve(test, withVerifier(acceptingVerifier), claim, http.StatusConflict)
		request.assertNotApplied(test)
	})
	test.Run("workspace of another workspace", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, patch, nil)
		dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, '{workspace_id}', to_jsonb($1::text)) WHERE id = $2`, foreignWorkspaceID(test), request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "target_workspace_mismatch")
		request.assertNotApplied(test)
	})
	test.Run("approver is not an owner", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, patch, nil)
		makeApproverOwner(test, request.action, "admin")
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.claim(test), http.StatusForbidden), "class_needs_owner")
		request.assertNotApplied(test)
	})
	test.Run("preview digest absent", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, patch, nil)
		makeApproverOwner(test, request.action, "owner")
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.action.claim(test, map[string]string{}), http.StatusConflict), "preview_required")
		request.assertNotApplied(test)
	})
	test.Run("no valid passkey assertion", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, patch, nil)
		request.action.enrollPasskey(test)
		makeApproverOwner(test, request.action, "owner")
		claim := request.claim(test)
		claim["assertion"] = nil
		request.action.approve(test, passkeyHandler(test), claim, http.StatusForbidden)
		request.assertNotApplied(test)
	})
	test.Run("expired", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, patch, nil)
		dbfx.Exec(test, `UPDATE decision_action SET expires_at = now() - interval '1 minute' WHERE id = $1`, request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "expired")
		request.assertNotApplied(test)
	})
	test.Run("second approve of the same action", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, patch, nil)
		handler := withVerifier(acceptingVerifier)
		request.approveOwner(test, handler, http.StatusOK)
		request.action.approve(test, handler, request.claim(test), http.StatusConflict)
		if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
			test.Fatalf("config_change_applied rows = %d after a replay, want 1", n)
		}
	})
}

func TestSettingsPatchConcurrentApprovalsApplyOnce(test *testing.T) {
	first, _ := requestSettings(test, http.StatusCreated, map[string]any{"compact_prompt": "new"}, nil)
	other := map[string]any{"compact_prompt": "other"}
	second, _ := fileSettings(test, http.StatusCreated, map[string]any{
		"workspace_id": testWorkspaceID, "settings": other, "base_sha256": first.base, "after_sha256": patchDigest(test, other),
	})
	makeApproverOwner(test, first.action, "owner")
	handler := withVerifier(acceptingVerifier)
	statuses := make([]int, 2)
	var group sync.WaitGroup
	for i, request := range []settingsRequest{first, second} {
		group.Add(1)
		go func() {
			defer group.Done()
			statuses[i] = approveStatus(test, handler, configRequest{action: request.action})
		}()
	}
	group.Wait()
	applied := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id IN ($1, $2) AND action = 'config_change_applied'`, first.action.issueID, second.action.issueID)
	if applied != 1 {
		test.Fatalf("applied %d times (statuses %v), want exactly 1", applied, statuses)
	}
}
