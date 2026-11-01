package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

type presetCall struct {
	Method, Path string
	Body         map[string]any
}

// presetServer records each request and answers with reply.
func presetServer(t *testing.T, reply string) (*[]presetCall, func()) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	var calls []presetCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := presetCall{Method: r.Method, Path: r.URL.Path}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &c.Body)
		}
		calls = append(calls, c)
		_, _ = w.Write([]byte(reply))
	}))
	setCLITestServerEnv(t, srv.URL)
	return &calls, srv.Close
}

const presetAgentID = "11111111-1111-1111-1111-111111111111"

// presetFlagsCmd carries every flag the preset subcommands read.
func presetFlagsCmd() *cobra.Command {
	cmd := &cobra.Command{}
	addPresetSettingFlags(cmd)
	cmd.Flags().StringArray("reset", nil, "")
	cmd.Flags().String("issue", "", "")
	cmd.Flags().Bool("clear", false, "")
	cmd.Flags().String("output", "table", "")
	cmd.Flags().String("profile", "", "")
	return cmd
}

// Cheese: `multica agent preset use <agent> <TAB>` offers the agent's usable
// presets, base and reset, exactly what /preset offers in a DM. It checks the
// completion function wired on the real command.
func TestPresetCompletion_UseOffersPresetsBaseAndReset(t *testing.T) {
	calls, stop := presetServer(t, `{"names":["base","reset","big","fast"]}`)
	defer stop()

	got, _ := agentPresetUseCmd.ValidArgsFunction(agentPresetUseCmd, []string{presetAgentID}, "")

	want := []string{"base", "reset", "big", "fast"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("completions = %v, want %v", got, want)
	}
	if len(*calls) != 1 || (*calls)[0].Path != "/api/agents/"+presetAgentID+"/presets/completions" {
		t.Fatalf("requests = %+v", *calls)
	}
}

// update and delete act on stored presets, so they complete from the full
// inventory: a stale preset must stay reachable for repair or deletion, and the
// list is open to a manager who cannot invoke the agent.
func TestPresetCompletion_UpdateAndDeleteListStoredPresets(t *testing.T) {
	calls, stop := presetServer(t, `[{"name":"big","stale":"model gone"},{"name":"fast","stale":""}]`)
	defer stop()

	got, _ := completePresetNames(true)(presetFlagsCmd(), []string{presetAgentID}, "")

	if want := []string{"big", "fast"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("completions = %v, want %v", got, want)
	}
	if len(*calls) != 1 || (*calls)[0].Path != "/api/agents/"+presetAgentID+"/presets" {
		t.Fatalf("requests = %+v", *calls)
	}
}

func TestPresetCompletion_FiltersByPrefixAndOnlyForSecondArg(t *testing.T) {
	_, stop := presetServer(t, `{"names":["base","reset","big","fast"]}`)
	defer stop()

	got, _ := completePresetNames(false)(presetFlagsCmd(), []string{presetAgentID}, "b")
	if want := []string{"base", "big"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("prefix b = %v, want %v", got, want)
	}
	if third, _ := completePresetNames(false)(presetFlagsCmd(), []string{presetAgentID, "fast"}, ""); len(third) != 0 {
		t.Fatalf("a third argument should complete nothing, got %v", third)
	}
}

func TestPresetCompletion_ServerErrorCompletesNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	got, dir := completePresetNames(false)(presetFlagsCmd(), []string{presetAgentID}, "")
	if len(got) != 0 || dir&cobra.ShellCompDirectiveNoFileComp == 0 {
		t.Fatalf("got %v, directive %v", got, dir)
	}
}

func TestPresetCreateSendsOnlyTheGivenFields(t *testing.T) {
	calls, stop := presetServer(t, `{"name":"fast"}`)
	defer stop()
	cmd := presetFlagsCmd()
	_ = cmd.Flags().Set("thinking-level", "low")
	_ = cmd.Flags().Set("context-arg", "--autocompact")
	_ = cmd.Flags().Set("context-arg", "200k")
	_ = cmd.Flags().Set("default", "true")

	if _, err := captureStdout(t, func() error { return runAgentPresetCreate(cmd, []string{presetAgentID, "fast"}) }); err != nil {
		t.Fatal(err)
	}

	if len(*calls) != 1 || (*calls)[0].Method != "POST" || (*calls)[0].Path != "/api/agents/"+presetAgentID+"/presets" {
		t.Fatalf("requests = %+v", *calls)
	}
	want := map[string]any{"name": "fast", "thinking_level": "low", "context_args": []any{"--autocompact", "200k"}, "is_default": true}
	if !reflect.DeepEqual((*calls)[0].Body, want) {
		t.Fatalf("body = %v, want %v", (*calls)[0].Body, want)
	}
}

// S4 on the wire: --reset sends JSON null (inherit), unset flags send nothing.
func TestPresetUpdateResetSendsNull(t *testing.T) {
	calls, stop := presetServer(t, `{"name":"fast"}`)
	defer stop()
	cmd := presetFlagsCmd()
	_ = cmd.Flags().Set("model", "claude-opus-5-5")
	_ = cmd.Flags().Set("reset", "thinking_level")
	_ = cmd.Flags().Set("reset", "context_args")

	if _, err := captureStdout(t, func() error { return runAgentPresetUpdate(cmd, []string{presetAgentID, "fast"}) }); err != nil {
		t.Fatal(err)
	}

	if len(*calls) != 1 || (*calls)[0].Method != "PATCH" || (*calls)[0].Path != "/api/agents/"+presetAgentID+"/presets/fast" {
		t.Fatalf("requests = %+v", *calls)
	}
	want := map[string]any{"model": "claude-opus-5-5", "thinking_level": nil, "context_args": nil}
	if !reflect.DeepEqual((*calls)[0].Body, want) {
		t.Fatalf("body = %v, want %v", (*calls)[0].Body, want)
	}
}

func TestPresetUpdateNeedsAChange(t *testing.T) {
	_, stop := presetServer(t, `{}`)
	defer stop()
	if err := runAgentPresetUpdate(presetFlagsCmd(), []string{presetAgentID, "fast"}); err == nil {
		t.Fatal("expected an error when nothing changes")
	}
}

func TestPresetUseRoutes(t *testing.T) {
	cases := []struct {
		name         string
		flags        map[string]string
		args         []string
		method, path string
		body         map[string]any
	}{
		{"default", map[string]string{"default": "true"}, []string{presetAgentID, "fast"}, "PUT", "/api/agents/" + presetAgentID + "/presets/default", map[string]any{"name": "fast"}},
		{"default base", map[string]string{"default": "true"}, []string{presetAgentID, "base"}, "PUT", "/api/agents/" + presetAgentID + "/presets/default", map[string]any{"name": "base"}},
		{"issue", map[string]string{"issue": "issue-1"}, []string{presetAgentID, "fast"}, "PUT", "/api/issues/issue-1/preset", map[string]any{"name": "fast"}},
		// reset, the keyword the completion offers, hands the target back.
		{"default reset", map[string]string{"default": "true"}, []string{presetAgentID, "reset"}, "PUT", "/api/agents/" + presetAgentID + "/presets/default", map[string]any{"name": "base"}},
		{"issue reset", map[string]string{"issue": "issue-1"}, []string{presetAgentID, "reset"}, "DELETE", "/api/issues/issue-1/preset", nil},
		{"issue clear", map[string]string{"issue": "issue-1", "clear": "true"}, []string{presetAgentID}, "DELETE", "/api/issues/issue-1/preset", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, stop := presetServer(t, `{}`)
			defer stop()
			cmd := presetFlagsCmd()
			for k, v := range tc.flags {
				_ = cmd.Flags().Set(k, v)
			}
			if _, err := captureStdout(t, func() error { return runAgentPresetUse(cmd, tc.args) }); err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 || (*calls)[0].Method != tc.method || (*calls)[0].Path != tc.path || !reflect.DeepEqual((*calls)[0].Body, tc.body) {
				t.Fatalf("requests = %+v, want %s %s %v", *calls, tc.method, tc.path, tc.body)
			}
		})
	}
}

func TestPresetUseNeedsATarget(t *testing.T) {
	_, stop := presetServer(t, `{}`)
	defer stop()
	if err := runAgentPresetUse(presetFlagsCmd(), []string{presetAgentID, "fast"}); err == nil || !strings.Contains(err.Error(), "--default") {
		t.Fatalf("err = %v, want a hint naming --default", err)
	}
}

func TestPresetDeleteAndList(t *testing.T) {
	calls, stop := presetServer(t, `[]`)
	defer stop()
	if _, err := captureStdout(t, func() error { return runAgentPresetDelete(presetFlagsCmd(), []string{presetAgentID, "fast"}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return runAgentPresetList(presetFlagsCmd(), []string{presetAgentID}) }); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || (*calls)[0].Method != "DELETE" || (*calls)[0].Path != "/api/agents/"+presetAgentID+"/presets/fast" ||
		(*calls)[1].Method != "GET" || (*calls)[1].Path != "/api/agents/"+presetAgentID+"/presets" {
		t.Fatalf("requests = %+v", *calls)
	}
}
