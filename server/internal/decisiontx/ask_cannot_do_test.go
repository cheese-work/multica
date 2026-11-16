package decisiontx

import (
	"encoding/json"
	"strings"
	"testing"
)

var testCatalog = []CannotDo{
	{Key: "daemon_restart", Tool: "multica", Subcommand: []string{"daemon", "restart"}, HumanStep: "Run `multica daemon restart` on the host."},
	{Key: "daemon_stop", Tool: "multica", Subcommand: []string{"daemon", "stop"}, HumanStep: "Run `multica daemon stop` on the host."},
	{Key: "firebase_console", HumanStep: "Open the Firebase console and press Publish."},
}

// cannotDoAsk is a valid ask whose first option has the given typed fields.
func cannotDoAsk(test *testing.T, fields map[string]any) Ask {
	test.Helper()
	raw := validAskMap()
	option := raw["options"].([]any)[0].(map[string]any)
	option["label"] = "Yes, reload"
	for key, value := range fields {
		option[key] = value
	}
	for _, other := range raw["options"].([]any)[1:] {
		other.(map[string]any)["after_choice"] = "record_only"
	}
	body, _ := json.Marshal(raw)
	ask, err := DecodeAsk(body)
	if err != nil {
		test.Fatalf("decode: %v", err)
	}
	return ask
}

func agentRun(commands ...[]string) map[string]any {
	list := make([]any, len(commands))
	for index, command := range commands {
		list[index] = command
	}
	return map[string]any{"after_choice": "agent_run", "agent_commands": list}
}

func TestCannotDoBlockRefusesTheCommandsAnAgentRunCannotRun(test *testing.T) {
	for name, command := range map[string][]string{
		"env var":         {"$MULTICA_CLI_PATH", "daemon", "restart"},
		"plain name":      {"multica", "daemon", "restart"},
		"quotes kept":     {`"$MULTICA_CLI_PATH"`, "daemon", "restart"},
		"path and global": {"/home/u/.local/bin/multica", "--profile", "dev", "daemon", "restart"},
		"exe stop":        {"multica.exe", "daemon", "stop"},
		"extra flags":     {"multica", "daemon", "restart", "--maintenance-token", "x"},
	} {
		test.Run(name, func(test *testing.T) {
			ask := cannotDoAsk(test, agentRun(command))
			_, err := ask.CheckCannotDo("block", testCatalog)
			if err == nil {
				test.Fatal("want refusal")
			}
			got := err.Error()
			for _, want := range []string{"ask.options[0]", "Yes, reload", "daemon_", "Run `multica daemon "} {
				if !strings.Contains(got, want) {
					test.Errorf("error %q lacks %q", got, want)
				}
			}
			if strings.Contains(got, "\n") {
				test.Errorf("error is more than one line: %q", got)
			}
		})
	}
}

func TestCannotDoAcceptsWhatAnAgentRunCanRun(test *testing.T) {
	for name, command := range map[string][]string{
		"status":  {"$MULTICA_CLI_PATH", "daemon", "status"},
		"similar": {"multica", "daemon", "restarts"},
		"short":   {"multica", "daemon"},
		"other":   {"gh", "pr", "ready", "7"},
	} {
		test.Run(name, func(test *testing.T) {
			ask := cannotDoAsk(test, agentRun(command))
			hints, err := ask.CheckCannotDo("block", testCatalog)
			if err != nil || len(hints) != 0 {
				test.Fatalf("err %v hints %v, want accepted", err, hints)
			}
		})
	}
}

func TestCannotDoSurfaceKey(test *testing.T) {
	ask := cannotDoAsk(test, map[string]any{"after_choice": "agent_run", "agent_surface": "firebase_console"})
	_, err := ask.CheckCannotDo("block", testCatalog)
	if err == nil || !strings.Contains(err.Error(), "firebase_console") || !strings.Contains(err.Error(), "Publish") {
		test.Fatalf("err = %v, want the key and the human step", err)
	}
	ask = cannotDoAsk(test, map[string]any{"after_choice": "agent_run", "agent_surface": "unlisted"})
	if _, err := ask.CheckCannotDo("block", testCatalog); err != nil {
		test.Fatalf("unlisted surface refused: %v", err)
	}
}

func TestCannotDoWrapperNeedsTheCommandItself(test *testing.T) {
	ask := cannotDoAsk(test, agentRun([]string{"env", "X=1", "multica", "daemon", "restart"}))
	_, err := ask.CheckCannotDo("block", testCatalog)
	if err == nil || !strings.Contains(err.Error(), "give the command itself as argv") {
		test.Fatalf("err = %v, want the wrapper line", err)
	}
	hints, err := ask.CheckCannotDo("warn", testCatalog)
	if err != nil || len(hints) != 1 || !strings.Contains(hints[0], "give the command itself as argv") {
		test.Fatalf("warn = %v %v, want one wrapper hint", hints, err)
	}
}

func TestCannotDoHumanStepsOptionIsAccepted(test *testing.T) {
	ask := cannotDoAsk(test, map[string]any{"after_choice": "human_steps"})
	if hints, err := ask.CheckCannotDo("block", testCatalog); err != nil || len(hints) != 0 {
		test.Fatalf("err %v hints %v", err, hints)
	}
}

func TestCannotDoModes(test *testing.T) {
	ask := cannotDoAsk(test, agentRun([]string{"multica", "daemon", "restart"}))
	hints, err := ask.CheckCannotDo("warn", testCatalog)
	if err != nil || len(hints) != 1 || !strings.Contains(hints[0], "daemon_restart") {
		test.Fatalf("warn = %v %v, want one hint", hints, err)
	}
	hints, err = ask.CheckCannotDo("off", testCatalog)
	if err != nil || len(hints) != 0 {
		test.Fatalf("off = %v %v, want nothing", hints, err)
	}
}

func TestCannotDoMissingAfterChoice(test *testing.T) {
	raw := validAskMap()
	body, _ := json.Marshal(raw)
	ask, err := DecodeAsk(body) // the typed field is optional to decode
	if err != nil {
		test.Fatal(err)
	}
	if _, err := ask.CheckCannotDo("block", testCatalog); err == nil || !strings.Contains(err.Error(), "ask.options[0].after_choice") {
		test.Fatalf("block err = %v, want the field name", err)
	}
	hints, err := ask.CheckCannotDo("warn", testCatalog)
	if err != nil || len(hints) != 1 || !strings.Contains(hints[0], "after_choice") {
		test.Fatalf("warn = %v %v, want one hint", hints, err)
	}
	if hints, err := ask.CheckCannotDo("off", testCatalog); err != nil || len(hints) != 0 {
		test.Fatalf("off = %v %v", hints, err)
	}
}

func TestDecodeRefusesBadTypedFields(test *testing.T) {
	cases := map[string]struct {
		fields map[string]any
		want   string
	}{
		"command as one string":      {map[string]any{"after_choice": "agent_run", "agent_commands": []any{"multica daemon restart"}}, "ask.options[0].agent_commands[0] must be an argv array of strings"},
		"empty argv":                 {map[string]any{"after_choice": "agent_run", "agent_commands": []any{[]any{}}}, "ask.options[0].agent_commands[0] must not be empty"},
		"bad after_choice":           {map[string]any{"after_choice": "maybe"}, "ask.options[0].after_choice must be agent_run, human_steps or record_only"},
		"agent_run with nothing":     {map[string]any{"after_choice": "agent_run"}, "ask.options[0].after_choice agent_run needs agent_commands or agent_surface"},
		"commands without agent_run": {map[string]any{"after_choice": "human_steps", "agent_commands": []any{[]any{"multica", "status"}}}, "ask.options[0].agent_commands needs after_choice agent_run"},
	}
	for name, tc := range cases {
		test.Run(name, func(test *testing.T) {
			raw := validAskMap()
			option := raw["options"].([]any)[0].(map[string]any)
			for key, value := range tc.fields {
				option[key] = value
			}
			body, _ := json.Marshal(raw)
			_, err := DecodeAsk(body)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				test.Fatalf("err = %v, want prefix %q", err, tc.want)
			}
		})
	}
}

func TestCannotDoFromSettings(test *testing.T) {
	mode, catalog, err := CannotDoFromSettings(nil)
	if err != nil || mode != "off" || len(catalog) != 2 || catalog[0].Key != "daemon_restart" {
		test.Fatalf("absent = %q %v %v, want off and the two built-in entries", mode, catalog, err)
	}
	mode, catalog, err = CannotDoFromSettings([]byte(`{"agent_cannot_do_mode":"block","agent_cannot_do":[{"key":"x","tool":"gh","subcommand":["pr","merge"],"human_step":"Merge it yourself."}]}`))
	if err != nil || mode != "block" || len(catalog) != 1 || catalog[0].Key != "x" {
		test.Fatalf("set = %q %v %v", mode, catalog, err)
	}
	if _, catalog, _ = CannotDoFromSettings([]byte(`{"agent_cannot_do":[]}`)); len(catalog) != 0 {
		test.Fatalf("an explicit empty list must clear the built-ins, got %v", catalog)
	}
	for _, bad := range []string{
		`{"agent_cannot_do_mode":"strict"}`, `{"agent_cannot_do_mode":true}`, `{"agent_cannot_do":{}}`,
		`{"agent_cannot_do":[{"tool":"gh","human_step":"x"}]}`,
		`{"agent_cannot_do":[{"key":"a","human_step":""}]}`,
		`{"agent_cannot_do":[{"key":"a","human_step":"x"},{"key":"a","human_step":"y"}]}`,
		`{"agent_cannot_do":[{"key":"a","human_step":"x","extra":1}]}`,
		`{"agent_cannot_do":[{"key":"a","tool":"multica ","subcommand":["daemon","restart"],"human_step":"x"}]}`,
		`{"agent_cannot_do":[{"key":"a","tool":"multica","subcommand":["daemon ","restart"],"human_step":"x"}]}`,
		`{"agent_cannot_do":[{"key":"a","tool":"multica","subcommand":["daemon",""],"human_step":"x"}]}`,
		`{"agent_cannot_do":[{"key":"a","tool":"multica","subcommand":["--debug","daemon"],"human_step":"x"}]}`,
	} {
		if _, _, err := CannotDoFromSettings([]byte(bad)); err == nil {
			test.Errorf("settings %s accepted", bad)
		}
	}
}
