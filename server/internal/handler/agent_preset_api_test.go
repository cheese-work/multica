package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// presetCall runs one preset endpoint as the workspace owner.
func presetCall(t *testing.T, h http.HandlerFunc, method, path string, body any, params ...string) *testutil.Response {
	t.Helper()
	return testutil.Call(t, h, withURLParams(newRequest(method, path, body), params...))
}

type presetJSON struct {
	Name          string   `json:"name"`
	Model         *string  `json:"model"`
	ThinkingLevel *string  `json:"thinking_level"`
	ServiceTier   *string  `json:"service_tier"`
	ContextArgs   []string `json:"context_args"`
	IsDefault     bool     `json:"is_default"`
	Stale         string   `json:"stale"`
}

func createPresetVia(t *testing.T, agentID string, body map[string]any) *testutil.Response {
	t.Helper()
	return presetCall(t, testHandler.CreateAgentPreset, "POST", "/api/agents/"+agentID+"/presets", body, "id", agentID)
}

func patchPresetVia(t *testing.T, agentID, name, rawBody string) *testutil.Response {
	t.Helper()
	var body any
	if err := json.Unmarshal([]byte(rawBody), &body); err != nil {
		t.Fatalf("bad test body %q: %v", rawBody, err)
	}
	return presetCall(t, testHandler.UpdateAgentPreset, "PATCH", "/api/agents/"+agentID+"/presets/"+name, body, "id", agentID, "name", name)
}

func listPresetsVia(t *testing.T, agentID string) []presetJSON {
	t.Helper()
	r := presetCall(t, testHandler.ListAgentPresets, "GET", "/api/agents/"+agentID+"/presets", nil, "id", agentID).Want(http.StatusOK)
	var out []presetJSON
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode presets %s: %v", r.Body.String(), err)
	}
	return out
}

func presetByName(t *testing.T, agentID, name string) presetJSON {
	t.Helper()
	for _, p := range listPresetsVia(t, agentID) {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("preset %q not found", name)
	return presetJSON{}
}

func cleanupAgentPresets(t *testing.T, agentID string) {
	t.Helper()
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_preset WHERE agent_id = $1`, agentID) })
}

// I1: identity and permission fields are not preset fields.
func TestPresetAPI_I1_RejectsIdentityFields(t *testing.T) {
	requireDB(t)
	agentID, _, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	cleanupAgentPresets(t, agentID)
	createPresetVia(t, agentID, map[string]any{"name": "ok"}).Want(http.StatusCreated)

	for field, value := range map[string]any{
		"custom_args": []string{"--add-dir", "/x"}, "instructions": "do evil", "mcp_config": map[string]any{},
		"custom_env": map[string]string{"A": "1"}, "runtime_id": "00000000-0000-0000-0000-000000000000",
		"permission_mode": "public", "skills": []string{"s"},
	} {
		t.Run(field, func(t *testing.T) {
			createPresetVia(t, agentID, map[string]any{"name": "bad-" + field, field: value}).Want(http.StatusBadRequest)
			presetCall(t, testHandler.UpdateAgentPreset, "PATCH", "/api/agents/"+agentID+"/presets/ok",
				map[string]any{field: value}, "id", agentID, "name", "ok").Want(http.StatusBadRequest)
		})
	}
	if n := len(listPresetsVia(t, agentID)); n != 1 {
		t.Fatalf("%d presets after rejected writes, want 1", n)
	}
}

// I2 (HTTP): context_args carry only the provider's context-window args.
func TestPresetAPI_I2_RejectsUnsafeContextArgs(t *testing.T) {
	requireDB(t)
	agentID, _, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	cleanupAgentPresets(t, agentID)

	for i, args := range [][]string{
		{"--add-dir", "/x"}, {"--dangerously-skip-permissions"}, {"--mcp-config", "x.json"}, {"--permission-mode", "bypass"},
		{"--model", "m"}, {"-m", "m"}, {"--sandbox", "danger-full-access"}, {"--autocompact", "200k", "--add-dir", "/x"},
	} {
		createPresetVia(t, agentID, map[string]any{"name": "bad" + string(rune('a'+i)), "context_args": args}).Want(http.StatusBadRequest)
	}
	createPresetVia(t, agentID, map[string]any{"name": "big", "context_args": []string{"--autocompact", "200k"}}).Want(http.StatusCreated)
	patchPresetVia(t, agentID, "big", `{"context_args":["--permission-mode","x"]}`).Want(http.StatusBadRequest)

	// A provider with no allow-list takes none.
	dbfx.Exec(t, `UPDATE agent_runtime SET provider = 'gemini' WHERE id = (SELECT runtime_id FROM agent WHERE id = $1)`, agentID)
	createPresetVia(t, agentID, map[string]any{"name": "gem", "context_args": []string{"--autocompact", "200k"}}).Want(http.StatusBadRequest)
}

// Reserved and malformed names are refused; a duplicate is a conflict.
func TestPresetAPI_CreateNameRules(t *testing.T) {
	requireDB(t)
	agentID, _, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	cleanupAgentPresets(t, agentID)

	for _, name := range []string{"base", "reset", "Has Space", "", "-x"} {
		createPresetVia(t, agentID, map[string]any{"name": name}).Want(http.StatusBadRequest)
	}
	createPresetVia(t, agentID, map[string]any{"name": "fast", "model": "claude-sonnet-5-5", "thinking_level": "low"}).Want(http.StatusCreated)
	createPresetVia(t, agentID, map[string]any{"name": "fast"}).Want(http.StatusConflict)
	presetCall(t, testHandler.DeleteAgentPreset, "DELETE", "/api/agents/"+agentID+"/presets/fast", nil, "id", agentID, "name", "fast").Want(http.StatusNoContent)
	presetCall(t, testHandler.DeleteAgentPreset, "DELETE", "/api/agents/"+agentID+"/presets/fast", nil, "id", agentID, "name", "fast").Want(http.StatusNotFound)
}

// S4: PATCH is tri-state: omitted = unchanged, null = inherit, value = set.
func TestPresetAPI_S4_PatchIsTriState(t *testing.T) {
	requireDB(t)
	agentID, _, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	cleanupAgentPresets(t, agentID)
	createPresetVia(t, agentID, map[string]any{
		"name": "fast", "model": "claude-sonnet-5-5", "thinking_level": "low", "context_args": []string{"--autocompact", "200k"},
	}).Want(http.StatusCreated)

	patchPresetVia(t, agentID, "fast", `{"model": null}`).Want(http.StatusOK)
	p := presetByName(t, agentID, "fast")
	if p.Model != nil || str(p.ThinkingLevel) != "low" || !reflect.DeepEqual(p.ContextArgs, []string{"--autocompact", "200k"}) {
		t.Fatalf("after {model:null}: %+v (model must inherit, the rest unchanged)", p)
	}

	patchPresetVia(t, agentID, "fast", `{"thinking_level": "medium", "context_args": null}`).Want(http.StatusOK)
	p = presetByName(t, agentID, "fast")
	if p.Model != nil || str(p.ThinkingLevel) != "medium" || p.ContextArgs != nil {
		t.Fatalf("after thinking set + context_args null: %+v", p)
	}

	patchPresetVia(t, agentID, "fast", `{}`).Want(http.StatusOK)
	if q := presetByName(t, agentID, "fast"); !reflect.DeepEqual(q, p) {
		t.Fatalf("empty patch changed the preset: %+v -> %+v", p, q)
	}
	patchPresetVia(t, agentID, "nope", `{"model": null}`).Want(http.StatusNotFound)
}

func defaultPreset(t *testing.T, agentID string) string {
	t.Helper()
	var name *string
	dbfx.QueryRow(t, `SELECT (SELECT name FROM agent_preset WHERE agent_id = $1 AND is_default)`, agentID).Scan(&name)
	return str(name)
}

// S5: one default per agent; `base` as the default leaves none.
func TestPresetAPI_S5_DefaultIsExclusive(t *testing.T) {
	requireDB(t)
	agentID, _, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	cleanupAgentPresets(t, agentID)
	createPresetVia(t, agentID, map[string]any{"name": "a", "is_default": true}).Want(http.StatusCreated)
	createPresetVia(t, agentID, map[string]any{"name": "b"}).Want(http.StatusCreated)
	if got := defaultPreset(t, agentID); got != "a" {
		t.Fatalf("default = %q, want a", got)
	}

	patchPresetVia(t, agentID, "b", `{"is_default": true}`).Want(http.StatusOK)
	if got := defaultPreset(t, agentID); got != "b" {
		t.Fatalf("after PATCH b is_default: default = %q, want b", got)
	}
	createPresetVia(t, agentID, map[string]any{"name": "c", "is_default": true}).Want(http.StatusCreated)
	if got := defaultPreset(t, agentID); got != "c" {
		t.Fatalf("after create c default: default = %q, want c", got)
	}

	setDefault := func(name string) *testutil.Response {
		return presetCall(t, testHandler.SetAgentDefaultPreset, "PUT", "/api/agents/"+agentID+"/presets/default", map[string]any{"name": name}, "id", agentID)
	}
	setDefault("a").Want(http.StatusOK)
	if got := defaultPreset(t, agentID); got != "a" {
		t.Fatalf("PUT default a: default = %q", got)
	}
	setDefault("base").Want(http.StatusOK)
	if got := defaultPreset(t, agentID); got != "<NULL>" {
		t.Fatalf("PUT default base: default = %q, want none", got)
	}
	setDefault("missing").Want(http.StatusNotFound)
}

// E5: the agent update lists stale presets and is not blocked by them.
func TestPresetAPI_E5_AgentUpdateListsStalePresets(t *testing.T) {
	requireDB(t)
	agentID, _, runtimeID, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	cleanupAgentPresets(t, agentID)
	createPresetVia(t, agentID, map[string]any{"name": "big", "context_args": []string{"--autocompact", "200k"}}).Want(http.StatusCreated)
	createPresetVia(t, agentID, map[string]any{"name": "fine", "thinking_level": "low"}).Want(http.StatusCreated)
	dbfx.Exec(t, `UPDATE agent_runtime SET provider = 'codex' WHERE id = $1`, runtimeID)

	r := presetCall(t, testHandler.UpdateAgent, "PUT", "/api/agents/"+agentID, map[string]any{"description": "rebound"}, "id", agentID).Want(http.StatusOK)

	var resp struct {
		Description  string `json:"description"`
		StalePresets []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"stale_presets"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Description != "rebound" {
		t.Fatalf("update was blocked or lost: %s", r.Body.String())
	}
	if len(resp.StalePresets) == 0 || resp.StalePresets[0].Name != "big" || resp.StalePresets[0].Reason == "" {
		t.Fatalf("stale_presets = %+v, want big first with a reason (fine's validity depends on the provider enum)", resp.StalePresets)
	}
	for _, s := range resp.StalePresets {
		if s.Name == "fine" && s.Reason == "" {
			t.Fatalf("a listed stale preset needs a reason: %+v", s)
		}
	}
	if p := presetByName(t, agentID, "big"); p.Stale == "" {
		t.Fatalf("list should show big as stale, got %+v", p)
	}
}

// The composer asks for names to complete: base, reset and the agent's own
// presets; a caller who cannot invoke the agent gets nothing.
func TestPresetAPI_CompletionsListUsablePresetsOnly(t *testing.T) {
	requireDB(t)
	agentID, _, runtimeID, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	cleanupAgentPresets(t, agentID)
	createPresetVia(t, agentID, map[string]any{"name": "fast", "thinking_level": "low"}).Want(http.StatusCreated)
	createPresetVia(t, agentID, map[string]any{"name": "big", "context_args": []string{"--autocompact", "200k"}}).Want(http.StatusCreated)

	names := func() []string {
		r := presetCall(t, testHandler.AgentPresetCompletions, "GET", "/api/agents/"+agentID+"/presets/completions", nil, "id", agentID).Want(http.StatusOK)
		var out struct {
			Names []string `json:"names"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Names
	}
	if got, want := names(), []string{"base", "reset", "big", "fast"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}

	// A stale preset is not offered: choosing it would be skipped at claim.
	dbfx.Exec(t, `UPDATE agent_runtime SET provider = 'codex' WHERE id = $1`, runtimeID)
	if got, want := names(), []string{"base", "reset", "fast"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after rebind names = %v, want %v", got, want)
	}

	ownerID := seedSecurityTestOwner(t, "completion-owner")
	dbfx.Exec(t, `UPDATE agent SET permission_mode = 'private', owner_id = $2 WHERE id = $1`, agentID, ownerID)
	r := presetCall(t, testHandler.AgentPresetCompletions, "GET", "/api/agents/"+agentID+"/presets/completions", nil, "id", agentID).Want(http.StatusForbidden)
	if strings.Contains(r.Body.String(), "fast") || strings.Contains(r.Body.String(), "big") {
		t.Fatalf("a denied caller learned preset names: %s", r.Body.String())
	}
}

func issueLabelNames(t *testing.T, issueID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT l.name FROM issue_to_label il JOIN issue_label l ON l.id = il.label_id
		WHERE il.issue_id = $1 ORDER BY l.name`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// S1: selecting a preset on an issue replaces every preset:* label in one step
// and leaves other labels alone.
func TestPresetAPI_S1_IssuePresetReplacesLabels(t *testing.T) {
	requireDB(t)
	issue := createIssueForTest(t, map[string]any{"title": "preset label replace"})
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issue.ID) })
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_label WHERE workspace_id = $1 AND name LIKE 'preset:%'`, testWorkspaceID)
	})
	other := createTestIssueLabel(t, "unrelated-s1")
	dbfx.Exec(t, `INSERT INTO issue_to_label (issue_id, label_id) VALUES ($1, $2)`, issue.ID, other)

	use := func(name string) {
		presetCall(t, testHandler.SetIssuePreset, "PUT", "/api/issues/"+issue.ID+"/preset", map[string]any{"name": name}, "id", issue.ID).Want(http.StatusOK)
	}
	use("200k")
	use("fast")
	if got, want := issueLabelNames(t, issue.ID), []string{"preset:fast", "unrelated-s1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	use("base")
	if got, want := issueLabelNames(t, issue.ID), []string{"preset:base", "unrelated-s1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	presetCall(t, testHandler.SetIssuePreset, "PUT", "/api/issues/"+issue.ID+"/preset", map[string]any{"name": "Bad Name"}, "id", issue.ID).Want(http.StatusBadRequest)
	presetCall(t, testHandler.ClearIssuePreset, "DELETE", "/api/issues/"+issue.ID+"/preset", nil, "id", issue.ID).Want(http.StatusOK)
	if got, want := issueLabelNames(t, issue.ID), []string{"unrelated-s1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after clear labels = %v, want %v", got, want)
	}
}

// update and delete complete from the stored presets: a stale one is still
// listed for repair, and a manager who cannot invoke the agent still sees it.
func TestPresetAPI_ListKeepsStalePresetsForManagerWithoutInvoke(t *testing.T) {
	requireDB(t)
	agentID, _, runtimeID, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	cleanupAgentPresets(t, agentID)
	createPresetVia(t, agentID, map[string]any{"name": "big", "context_args": []string{"--autocompact", "200k"}}).Want(http.StatusCreated)
	createPresetVia(t, agentID, map[string]any{"name": "fast", "thinking_level": "low"}).Want(http.StatusCreated)
	dbfx.Exec(t, `UPDATE agent_runtime SET provider = 'codex' WHERE id = $1`, runtimeID)
	ownerID := seedSecurityTestOwner(t, "list-owner")
	dbfx.Exec(t, `UPDATE agent SET permission_mode = 'private', owner_id = $2 WHERE id = $1`, agentID, ownerID)
	presetCall(t, testHandler.AgentPresetCompletions, "GET", "/api/agents/"+agentID+"/presets/completions", nil, "id", agentID).Want(http.StatusForbidden)

	var names []string
	for _, p := range listPresetsVia(t, agentID) {
		names = append(names, p.Name)
	}
	if want := []string{"big", "fast"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("stored presets = %v, want %v", names, want)
	}
	if got := presetByName(t, agentID, "big").Stale; got == "" {
		t.Fatal("big should be reported stale after the rebind")
	}
}

// Concurrent selections serialize on the issue, so exactly one preset label survives.
func TestPresetAPI_ConcurrentSelectionsLeaveOneLabel(t *testing.T) {
	requireDB(t)
	issue := createIssueForTest(t, map[string]any{"title": "preset label race"})
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issue.ID) })
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_label WHERE workspace_id = $1 AND name LIKE 'preset:%'`, testWorkspaceID)
	})

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := withURLParams(newRequest("PUT", "/api/issues/"+issue.ID+"/preset", map[string]any{"name": fmt.Sprintf("p%d", i)}), "id", issue.ID)
			w := httptest.NewRecorder()
			testHandler.SetIssuePreset(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("PUT p%d = %d: %s", i, w.Code, w.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()

	var presets []string
	for _, name := range issueLabelNames(t, issue.ID) {
		if strings.HasPrefix(name, "preset:") {
			presets = append(presets, name)
		}
	}
	if len(presets) != 1 {
		t.Fatalf("preset labels after %d concurrent selections = %v, want exactly one", n, presets)
	}
}

// R7: cost by preset sums task_usage per task, grouped by the preset the run was claimed under.
func TestPresetAPI_R7_UsageByPreset(t *testing.T) {
	requireDB(t)
	agentID, sessionID, _, _ := presetAgent(t, "claude", "claude-opus-5-5", "high")
	for _, run := range []struct {
		preset      string
		in, out, ct int64
	}{{"fast", 100, 10, 5}, {"fast", 200, 20, 7}, {"base", 1000, 100, 50}} {
		taskID := queueArchiveTestTask(t, agentID, sessionID, "completed")
		dbfx.Exec(t, `UPDATE agent_task_queue SET preset_name = $2 WHERE id = $1`, taskID, run.preset)
		dbfx.Exec(t, `INSERT INTO task_usage (task_id, provider, model, input_tokens, output_tokens, cost_usd_ticks) VALUES ($1, 'claude', 'm', $2, $3, $4)`,
			taskID, run.in, run.out, run.ct)
	}

	r := presetCall(t, testHandler.AgentPresetUsage, "GET", "/api/agents/"+agentID+"/presets/usage", nil, "id", agentID).Want(http.StatusOK)
	var rows []struct {
		PresetName   string `json:"preset_name"`
		Tasks        int64  `json:"tasks"`
		InputTokens  int64  `json:"input_tokens"`
		OutputTokens int64  `json:"output_tokens"`
		CostUSDTicks int64  `json:"cost_usd_ticks"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	got := map[string][4]int64{}
	for _, row := range rows {
		got[row.PresetName] = [4]int64{row.Tasks, row.InputTokens, row.OutputTokens, row.CostUSDTicks}
	}
	want := map[string][4]int64{"fast": {2, 300, 30, 12}, "base": {1, 1000, 100, 50}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage by preset = %v, want %v (body %s)", got, want, r.Body.String())
	}
}
