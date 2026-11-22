package handler

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-8819 (plan r9 S3b): no writer may move a workspace into the gap state
// (agent_cannot_do_mode warn or block with no agent_cannot_do key).

const (
	gapList   = `[{"key":"k","tool":"gh","subcommand":["pr","merge"],"human_step":"Merge it."}]`
	gapFixHit = "needs an explicit `agent_cannot_do` list"
)

func gapSettings(mode, list string, extra ...string) string {
	parts := append([]string(nil), extra...)
	if mode != "" {
		parts = append(parts, `"agent_cannot_do_mode":"`+mode+`"`)
	}
	if list != "" {
		parts = append(parts, `"agent_cannot_do":`+list)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func asMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWorkspaceSaveRefusesWritesThatCreateTheGapState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stored   string
		incoming string
		status   int
	}{
		{"warn with no list", `{}`, gapSettings("warn", ""), http.StatusBadRequest},
		{"block with no list", `{"unlisted":1}`, gapSettings("block", "", `"unlisted":1`), http.StatusBadRequest},
		{"remove the list while block", gapSettings("block", gapList), gapSettings("block", ""), http.StatusBadRequest},
		{"remove the list while warn", gapSettings("warn", gapList), gapSettings("warn", ""), http.StatusBadRequest},
		{"turn on warn over a stored list-less off", gapSettings("off", ""), gapSettings("warn", ""), http.StatusBadRequest},
		{"warn with a list", `{}`, gapSettings("warn", gapList), http.StatusOK},
		{"warn with an empty list", `{}`, gapSettings("warn", `[]`), http.StatusOK},
		{"off with no list", `{}`, gapSettings("off", ""), http.StatusOK},
		{"remove the list while off", gapSettings("off", gapList), gapSettings("off", ""), http.StatusOK},
		{"no mode, no list", gapSettings("block", gapList), `{"unlisted":1}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setWorkspaceSettings(t, tc.stored)
			res := ownerCatalogPatch(t, testUserID, asMap(t, tc.incoming)).Want(tc.status)
			if tc.status == http.StatusBadRequest {
				if body := res.Body.String(); !strings.Contains(body, gapFixHit) || !strings.Contains(body, "set the list first, or set the mode to off") {
					t.Fatalf("400 body lacks the fix text: %s", body)
				}
			}
		})
	}
}

func TestWorkspaceSaveKeepsAGapWorkspaceWritable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		incoming string
	}{
		{"changes another key and keeps the gap", gapSettings("warn", "", `"unlisted":2`)},
		{"adds a list", gapSettings("warn", gapList)},
		{"sets off", gapSettings("off", "")},
		{"moves warn to block and keeps the gap", gapSettings("block", "")},
		{"drops the mode", `{"unlisted":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setWorkspaceSettings(t, gapSettings("warn", "", `"unlisted":1`))
			ownerCatalogPatch(t, testUserID, asMap(t, tc.incoming)).Want(http.StatusOK)
		})
	}
}

func TestWorkspaceSaveGapRefusalLeavesOwnerOnlyRuleFirst(t *testing.T) {
	setWorkspaceSettings(t, gapSettings("block", gapList))
	adminID := createPlainMember(t, "che8819-admin-"+safeTestName(t)+"@multica.test")
	dbfx.Exec(t, `UPDATE member SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, adminID)
	// An admin who drops the list is still told the list is the owner's (403, not 400).
	ownerCatalogPatch(t, adminID, asMap(t, gapSettings("block", ""))).Want(http.StatusForbidden)
}

func TestWorkspacePausePatchKeepsAGapWorkspaceWritable(t *testing.T) {
	setWorkspaceSettings(t, gapSettings("warn", ""))
	req := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{"wakeups_paused": true}), "id", testWorkspaceID)
	req.Header.Set("X-User-ID", testUserID)
	testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusOK)
}

func TestSettingsPatchRefusesRequestThatCreatesTheGapState(t *testing.T) {
	setWorkspaceSettings(t, `{"unlisted":"keep"}`)
	patch := map[string]any{"agent_cannot_do_mode": "warn"}
	params := map[string]any{
		"workspace_id": testWorkspaceID, "settings": patch,
		"base_sha256": liveSettingsDigest(t, "agent_cannot_do_mode"), "after_sha256": patchDigest(t, patch),
	}
	_, body := fileSettings(t, http.StatusBadRequest, params)
	mustContain(t, body, gapFixHit)
}

func TestSettingsPatchRefusesRemovingTheListWhileEnforcing(t *testing.T) {
	setWorkspaceSettings(t, gapSettings("block", gapList))
	patch := map[string]any{"agent_cannot_do": nil}
	params := map[string]any{
		"workspace_id": testWorkspaceID, "settings": patch,
		"base_sha256": liveSettingsDigest(t, "agent_cannot_do"), "after_sha256": patchDigest(t, patch),
	}
	_, body := fileSettings(t, http.StatusBadRequest, params)
	mustContain(t, body, gapFixHit)
}

func TestSettingsPatchRefusesAtApprovalWhenAnUnnamedKeyMovedIntoTheGap(t *testing.T) {
	// The patch names only the mode; the list is unnamed, so removing it after
	// the request does not drift the base digest. The apply must still refuse.
	setWorkspaceSettings(t, gapSettings("off", gapList))
	patch := map[string]any{"agent_cannot_do_mode": "block"}
	params := map[string]any{
		"workspace_id": testWorkspaceID, "settings": patch,
		"base_sha256": liveSettingsDigest(t, "agent_cannot_do_mode"), "after_sha256": patchDigest(t, patch),
	}
	request, _ := fileSettings(t, http.StatusCreated, params)
	dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'agent_cannot_do' WHERE id = $1`, testWorkspaceID)
	body := request.approveOwner(t, withVerifier(acceptingVerifier), http.StatusBadRequest)
	mustContain(t, body, gapFixHit)
	if got := storedSettings(t); string(got["agent_cannot_do_mode"]) != `"off"` {
		t.Fatalf("a refused approval changed the mode: %v", got)
	}
}

func TestSettingsPatchKeepsAGapWorkspaceWritable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch map[string]any
		want  string // mode after the approval
	}{
		{"another key", map[string]any{"compact_prompt": "new"}, `"warn"`},
		{"adds a list", map[string]any{"agent_cannot_do": []any{map[string]any{"key": "k", "tool": "gh", "human_step": "x"}}}, `"warn"`},
		{"sets off", map[string]any{"agent_cannot_do_mode": "off"}, `"off"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setWorkspaceSettings(t, gapSettings("warn", ""))
			keys := patchKeys(tc.patch)
			params := map[string]any{
				"workspace_id": testWorkspaceID, "settings": tc.patch,
				"base_sha256": liveSettingsDigest(t, keys...), "after_sha256": patchDigest(t, tc.patch),
			}
			request, _ := fileSettings(t, http.StatusCreated, params)
			request.approveOwner(t, withVerifier(acceptingVerifier), http.StatusOK)
			if got := storedSettings(t); string(got["agent_cannot_do_mode"]) != tc.want {
				t.Fatalf("mode after approval = %s, want %s", got["agent_cannot_do_mode"], tc.want)
			}
		})
	}
}

func TestCannotDoGap(t *testing.T) {
	for _, tc := range []struct {
		settings string
		want     bool
	}{
		{`{}`, false},
		{`{"agent_cannot_do_mode":"off"}`, false},
		{`{"agent_cannot_do_mode":"warn"}`, true},
		{`{"agent_cannot_do_mode":"block"}`, true},
		{`{"agent_cannot_do_mode":"block","agent_cannot_do":[]}`, false},
		{`{"agent_cannot_do_mode":"warn","agent_cannot_do":` + gapList + `}`, false},
		{`{"agent_cannot_do":` + gapList + `}`, false},
		{`{"agent_cannot_do_mode":"bogus"}`, false}, // not a mode: validation refuses it elsewhere
	} {
		if got := cannotDoGap(asMap(t, tc.settings)); got != tc.want {
			t.Errorf("cannotDoGap(%s) = %v, want %v", tc.settings, got, tc.want)
		}
	}
	if cannotDoGap(nil) {
		t.Error("cannotDoGap(nil) = true")
	}
}

// The gap refusal covers every writer only while no other writer names the two
// keys. A new writer must call cannotDoGap and join this list.
func TestOnlyKnownFilesNameTheCannotDoSettings(t *testing.T) {
	allowed := map[string]string{
		"internal/decisionaction/config.go":                  "allow table of the settings patch",
		"internal/decisiontx/ask.go":                         "comment on the AgentSurface field",
		"internal/decisiontx/ask_cannot_do.go":               "reader and key constants",
		"cmd/multica/cmd_decision.go":                        "help text of the decision request flag",
		"internal/handler/decision_action_settings_patch.go": "settings patch: validation and the gap check",
		"internal/handler/agent_cannot_do.go":                "validation, owner-only rule and the gap helper",
	}
	root := filepath.Join("..", "..")
	var found []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == "migrations" || strings.HasPrefix(rel, "migrations/") || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, name := range []string{"agent_cannot_do", "SettingCannotDo"} {
			if strings.Contains(text, name) {
				found = append(found, rel)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)
	for _, rel := range found {
		if _, ok := allowed[rel]; !ok {
			t.Errorf("%s names agent_cannot_do or agent_cannot_do_mode: route its writes through cannotDoGap and add it to this list", rel)
		}
	}
	for rel := range allowed {
		if !slices.Contains(found, rel) {
			t.Errorf("%s no longer names the settings: remove it from the list", rel)
		}
	}
}
