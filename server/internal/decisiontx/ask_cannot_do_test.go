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
			_, err := ask.CheckCannotDo("block", testCatalog, "")
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
			hints, err := ask.CheckCannotDo("block", testCatalog, "")
			if err != nil || len(hints) != 0 {
				test.Fatalf("err %v hints %v, want accepted", err, hints)
			}
		})
	}
}

func TestCannotDoSurfaceKey(test *testing.T) {
	ask := cannotDoAsk(test, map[string]any{"after_choice": "agent_run", "agent_surface": "firebase_console"})
	_, err := ask.CheckCannotDo("block", testCatalog, "")
	if err == nil || !strings.Contains(err.Error(), "firebase_console") || !strings.Contains(err.Error(), "Publish") {
		test.Fatalf("err = %v, want the key and the human step", err)
	}
	ask = cannotDoAsk(test, map[string]any{"after_choice": "agent_run", "agent_surface": "unlisted"})
	if _, err := ask.CheckCannotDo("block", testCatalog, ""); err != nil {
		test.Fatalf("unlisted surface refused: %v", err)
	}
}

func TestCannotDoWrapperNeedsTheCommandItself(test *testing.T) {
	ask := cannotDoAsk(test, agentRun([]string{"env", "X=1", "multica", "daemon", "restart"}))
	_, err := ask.CheckCannotDo("block", testCatalog, "")
	if err == nil || !strings.Contains(err.Error(), "give the command itself as argv") {
		test.Fatalf("err = %v, want the wrapper line", err)
	}
	hints, err := ask.CheckCannotDo("warn", testCatalog, "")
	if err != nil || len(hints) != 1 || !strings.Contains(hints[0], "give the command itself as argv") {
		test.Fatalf("warn = %v %v, want one wrapper hint", hints, err)
	}
}

func TestCannotDoHumanStepsOptionIsAccepted(test *testing.T) {
	ask := cannotDoAsk(test, map[string]any{"after_choice": "human_steps"})
	if hints, err := ask.CheckCannotDo("block", testCatalog, ""); err != nil || len(hints) != 0 {
		test.Fatalf("err %v hints %v", err, hints)
	}
}

func TestCannotDoModes(test *testing.T) {
	ask := cannotDoAsk(test, agentRun([]string{"multica", "daemon", "restart"}))
	hints, err := ask.CheckCannotDo("warn", testCatalog, "")
	if err != nil || len(hints) != 1 || !strings.Contains(hints[0], "daemon_restart") {
		test.Fatalf("warn = %v %v, want one hint", hints, err)
	}
	hints, err = ask.CheckCannotDo("off", testCatalog, "")
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
	if _, err := ask.CheckCannotDo("block", testCatalog, ""); err == nil || !strings.Contains(err.Error(), "ask.options[0].after_choice") {
		test.Fatalf("block err = %v, want the field name", err)
	}
	hints, err := ask.CheckCannotDo("warn", testCatalog, "")
	if err != nil || len(hints) != 1 || !strings.Contains(hints[0], "after_choice") {
		test.Fatalf("warn = %v %v, want one hint", hints, err)
	}
	if hints, err := ask.CheckCannotDo("off", testCatalog, ""); err != nil || len(hints) != 0 {
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
	if err != nil || mode != "off" || len(catalog) != 3 || catalog[0].Key != "daemon_restart" {
		test.Fatalf("absent = %q %v %v, want off and the three built-in entries", mode, catalog, err)
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

// CHE-7359 (T2): a typed option carries out what a relay run would only repeat.

func TestCannotDoBlockRefusesRelayOnlyStatusAndAssign(test *testing.T) {
	for name, tc := range map[string]struct {
		command []string
		fix     string
	}{
		"status":        {[]string{"$MULTICA_CLI_PATH", "issue", "status", "CHE-1", "done"}, "issue.status.set"},
		"status global": {[]string{"/usr/bin/multica", "--profile", "dev", "issue", "status", "CHE-1", "done", "--no-start"}, "issue.status.set"},
		"assign":        {[]string{"multica", "issue", "assign", "CHE-1", "--to", "Tofu"}, "issue.assign"},
	} {
		test.Run(name, func(test *testing.T) {
			ask := cannotDoAsk(test, agentRun(tc.command))
			_, err := ask.CheckCannotDo("block", testCatalog, "")
			if err == nil || !strings.Contains(err.Error(), tc.fix) || !strings.Contains(err.Error(), "ask.options[0]") || strings.Contains(err.Error(), "\n") {
				test.Fatalf("err = %v, want a one-line refusal with %q", err, tc.fix)
			}
			hints, err := ask.CheckCannotDo("warn", testCatalog, "")
			if err != nil || len(hints) != 1 || !strings.Contains(hints[0], tc.fix) {
				test.Fatalf("warn = %v %v, want one hint with %q", hints, err, tc.fix)
			}
			if hints, err := ask.CheckCannotDo("off", testCatalog, ""); err != nil || len(hints) != 0 {
				test.Fatalf("off = %v %v, want nothing", hints, err)
			}
		})
	}
}

func TestCannotDoAcceptsReadsAndMixedCommands(test *testing.T) {
	for name, commands := range map[string][][]string{
		"issue get":             {{"$MULTICA_CLI_PATH", "issue", "get", "CHE-1"}},
		"status and a build":    {{"multica", "issue", "status", "CHE-1", "done"}, {"make", "test"}},
		"status and issue list": {{"multica", "issue", "status", "CHE-1", "done"}, {"multica", "issue", "list"}},
	} {
		test.Run(name, func(test *testing.T) {
			ask := cannotDoAsk(test, agentRun(commands...))
			if hints, err := ask.CheckCannotDo("block", testCatalog, ""); err != nil || len(hints) != 0 {
				test.Fatalf("err %v hints %v, want accepted", err, hints)
			}
		})
	}
}

func TestCannotDoRefusesCommandsThatNeedThePasskeyCard(test *testing.T) {
	for name, command := range map[string][]string{
		"propose":      {"$MULTICA_CLI_PATH", "decision", "propose", "--type", "workspace.settings.patch"},
		"budget raise": {"multica", "issue", "budget", "raise", "CHE-1", "--cap", "36"},
		"top up":       {"multica", "issue", "budget", "top-up", "CHE-1"},
	} {
		test.Run(name, func(test *testing.T) {
			ask := cannotDoAsk(test, agentRun(command))
			_, err := ask.CheckCannotDo("block", testCatalog, "")
			if err == nil || !strings.Contains(err.Error(), "--action-file") || !strings.Contains(err.Error(), "passkey") {
				test.Fatalf("err = %v, want the --action-file fix", err)
			}
		})
	}
}

func TestCannotDoAllowedAgents(test *testing.T) {
	const listed, other = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	catalog := []CannotDo{{Key: "c00_restart", Tool: "docker", Subcommand: []string{"compose", "restart"}, HumanStep: "Run it on C00.", AllowedAgents: []string{listed}}}
	ask := cannotDoAsk(test, agentRun([]string{"docker", "compose", "restart", "doop"}))
	if _, err := ask.CheckCannotDo("block", catalog, listed); err != nil {
		test.Fatalf("listed asker refused: %v", err)
	}
	for _, asker := range []string{other, ""} {
		_, err := ask.CheckCannotDo("block", catalog, asker)
		if err == nil || !strings.Contains(err.Error(), "c00_restart") || !strings.Contains(err.Error(), "Run it on C00.") || !strings.Contains(err.Error(), "issue.assign") {
			test.Fatalf("asker %q: err = %v, want the key, the human step and issue.assign", asker, err)
		}
	}
	ask = cannotDoAsk(test, agentRun([]string{"docker", "ps"}))
	if _, err := ask.CheckCannotDo("block", catalog, other); err != nil {
		test.Fatalf("unmatched command refused: %v", err)
	}
}

func TestBuiltinCatalogHasTheC00Entry(test *testing.T) {
	_, catalog, err := CannotDoFromSettings(nil)
	if err != nil {
		test.Fatal(err)
	}
	for _, entry := range catalog {
		if entry.Key == "c00_restart" && len(entry.AllowedAgents) == 1 {
			return
		}
	}
	test.Fatalf("catalog %v has no c00_restart entry with one allowed agent", catalog)
}

func TestParseCannotDoAllowedAgents(test *testing.T) {
	entry := func(ids string) string {
		return `[{"key":"k","tool":"gh","human_step":"x","allowed_agents":` + ids + `}]`
	}
	if _, err := ParseCannotDo([]byte(entry(`["11111111-1111-4111-8111-111111111111"]`))); err != nil {
		test.Fatalf("good: %v", err)
	}
	for _, bad := range []string{`["Tofu"]`, `[""]`, `["11111111-1111-4111-8111-111111111111","11111111-1111-4111-8111-111111111111"]`, `"x"`} {
		if _, err := ParseCannotDo([]byte(entry(bad))); err == nil {
			test.Errorf("allowed_agents %s accepted", bad)
		}
	}
}
