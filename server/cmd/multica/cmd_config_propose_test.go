package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

const (
	proposeAgentID    = "11111111-1111-4111-8111-111111111111"
	proposeSquadID    = "22222222-2222-4222-8222-222222222222"
	proposeWorkflowID = "33333333-3333-4333-8333-333333333333"
)

func sha(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func newConfigProposeTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "propose"}
	c.Flags().String("file", "", "")
	c.Flags().String("out", "", "")
	c.Flags().Bool("dry-run", false, "")
	c.Flags().String("status-key", "", "")
	c.Flags().StringArray("set", nil, "")
	c.Flags().Int("expires-in", 0, "")
	c.Flags().Int("max-runs", 0, "")
	c.Flags().Bool("include-child-prs", false, "")
	return c
}

// proposeServer answers the live GETs from routes and records every request
// path, so a test can prove a refused proposal read nothing.
func proposeServer(t *testing.T, routes map[string]any) *[]string {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		body, ok := routes[r.URL.Path]
		if !ok || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	return &seen
}

type proposeAction struct {
	Type    string                     `json:"type"`
	Version int                        `json:"version"`
	Params  map[string]json.RawMessage `json:"params"`
	Expires int64                      `json:"expires_in_seconds"`
}

func (a proposeAction) str(t *testing.T, key string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(a.Params[key], &value); err != nil {
		t.Fatalf("param %s = %s: %v", key, a.Params[key], err)
	}
	return value
}

func writeInput(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// propose runs the command with the flags and returns stdout. It fails the test
// on error unless wantErr.
func propose(t *testing.T, flags map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd := newConfigProposeTestCmd()
	for name, value := range flags {
		if name == "set" {
			for _, pair := range strings.Split(value, "\x00") {
				_ = cmd.Flags().Set("set", pair)
			}
			continue
		}
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	return captureStdout(t, func() error { return runConfigPropose(cmd, args) })
}

func readAction(t *testing.T, path string) proposeAction {
	t.Helper()
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The file must pass the same envelope check `decision request` applies.
	if _, err := readDecisionAction(path); err != nil {
		t.Fatalf("action file refused by decision request: %v", err)
	}
	var action proposeAction
	if err := json.Unmarshal(text, &action); err != nil {
		t.Fatal(err)
	}
	return action
}

func TestProposeAgentInstructionsWritesTheActionFileWithBothDigests(t *testing.T) {
	seen := proposeServer(t, map[string]any{"/api/agents/" + proposeAgentID: map[string]any{"instructions": "old text"}})
	out := filepath.Join(t.TempDir(), "action.json")
	stdout, err := propose(t, map[string]string{"file": writeInput(t, "new text"), "out": out}, "agent.instructions", proposeAgentID)
	if err != nil {
		t.Fatal(err)
	}
	action := readAction(t, out)
	if action.Type != "agent.instructions.set" || action.Version != 1 {
		t.Fatalf("action = %+v", action)
	}
	if action.str(t, "agent_id") != proposeAgentID || action.str(t, "instructions") != "new text" {
		t.Fatalf("params = %s", action.Params)
	}
	if action.str(t, "base_sha256") != sha("old text") || action.str(t, "after_sha256") != sha("new text") {
		t.Fatalf("digests = %s / %s", action.Params["base_sha256"], action.Params["after_sha256"])
	}
	if !strings.Contains(stdout, sha("old text")) || !strings.Contains(stdout, sha("new text")) {
		t.Errorf("stdout lacks the digests: %s", stdout)
	}
	if len(*seen) != 1 {
		t.Errorf("requests = %v, want one GET", *seen)
	}
}

func TestProposeDryRunPrintsDigestsAndActionAndWritesNothing(t *testing.T) {
	proposeServer(t, map[string]any{"/api/squads/" + proposeSquadID: map[string]any{"instructions": "old"}})
	dir := t.TempDir()
	t.Chdir(dir)
	stdout, err := propose(t, map[string]string{"file": writeInput(t, "new"), "dry-run": "true"}, "squad.instructions", proposeSquadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{sha("old"), sha("new"), `"type": "squad.instructions.set"`, `"squad_id": "` + proposeSquadID + `"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, stdout)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("dry run wrote files: %v", entries)
	}
}

func TestProposeWorkspaceContextUsesTheCurrentWorkspaceAndReadsNullAsEmpty(t *testing.T) {
	proposeServer(t, map[string]any{"/api/workspaces/ws-1": map[string]any{"context": nil}})
	out := filepath.Join(t.TempDir(), "action.json")
	if _, err := propose(t, map[string]string{"file": writeInput(t, "ctx"), "out": out}, "workspace.context"); err != nil {
		t.Fatal(err)
	}
	action := readAction(t, out)
	if action.Type != "workspace.context.set" || action.str(t, "workspace_id") != "ws-1" || action.str(t, "context") != "ctx" {
		t.Fatalf("action = %+v params %s", action, action.Params)
	}
	if action.str(t, "base_sha256") != sha("") {
		t.Errorf("a NULL context must hash as empty, got %s", action.Params["base_sha256"])
	}
}

func TestProposeWorkflowStepsHashesTheGETShapeWithTheServerRecipe(t *testing.T) {
	live := json.RawMessage(`[{"status_key":"todo","handler":{"type":"agent","id":"a"},"instructions":"x"}]`)
	proposeServer(t, map[string]any{"/api/issue-workflows/" + proposeWorkflowID: map[string]any{"id": proposeWorkflowID, "steps": live}})
	next := `[{"status_key":"todo","handler":{"type":"agent","id":"a"},"instructions":"y <z>"}]`
	out := filepath.Join(t.TempDir(), "action.json")
	if _, err := propose(t, map[string]string{"file": writeInput(t, next), "out": out}, "workflow.steps", proposeWorkflowID); err != nil {
		t.Fatal(err)
	}
	action := readAction(t, out)
	wantBase, _ := decisionaction.WorkflowDigest(proposeWorkflowID, live)
	wantAfter, _ := decisionaction.WorkflowDigest(proposeWorkflowID, json.RawMessage(next))
	if action.Type != "workflow.steps.set" || action.str(t, "base_sha256") != wantBase || action.str(t, "after_sha256") != wantAfter {
		t.Fatalf("action = %+v params %s", action, action.Params)
	}
	var got, want any
	_ = json.Unmarshal(action.Params["steps"], &got)
	_ = json.Unmarshal([]byte(next), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %s, want the file's steps", action.Params["steps"])
	}
}

func TestProposeWorkflowStepInstructionsChangesOnlyTheNamedStep(t *testing.T) {
	live := json.RawMessage(`[{"status_key":"todo","handler":{"type":"agent","id":"a"},"instructions":"a"},{"status_key":"review","handler":{"type":"agent","id":"a"},"instructions":"b"}]`)
	proposeServer(t, map[string]any{"/api/issue-workflows/" + proposeWorkflowID: map[string]any{"steps": live}})
	flags := map[string]string{"file": writeInput(t, "B2"), "status-key": "review", "out": filepath.Join(t.TempDir(), "a.json")}
	if _, err := propose(t, flags, "workflow.step.instructions", proposeWorkflowID); err != nil {
		t.Fatal(err)
	}
	action := readAction(t, flags["out"])
	wantBase, _ := decisionaction.WorkflowDigest(proposeWorkflowID, live)
	wantAfter, _ := decisionaction.WorkflowDigest(proposeWorkflowID, json.RawMessage(`[{"status_key":"todo","handler":{"type":"agent","id":"a"},"instructions":"a"},{"status_key":"review","handler":{"type":"agent","id":"a"},"instructions":"B2"}]`))
	if action.Type != "workflow.step.instructions.set" || action.str(t, "status_key") != "review" || action.str(t, "instructions") != "B2" || action.str(t, "base_sha256") != wantBase {
		t.Fatalf("action = %+v params %s", action, action.Params)
	}
	// The server computes the after digest for this action; the CLI only shows it.
	if _, sent := action.Params["after_sha256"]; sent {
		t.Error("workflow.step.instructions.set takes no after_sha256")
	}
	stdout, _ := propose(t, map[string]string{"file": flags["file"], "status-key": "review", "dry-run": "true"}, "workflow.step.instructions", proposeWorkflowID)
	if !strings.Contains(stdout, wantAfter) {
		t.Errorf("dry run lacks the after digest %s:\n%s", wantAfter, stdout)
	}
	if _, err := propose(t, map[string]string{"file": flags["file"], "status-key": "nope", "dry-run": "true"}, "workflow.step.instructions", proposeWorkflowID); err == nil {
		t.Error("an unknown status key was accepted")
	}
}

func TestProposeSettingsPatchDigestsTheNamedKeysOnly(t *testing.T) {
	workspace := map[string]any{"settings": map[string]any{"compact_prompt": "old", "wakeups_paused": true, "auto_compact": map[string]any{"a": 1, "b": 2}, "unrelated": "x"}}
	proposeServer(t, map[string]any{"/api/workspaces/ws-1": workspace})
	out := filepath.Join(t.TempDir(), "action.json")
	flags := map[string]string{"out": out, "set": strings.Join([]string{"compact_prompt=new", "wakeups_paused:=false", `auto_compact:={"b":3}`, "mermaid_orientation:=null"}, "\x00")}
	if _, err := propose(t, flags, "workspace.settings"); err != nil {
		t.Fatal(err)
	}
	action := readAction(t, out)
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	wantBase, _ := decisionaction.SettingsDigest("ws-1", map[string]json.RawMessage{
		"compact_prompt": raw(`"old"`), "wakeups_paused": raw(`true`), "auto_compact": raw(`{"a":1,"b":2}`), "mermaid_orientation": nil})
	// An unpaused workspace stores no switch, an object key keeps its unnamed fields.
	wantAfter, _ := decisionaction.SettingsDigest("ws-1", map[string]json.RawMessage{
		"compact_prompt": raw(`"new"`), "wakeups_paused": nil, "auto_compact": raw(`{"a":1,"b":3}`), "mermaid_orientation": nil})
	if action.Type != "workspace.settings.patch" || action.str(t, "workspace_id") != "ws-1" || action.str(t, "base_sha256") != wantBase || action.str(t, "after_sha256") != wantAfter {
		t.Fatalf("action = %+v params %s", action, action.Params)
	}
	var settings map[string]json.RawMessage
	_ = json.Unmarshal(action.Params["settings"], &settings)
	if len(settings) != 4 || string(settings["compact_prompt"]) != `"new"` || string(settings["wakeups_paused"]) != `false` {
		t.Errorf("settings = %s", action.Params["settings"])
	}
}

func TestProposeRefusesDeniedFieldsAndKeysBeforeReadingAnything(t *testing.T) {
	seen := proposeServer(t, map[string]any{})
	input := writeInput(t, "x")
	for name, tc := range map[string]struct {
		flags map[string]string
		args  []string
	}{
		"project description":   {map[string]string{"file": input}, []string{"project.description", proposeAgentID}},
		"unknown field":         {map[string]string{"file": input}, []string{"member.role", proposeAgentID}},
		"delegation switch":     {map[string]string{"set": "delegated_governed_writes:=true"}, []string{"workspace.settings"}},
		"key not on the table":  {map[string]string{"set": "decision_gate:=false"}, []string{"workspace.settings"}},
		"mixed allowed+denied":  {map[string]string{"set": "compact_prompt=x\x00repos:=[]"}, []string{"workspace.settings"}},
		"settings without keys": {map[string]string{}, []string{"workspace.settings"}},
	} {
		t.Run(name, func(t *testing.T) {
			flags := tc.flags
			flags["out"] = filepath.Join(t.TempDir(), "a.json")
			_, err := propose(t, flags, tc.args...)
			if err == nil || !strings.Contains(err.Error(), "denylisted") {
				t.Fatalf("err = %v, want a denylisted refusal", err)
			}
			if _, statErr := os.Stat(flags["out"]); statErr == nil {
				t.Error("an action file was written for a refused change")
			}
		})
	}
	if len(*seen) != 0 {
		t.Errorf("refused proposals read %v", *seen)
	}
}

func TestProposeRejectsBadInputWithoutWriting(t *testing.T) {
	proposeServer(t, map[string]any{"/api/agents/" + proposeAgentID: map[string]any{"instructions": "old"}})
	out := filepath.Join(t.TempDir(), "a.json")
	for name, tc := range map[string]struct {
		flags map[string]string
		args  []string
	}{
		"no file":         {map[string]string{}, []string{"agent.instructions", proposeAgentID}},
		"not a uuid":      {map[string]string{"file": writeInput(t, "x")}, []string{"agent.instructions", "nope"}},
		"no target":       {map[string]string{"file": writeInput(t, "x")}, []string{"agent.instructions"}},
		"nul byte":        {map[string]string{"file": writeInput(t, "a\x00b")}, []string{"agent.instructions", proposeAgentID}},
		"no out, no dry":  {map[string]string{"file": writeInput(t, "x")}, []string{"agent.instructions", proposeAgentID}},
		"steps not array": {map[string]string{"file": writeInput(t, `{}`), "out": out}, []string{"workflow.steps", proposeWorkflowID}},
		"set with file":   {map[string]string{"file": writeInput(t, "x"), "set": "compact_prompt=x", "out": out}, []string{"workspace.settings"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := propose(t, tc.flags, tc.args...); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a file was written for bad input")
	}
}

func TestProposeCarriesTheExpiryWhenAsked(t *testing.T) {
	proposeServer(t, map[string]any{"/api/agents/" + proposeAgentID: map[string]any{"instructions": "old"}})
	out := filepath.Join(t.TempDir(), "a.json")
	if _, err := propose(t, map[string]string{"file": writeInput(t, "new"), "out": out, "expires-in": "7200"}, "agent.instructions", proposeAgentID); err != nil {
		t.Fatal(err)
	}
	if action := readAction(t, out); action.Expires != 7200 {
		t.Errorf("expires_in_seconds = %d", action.Expires)
	}
}

// The server trims a step's instructions (and clears them on a handler-less
// step) before it hashes the stored steps, so the digest the CLI shows must be
// the digest of that result, not of the raw file.
func TestProposeWorkflowStepsHashesTheServerNormalizedInstructions(t *testing.T) {
	live := json.RawMessage(`[{"status_key":"todo","handler":{"type":"agent","id":"a"},"instructions":"x"},{"status_key":"done","handler":{"type":"none"},"instructions":""}]`)
	proposeServer(t, map[string]any{"/api/issue-workflows/" + proposeWorkflowID: map[string]any{"steps": live}})
	raw := `[{"status_key":"todo","handler":{"type":"agent","id":"a"},"instructions":"  y\n\n"},{"status_key":"done","handler":{"type":"none"},"instructions":"dropped"}]`
	normalized := `[{"status_key":"todo","handler":{"type":"agent","id":"a"},"instructions":"y"},{"status_key":"done","handler":{"type":"none"},"instructions":""}]`
	out := filepath.Join(t.TempDir(), "action.json")
	if _, err := propose(t, map[string]string{"file": writeInput(t, raw), "out": out}, "workflow.steps", proposeWorkflowID); err != nil {
		t.Fatal(err)
	}
	action := readAction(t, out)
	want, _ := decisionaction.WorkflowDigest(proposeWorkflowID, json.RawMessage(normalized))
	if action.str(t, "after_sha256") != want {
		t.Errorf("after_sha256 = %s, want the digest of the normalized steps %s", action.str(t, "after_sha256"), want)
	}
}

func TestProposeWorkflowStepInstructionsTrimsLikeTheServer(t *testing.T) {
	live := json.RawMessage(`[{"status_key":"review","handler":{"type":"agent","id":"a"},"instructions":"b"}]`)
	proposeServer(t, map[string]any{"/api/issue-workflows/" + proposeWorkflowID: map[string]any{"steps": live}})
	out := filepath.Join(t.TempDir(), "action.json")
	if _, err := propose(t, map[string]string{"file": writeInput(t, "B2\n"), "status-key": "review", "out": out}, "workflow.step.instructions", proposeWorkflowID); err != nil {
		t.Fatal(err)
	}
	if got := readAction(t, out).str(t, "instructions"); got != "B2" {
		t.Errorf("instructions = %q, want the trimmed text", got)
	}
}

func TestProposeWorkflowStepInstructionsRefusesAHandlerlessStep(t *testing.T) {
	live := json.RawMessage(`[{"status_key":"review","handler":{"type":"none"},"instructions":""}]`)
	proposeServer(t, map[string]any{"/api/issue-workflows/" + proposeWorkflowID: map[string]any{"steps": live}})
	flags := map[string]string{"file": writeInput(t, "B2"), "status-key": "review", "dry-run": "true"}
	if _, err := propose(t, flags, "workflow.step.instructions", proposeWorkflowID); err == nil {
		t.Error("instructions on a handler-less step were accepted; the server would drop them")
	}
}

func TestProposeWorkflowBudgetHashesTheLiveBudgetAndSendsOnlyNamedFields(t *testing.T) {
	proposeServer(t, map[string]any{"/api/issue-workflows/" + proposeWorkflowID: map[string]any{"id": proposeWorkflowID, "max_runs_per_issue": nil, "include_child_prs": true}})
	out := filepath.Join(t.TempDir(), "action.json")
	stdout, err := propose(t, map[string]string{"max-runs": "40", "include-child-prs": "false", "out": out}, "workflow.budget", proposeWorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	forty := 40
	wantBase := decisionaction.WorkflowBudgetDigest(proposeWorkflowID, nil, true)
	wantAfter := decisionaction.WorkflowBudgetDigest(proposeWorkflowID, &forty, false)
	action := readAction(t, out)
	if action.Type != "workflow.budget.set" || action.str(t, "workflow_id") != proposeWorkflowID || action.str(t, "base_sha256") != wantBase {
		t.Fatalf("action = %+v params %s", action, action.Params)
	}
	if string(action.Params["max_runs_per_issue"]) != "40" || string(action.Params["include_child_prs"]) != "false" {
		t.Errorf("budget params = %s", action.Params)
	}
	if !strings.Contains(stdout, wantBase) || !strings.Contains(stdout, wantAfter) {
		t.Errorf("stdout lacks the digests: %s", stdout)
	}

	// Naming one field leaves the other out of the action and out of the digest.
	only := filepath.Join(t.TempDir(), "only.json")
	if _, err := propose(t, map[string]string{"include-child-prs": "false", "out": only}, "workflow.budget", proposeWorkflowID); err != nil {
		t.Fatal(err)
	}
	if _, present := readAction(t, only).Params["max_runs_per_issue"]; present {
		t.Error("an unnamed cap was sent")
	}
}

func TestProposeWorkflowBudgetRejectsBadInputWithoutWriting(t *testing.T) {
	proposeServer(t, map[string]any{"/api/issue-workflows/" + proposeWorkflowID: map[string]any{"id": proposeWorkflowID, "include_child_prs": true}})
	out := filepath.Join(t.TempDir(), "a.json")
	for name, tc := range map[string]struct {
		flags map[string]string
		args  []string
	}{
		"no budget flag":      {map[string]string{"out": out}, []string{"workflow.budget", proposeWorkflowID}},
		"cap above limit":     {map[string]string{"max-runs": "1001", "out": out}, []string{"workflow.budget", proposeWorkflowID}},
		"negative cap":        {map[string]string{"max-runs": "-1", "out": out}, []string{"workflow.budget", proposeWorkflowID}},
		"file is not taken":   {map[string]string{"max-runs": "5", "file": writeInput(t, "x"), "out": out}, []string{"workflow.budget", proposeWorkflowID}},
		"no target":           {map[string]string{"max-runs": "5", "out": out}, []string{"workflow.budget"}},
		"flag on other field": {map[string]string{"max-runs": "5", "file": writeInput(t, "x"), "out": out}, []string{"agent.instructions", proposeAgentID}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := propose(t, tc.flags, tc.args...); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a file was written for bad input")
	}
}
