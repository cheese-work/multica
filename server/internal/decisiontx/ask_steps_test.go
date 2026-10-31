package decisiontx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stepsAsk returns the valid base ask plus the fields of a human_steps fixture.
func stepsAsk(test *testing.T, fixture string) map[string]any {
	test.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "human_steps", fixture+".json"))
	if err != nil {
		test.Fatal(err)
	}
	var extra map[string]any
	if err := json.Unmarshal(data, &extra); err != nil {
		test.Fatal(err)
	}
	ask := validAskMap()
	for key, value := range extra {
		ask[key] = value
	}
	return ask
}

func decodeMap(ask map[string]any) (Ask, error) {
	raw, _ := json.Marshal(ask)
	return DecodeAsk(raw)
}

func firstStep(ask map[string]any) map[string]any {
	return ask["human_steps"].([]any)[0].(map[string]any)
}

func TestHumanStepsFixtures(test *testing.T) {
	cases := []struct{ fixture, want string }{
		{"pass", ""},
		{"che1532_1505", "ask.human_steps[0] names MULTICA_TOKEN without a values entry"},
		{"doop", "ask.probes requires at least one item"},
	}
	for _, tc := range cases {
		test.Run(tc.fixture, func(test *testing.T) {
			_, err := decodeMap(stepsAsk(test, tc.fixture))
			if got := errString(err); got != tc.want {
				test.Fatalf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHumanStepsRejects(test *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"no action", func(ask map[string]any) { delete(firstStep(ask), "action") }, "ask.human_steps[0].action is required"},
		{"no verify_done", func(ask map[string]any) { delete(firstStep(ask), "verify_done") }, "ask.human_steps[0].verify_done is required"},
		{"no after_done", func(ask map[string]any) { delete(ask, "after_done") }, "ask.after_done is required"},
		{"empty probes", func(ask map[string]any) { ask["probes"] = []string{} }, "ask.probes requires at least one item"},
		{"steps not a list", func(ask map[string]any) { ask["human_steps"] = "x" }, "ask.human_steps must be a list"},
		{"step not an object", func(ask map[string]any) { ask["human_steps"] = []any{"x"} }, "ask.human_steps[0] must be an object with action, command, values, verify_done"},
		{"unknown step key", func(ask map[string]any) { firstStep(ask)["extra"] = 1 }, "ask.human_steps[0].extra is not a known field"},
		{"value without name", func(ask map[string]any) {
			firstStep(ask)["values"].([]any)[0].(map[string]any)["name"] = ""
		}, "ask.human_steps[0].values[0].name is required"},
		{"value without value or source", func(ask map[string]any) {
			firstStep(ask)["values"] = []any{map[string]any{"name": "MULTICA_SERVER_URL"}}
		}, "ask.human_steps[0].values[0] requires a value or a source"},
		{"placeholder without source", func(ask map[string]any) {
			firstStep(ask)["values"] = []any{map[string]any{"name": "MULTICA_SERVER_URL", "value": "<url>"}}
		}, "ask.human_steps[0].values[0].value is a placeholder; give the literal or a source"},
		{"ellipsis without source", func(ask map[string]any) {
			firstStep(ask)["values"] = []any{map[string]any{"name": "MULTICA_SERVER_URL", "value": "https://…"}}
		}, "ask.human_steps[0].values[0].value is a placeholder; give the literal or a source"},
		{"placeholder in command", func(ask map[string]any) { firstStep(ask)["command"] = "gh secret set X --body <token>" }, "ask.human_steps[0] names <token> without a values entry"},
		{"secret name is only a prefix of the declared name", func(ask map[string]any) {
			step := firstStep(ask)
			step["command"] = "gh secret set MULTICA_TOKEN"
			step["values"] = []any{map[string]any{"name": "MULTICA_TOKEN_BACKUP", "value": "x"}}
		}, "ask.human_steps[0] names MULTICA_TOKEN without a values entry"},
		{"placeholder is only a word inside the declared name", func(ask map[string]any) {
			step := firstStep(ask)
			step["command"] = "gh secret set X --body <token>"
			step["values"] = []any{map[string]any{"name": "unrelated_token_backup", "value": "x"}}
		}, "ask.human_steps[0] names <token> without a values entry"},
		{"implicit value word", func(ask map[string]any) { firstStep(ask)["action"] = "Set the URL, tương tự the others" }, "ask.human_steps[0].action leaves values implicit (tương tự); write each one"},
		{"implicit etc", func(ask map[string]any) { firstStep(ask)["action"] = "Set the URL, etc." }, "ask.human_steps[0].action leaves values implicit (etc.); write each one"},
		{"bare uuid in action", func(ask map[string]any) {
			firstStep(ask)["action"] = "Open agent 75eed3bd-33c6-42f5-b612-cf0f03b8879a"
		}, "ask.human_steps[0].action has a bare UUID; use a name or a mention link"},
		{"bare uuid in after_done", func(ask map[string]any) {
			ask["after_done"] = "Ping 75eed3bd-33c6-42f5-b612-cf0f03b8879a"
		}, "ask.after_done has a bare UUID; use a name or a mention link"},
		{"probes list of numbers", func(ask map[string]any) { ask["probes"] = []any{3} }, "ask.probes must be a list of strings"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			ask := stepsAsk(test, "pass")
			tc.mutate(ask)
			_, err := decodeMap(ask)
			if got := errString(err); got != tc.want {
				test.Fatalf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHumanStepsAccepts(test *testing.T) {
	cases := map[string]func(map[string]any){
		"placeholder with source": func(ask map[string]any) {
			firstStep(ask)["values"].([]any)[0].(map[string]any)["value"] = "<url>"
		},
		"uuid in command and value": func(ask map[string]any) {
			firstStep(ask)["command"] = "echo 75eed3bd-33c6-42f5-b612-cf0f03b8879a MULTICA_SERVER_URL"
		},
		"uuid inside mention link": func(ask map[string]any) {
			ask["after_done"] = "[x99](mention://agent/75eed3bd-33c6-42f5-b612-cf0f03b8879a) re-runs it"
		},
		"/etc path in command": func(ask map[string]any) {
			firstStep(ask)["command"] = "cat /etc/hosts MULTICA_SERVER_URL"
		},
		"no steps and no extras": func(ask map[string]any) {
			delete(ask, "human_steps")
			delete(ask, "probes")
			delete(ask, "after_done")
		},
	}
	for name, mutate := range cases {
		test.Run(name, func(test *testing.T) {
			ask := stepsAsk(test, "pass")
			mutate(ask)
			if _, err := decodeMap(ask); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestContentRendersHumanSteps(test *testing.T) {
	ask, err := decodeMap(stepsAsk(test, "pass"))
	if err != nil {
		test.Fatal(err)
	}
	content := ask.Content("acme")
	last := -1
	for _, marker := range []string{
		"**Default if no reply**", "**Steps for you**",
		"1. Set the API URL secret", "   ```\n   gh secret set MULTICA_SERVER_URL", "- MULTICA_SERVER_URL (API port 19444, not web port 19443): https://congvc-c00.taila6fa8a.ts.net:19444 (source: agent env MULTICA_SERVER_URL)", "Done when: gh secret list -R cheese-work/multica-dotfiles shows MULTICA_SERVER_URL",
		"2. Set the workspace secret", "- MULTICA_WORKSPACE_ID (UUID, not the slug cheese-work): f2b734e0-b6de-4414-8a27-a2f69b0ef843",
		"3. Set the token secret", "gh secret set MULTICA_TOKEN -R cheese-work/multica-dotfiles\n", "(source: web app Settings > API Tokens",
		"**Checked first**", "- 15:15 ICT: curl", "- not verified: token write scope",
		"**After you finish**", "x99-claude-sonnet re-runs skill-sync.yml", "<details>",
	} {
		at := strings.Index(content, marker)
		if at <= last {
			test.Fatalf("%q missing or out of order in %q", marker, content)
		}
		last = at
	}
}

func TestContentKeepsTokenOutOfTheCommand(test *testing.T) {
	ask, err := decodeMap(stepsAsk(test, "pass"))
	if err != nil {
		test.Fatal(err)
	}
	if content := ask.Content("acme"); strings.Contains(content, "MULTICA_TOKEN -R cheese-work/multica-dotfiles --body") {
		test.Fatalf("token step must prompt, not take --body: %q", content)
	}
}

func TestContentFenceGrowsPastBackticks(test *testing.T) {
	raw := stepsAsk(test, "pass")
	firstStep(raw)["command"] = "echo ``` MULTICA_SERVER_URL"
	ask, err := decodeMap(raw)
	if err != nil {
		test.Fatal(err)
	}
	if content := ask.Content("acme"); !strings.Contains(content, "   ````\n   echo ``` MULTICA_SERVER_URL\n   ````") {
		test.Fatalf("command fence did not grow: %q", content)
	}
}

func TestContentWithoutHumanStepsMatchesLegacy(test *testing.T) {
	content := decodedAsk(test).Content("acme")
	for _, section := range []string{"**Steps for you**", "**Checked first**", "**After you finish**"} {
		if strings.Contains(content, section) {
			test.Fatalf("legacy ask rendered %q: %q", section, content)
		}
	}
	want := "**Default if no reply**\nLATER\n\n<details>\n<summary>Details</summary>"
	if !strings.Contains(content, want) {
		test.Fatalf("legacy layout changed: %q", content)
	}
}

func TestLintHumanSteps(test *testing.T) {
	hasHint := func(hints []string, part string) bool {
		for _, hint := range hints {
			if strings.Contains(hint, part) {
				return true
			}
		}
		return false
	}
	pass, err := decodeMap(stepsAsk(test, "pass"))
	if err != nil {
		test.Fatal(err)
	}
	if hints := LintAsk(pass); len(hints) != 0 {
		test.Fatalf("clean steps ask got hints: %q", hints)
	}

	asksToRun := decodedAsk(test)
	asksToRun.Decision = "Merge the PR now?"
	if hints := LintAsk(asksToRun); !hasHint(hints, "no human_steps") {
		test.Fatalf("hints = %q, want a no-human_steps hint", hints)
	}

	merge := pass
	merge.HumanSteps = []HumanStep{{Action: "Merge the PR", VerifyDone: "PR shows Merged"}}
	if hints := LintAsk(merge); !hasHint(hints, "merge step") {
		test.Fatalf("hints = %q, want a merge-readiness hint", hints)
	}
	merge.Probes = []string{"PR is ready, not draft; CI checks green"}
	if hints := LintAsk(merge); hasHint(hints, "merge step") {
		test.Fatalf("hints = %q, probes name ready and CI", hints)
	}

	uuidName := pass
	uuidName.HumanSteps = []HumanStep{{Action: "Set it", VerifyDone: "done", Values: []StepValue{{Name: "WORKSPACE_ID", Value: "f2b734e0-b6de-4414-8a27-a2f69b0ef843"}}}}
	if hints := LintAsk(uuidName); !hasHint(hints, "UUID") {
		test.Fatalf("hints = %q, want a UUID-name hint", hints)
	}

	bare := pass
	bare.Decision = "Should 75eed3bd-33c6-42f5-b612-cf0f03b8879a continue?"
	if hints := LintAsk(bare); !hasHint(hints, "decision has a bare UUID") {
		test.Fatalf("hints = %q, want a bare-UUID hint", hints)
	}
}
