package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
	"github.com/multica-ai/multica/server/internal/service"
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
	patch := map[string]any{"compact_prompt": "new", "mermaid_orientation": "author", "github_wake_on_pr_merge": nil, "ask_guard_mode": "warn"}
	request, _ := requestSettings(test, http.StatusCreated, patch, nil)
	_, keyID := request.action.enrollPasskey(test)
	verify := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) { return parseUUID(keyID), nil }
	request.approveOwner(test, withVerifier(verify), http.StatusOK)

	got := storedSettings(test)
	if string(got["compact_prompt"]) != `"new"` || string(got["mermaid_orientation"]) != `"author"` || string(got["ask_guard_mode"]) != `"warn"` {
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
		{"pause switch is not a boolean", http.StatusBadRequest, "invalid_params", map[string]any{"wakeups_paused": "yes"}, nil},
		{"atomic: one denied key among allowed", http.StatusForbidden, "denylisted", map[string]any{"compact_prompt": "new", "delegated_governed_writes": 1}, nil},
		{"atomic: one invalid value among valid", http.StatusBadRequest, "invalid_params", map[string]any{"compact_prompt": "new", "github_wake_on_pr_merge": "yes"}, nil},
		{"compact_prompt wrong type", http.StatusBadRequest, "invalid_params", map[string]any{"compact_prompt": 5}, nil},
		{"mermaid_orientation outside enum", http.StatusBadRequest, "invalid_params", map[string]any{"mermaid_orientation": "sideways"}, nil},
		{"ask_guard_mode outside the modes", http.StatusBadRequest, "invalid_params", map[string]any{"ask_guard_mode": "bogus"}, nil},
		{"ask_guard_mode is not a string", http.StatusBadRequest, "invalid_params", map[string]any{"ask_guard_mode": true}, nil},
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

// approveRaw posts the approval and returns the status and body without
// failing the test, so concurrent callers can report both outcomes.
func (request settingsRequest) approveRaw(test *testing.T, handler *Handler) (int, string) {
	test.Helper()
	raw, err := json.Marshal(request.claim(test))
	if err != nil {
		test.Error(err)
		return 0, ""
	}
	call := request.action.decide("approve", request.action.humanID, "interactive_jwt")
	call.Body = readerCloser{bytes.NewReader(raw)}
	recorder := httptest.NewRecorder()
	handler.ApproveDecisionAction(recorder, call)
	return recorder.Code, recorder.Body.String()
}

func TestSettingsPatchConcurrentApprovalsApplyOnce(test *testing.T) {
	first, _ := requestSettings(test, http.StatusCreated, map[string]any{"compact_prompt": "new"}, nil)
	other := map[string]any{"compact_prompt": "other"}
	second, _ := fileSettings(test, http.StatusCreated, map[string]any{
		"workspace_id": testWorkspaceID, "settings": other, "base_sha256": first.base, "after_sha256": patchDigest(test, other),
	})
	makeApproverOwner(test, first.action, "owner")
	handler := withVerifier(acceptingVerifier)
	statuses, bodies := make([]int, 2), make([]string, 2)
	var group sync.WaitGroup
	for i, request := range []settingsRequest{first, second} {
		group.Add(1)
		go func() {
			defer group.Done()
			statuses[i], bodies[i] = request.approveRaw(test, handler)
		}()
	}
	group.Wait()
	applied := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id IN ($1, $2) AND action = 'config_change_applied'`, first.action.issueID, second.action.issueID)
	if applied != 1 {
		test.Fatalf("applied %d times (statuses %v), want exactly 1", applied, statuses)
	}
	winner := 0
	if statuses[1] == http.StatusOK {
		winner = 1
	}
	if statuses[winner] != http.StatusOK {
		test.Fatalf("statuses %v; one approval must succeed", statuses)
	}
	// The loser meets the busy try-lock or the winner's committed value. A busy
	// loser retries, and the retry must then drift.
	loser := []settingsRequest{first, second}[1-winner]
	if statuses[1-winner] == http.StatusConflict && strings.Contains(bodies[1-winner], "busy_retry") {
		statuses[1-winner], bodies[1-winner] = loser.approveRaw(test, handler)
	}
	if statuses[1-winner] != http.StatusConflict || !strings.Contains(bodies[1-winner], "precondition_drift") {
		test.Fatalf("loser got %d %s; want 409 precondition_drift", statuses[1-winner], bodies[1-winner])
	}
}

func TestSettingsPatchSecondRequestOnTheSameBaseDrifts(test *testing.T) {
	first, _ := requestSettings(test, http.StatusCreated, map[string]any{"compact_prompt": "new"}, nil)
	other := map[string]any{"compact_prompt": "other"}
	second, _ := fileSettings(test, http.StatusCreated, map[string]any{
		"workspace_id": testWorkspaceID, "settings": other, "base_sha256": first.base, "after_sha256": patchDigest(test, other),
	})
	handler := withVerifier(acceptingVerifier)
	first.approveOwner(test, handler, http.StatusOK)
	mustContain(test, second.approveOwner(test, handler, http.StatusConflict), "precondition_drift")
	if got := storedSettings(test); string(got["compact_prompt"]) != `"new"` {
		test.Fatalf("the losing approval changed settings: %v", got)
	}
}

func TestSettingsPatchApprovesWithARealPasskey(test *testing.T) {
	request, _ := requestSettings(test, http.StatusCreated, map[string]any{"compact_prompt": "new"}, nil)
	device, keyID := request.action.enrollPasskey(test)
	makeApproverOwner(test, request.action, "owner")
	handler := passkeyHandler(test)
	issued := request.action.issue(test, handler)
	claim := request.claim(test)
	claim["assertion"] = request.action.assertion(test, device, issued, passkeyStart+1)
	request.action.approve(test, handler, claim, http.StatusOK)
	if got := storedSettings(test); string(got["compact_prompt"]) != `"new"` {
		test.Fatalf("settings after a signed approval: %v", got)
	}
	var used string
	dbfx.QueryRow(test, `SELECT webauthn_credential_id::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&used)
	if used != keyID {
		test.Fatalf("recorded credential %s, want %s", used, keyID)
	}
	claim["assertion"] = request.action.assertion(test, device, issued, passkeyStart+2)
	request.action.approve(test, handler, claim, http.StatusConflict)
	if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
		test.Fatalf("config_change_applied rows = %d after a replay, want 1", n)
	}
}

func TestSettingsPatchRefusesWithoutAValidAssertion(test *testing.T) {
	handler := passkeyHandler(test)
	for name, assertion := range map[string]func(settingsRequest, *passkeytest.Authenticator, issuedChallenge) any{
		"missing": func(settingsRequest, *passkeytest.Authenticator, issuedChallenge) any { return nil },
		"signed for another challenge": func(request settingsRequest, device *passkeytest.Authenticator, issued issuedChallenge) any {
			issued.Options.PublicKey.Challenge = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			return request.action.assertion(test, device, issued, passkeyStart+1)
		},
	} {
		test.Run(name, func(test *testing.T) {
			request, _ := requestSettings(test, http.StatusCreated, map[string]any{"compact_prompt": "new"}, nil)
			device, _ := request.action.enrollPasskey(test)
			makeApproverOwner(test, request.action, "owner")
			issued := request.action.issue(test, handler)
			claim := request.claim(test)
			claim["assertion"] = assertion(request, device, issued)
			request.action.approve(test, handler, claim, http.StatusForbidden)
			request.assertNotApplied(test)
		})
	}
	test.Run("assertion issued for another action", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, map[string]any{"compact_prompt": "new"}, nil)
		request.action.enrollPasskey(test)
		makeApproverOwner(test, request.action, "owner")
		otherPatch := map[string]any{"compact_prompt": "other"}
		other, _ := fileSettings(test, http.StatusCreated, map[string]any{
			"workspace_id": testWorkspaceID, "settings": otherPatch, "base_sha256": request.base, "after_sha256": patchDigest(test, otherPatch),
		})
		otherDevice, _ := other.action.enrollPasskey(test)
		makeApproverOwner(test, other.action, "owner")
		issued := other.action.issue(test, handler)
		claim := request.claim(test)
		claim["assertion"] = other.action.assertion(test, otherDevice, issued, passkeyStart+1)
		request.action.approve(test, handler, claim, http.StatusForbidden)
		request.assertNotApplied(test)
	})
	test.Run("owner role revoked after the challenge", func(test *testing.T) {
		request, _ := requestSettings(test, http.StatusCreated, map[string]any{"compact_prompt": "new"}, nil)
		device, _ := request.action.enrollPasskey(test)
		makeApproverOwner(test, request.action, "owner")
		issued := request.action.issue(test, handler)
		dbfx.Exec(test, `UPDATE member SET role = 'member' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, request.action.humanID)
		claim := request.claim(test)
		claim["assertion"] = request.action.assertion(test, device, issued, passkeyStart+1)
		request.action.approve(test, handler, claim, http.StatusForbidden)
		request.assertNotApplied(test)
	})
}

// filePause stores the settings and files a patch of the pause switch.
func filePause(test *testing.T, seed string, patch, after map[string]any) settingsRequest {
	test.Helper()
	setWorkspaceSettings(test, seed)
	request, _ := fileSettings(test, http.StatusCreated, map[string]any{
		"workspace_id": testWorkspaceID, "settings": patch,
		"base_sha256": liveSettingsDigest(test, wakeupsPausedKey), "after_sha256": patchDigest(test, after),
	})
	return request
}

// approvePause files the patch and approves it as the owner.
func approvePause(test *testing.T, seed string, patch, after map[string]any) settingsRequest {
	test.Helper()
	request := filePause(test, seed, patch, after)
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	return request
}

func workspaceActivityCount(test *testing.T, action string) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE workspace_id = $1 AND action = $2`, testWorkspaceID, action)
}

func TestSettingsPatchPausesWithTheServerStamp(test *testing.T) {
	before := workspaceActivityCount(test, wakeupsPausedAction)
	request := approvePause(test, seededSettings, map[string]any{wakeupsPausedKey: true}, map[string]any{wakeupsPausedKey: true})
	got := storedSettings(test)
	if string(got[wakeupsPausedKey]) != `true` || string(got[wakeupsPausedByKey]) != `"`+request.action.humanID+`"` || len(got[wakeupsPausedAtKey]) == 0 {
		test.Fatalf("pause not stamped by the approver: %v", got)
	}
	if string(got["compact_prompt"]) != `"old"` {
		test.Fatalf("an unnamed key changed: %v", got)
	}
	if after := workspaceActivityCount(test, wakeupsPausedAction); after != before+1 {
		test.Fatalf("wakeups_paused activity rows %d, want %d", after, before+1)
	}
	test.Cleanup(func() {
		dbfx.Exec(test, `DELETE FROM activity_log WHERE workspace_id = $1 AND action = $2`, testWorkspaceID, wakeupsPausedAction)
	})
}

func TestSettingsPatchPauseOfAPausedWorkspaceKeepsTheStamp(test *testing.T) {
	seed := `{"wakeups_paused":true,"wakeups_paused_by":"earlier","wakeups_paused_at":"2026-01-01T00:00:00Z"}`
	before := workspaceActivityCount(test, wakeupsPausedAction)
	approvePause(test, seed, map[string]any{wakeupsPausedKey: true}, map[string]any{wakeupsPausedKey: true})
	got := storedSettings(test)
	if string(got[wakeupsPausedByKey]) != `"earlier"` || string(got[wakeupsPausedAtKey]) != `"2026-01-01T00:00:00Z"` {
		test.Fatalf("the stamp of the earlier pause changed: %v", got)
	}
	if after := workspaceActivityCount(test, wakeupsPausedAction); after != before {
		test.Fatalf("a pause that did not flip wrote an activity row")
	}
}

func TestSettingsPatchResumeClearsThePauseAndSettles(test *testing.T) {
	seed := `{"compact_prompt":"old","wakeups_paused":true,"wakeups_paused_by":"earlier","wakeups_paused_at":"2026-01-01T00:00:00Z"}`
	before := workspaceActivityCount(test, wakeupsResumedAction)
	// false and null are one resume: an unpaused workspace stores no switch.
	approvePause(test, seed, map[string]any{wakeupsPausedKey: false}, map[string]any{wakeupsPausedKey: nil})
	got := storedSettings(test)
	for _, key := range []string{wakeupsPausedKey, wakeupsPausedByKey, wakeupsPausedAtKey} {
		if _, present := got[key]; present {
			test.Fatalf("%s survived the resume: %v", key, got)
		}
	}
	if len(got[wakeupsResumedAtKey]) == 0 || string(got["compact_prompt"]) != `"old"` {
		test.Fatalf("resume did not settle or changed an unnamed key: %v", got)
	}
	if after := workspaceActivityCount(test, wakeupsResumedAction); after != before+1 {
		test.Fatalf("wakeups_resumed activity rows %d, want %d", after, before+1)
	}
	test.Cleanup(func() {
		dbfx.Exec(test, `DELETE FROM activity_log WHERE workspace_id = $1 AND action = $2`, testWorkspaceID, wakeupsResumedAction)
	})
}

func TestSettingsPatchPauseKeepsUnnamedNumbersExact(test *testing.T) {
	// 2^53+1 does not survive a float64 round trip.
	seed := `{"compact_prompt":"old","big":9007199254740993,"nested":{"ids":[9007199254740993,1.50],"deep":{"n":12345678901234567890}}}`
	approvePause(test, seed, map[string]any{wakeupsPausedKey: true}, map[string]any{wakeupsPausedKey: true})
	got := storedSettings(test)
	// jsonb normalizes whitespace but keeps numeric digits.
	if string(got["big"]) != `9007199254740993` || strings.ReplaceAll(string(got["nested"]), " ", "") != `{"ids":[9007199254740993,1.50],"deep":{"n":12345678901234567890}}` {
		test.Fatalf("unnamed values changed by the pause: big=%s nested=%s", got["big"], got["nested"])
	}
	if string(got[wakeupsPausedKey]) != `true` {
		test.Fatalf("pause not applied: %v", got)
	}
}

func TestSettingsPatchResumeSettlesHeldWakeups(test *testing.T) {
	seed := `{"wakeups_paused":true,"wakeups_paused_by":"earlier","wakeups_paused_at":"2026-01-01T00:00:00Z"}`
	request := filePause(test, seed, map[string]any{wakeupsPausedKey: false}, map[string]any{wakeupsPausedKey: nil})
	wakeup, err := (&service.IssueWakeupService{Tasks: testHandler.TaskService}).Create(context.Background(), parseUUID(request.action.issueID), parseUUID(testUserID), pgtype.UUID{}, service.WakeupInput{
		AgentID: request.action.agentID, Kind: "event", Mode: "once", EventTypes: []string{"comment.created"},
		Instruction: "Held while paused.",
	})
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Cleanup(test, `DELETE FROM issue_wakeup WHERE id = $1`, wakeup.ID)
	dbfx.Cleanup(test, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup.ID)
	dbfx.Exec(test, `INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload) VALUES (gen_random_uuid(), $1, 1, 'held-while-paused', 'comment.created', '{}'::jsonb)`, wakeup.ID)
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	var outcome string
	dbfx.QueryRow(test, `SELECT COALESCE(payload->>'outcome', '') FROM issue_wakeup_receipt WHERE wakeup_id = $1 AND processed_at IS NOT NULL`, wakeup.ID).Scan(&outcome)
	if outcome != "paused" {
		test.Fatalf("held receipt outcome %q, want it consumed as paused", outcome)
	}
	test.Cleanup(func() {
		dbfx.Exec(test, `DELETE FROM activity_log WHERE workspace_id = $1 AND action = $2`, testWorkspaceID, wakeupsResumedAction)
	})
}
