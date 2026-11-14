package decisiontx

import (
	"encoding/json"
	"strings"
	"testing"
)

// CHE-5753 (R03 of CHE-5575, CHE-5751): a card that asks a human to act is
// refused when it names a UI control nobody saw, a repo file with no repo and
// commit, a command nobody dry-ran, or a placeholder the human must edit.

const goodSHA = "0123456789abcdef0123456789abcdef01234567"

func TestHumanAskGuardRejects(test *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		// (a) a UI control path with no cited place where it was observed
		{"ui path in action without observed", func(ask map[string]any) {
			step := firstStep(ask)
			step["action"] = "Open Workspace settings → Delegated governed writes and switch it on"
			delete(step, "observed")
		}, "ask.human_steps[0] cites a UI path with no observed; say where and when you saw the control"},
		{"ascii arrow in action", func(ask map[string]any) {
			step := firstStep(ask)
			step["action"] = "Open Settings -> Members"
			delete(step, "observed")
		}, "ask.human_steps[0] cites a UI path with no observed; say where and when you saw the control"},
		{"ui path in a value source", func(ask map[string]any) {
			step := firstStep(ask)
			step["values"] = []any{map[string]any{"name": "MULTICA_SERVER_URL", "source": "web app Settings > API Tokens"}}
			delete(step, "observed")
		}, "ask.human_steps[0] cites a UI path with no observed; say where and when you saw the control"},
		{"blank observed", func(ask map[string]any) {
			step := firstStep(ask)
			step["action"] = "Open Settings > API Tokens"
			step["observed"] = "  "
		}, "ask.human_steps[0] cites a UI path with no observed; say where and when you saw the control"},

		// (b) a file path with no repo and commit SHA
		{"repo file without files entry", func(ask map[string]any) {
			firstStep(ask)["command"] = "bash scripts/run-checks.sh MULTICA_SERVER_URL"
		}, "ask.human_steps[0] names the file scripts/run-checks.sh without a files entry with repo and a 40-character commit SHA"},
		{"repo file in action", func(ask map[string]any) {
			firstStep(ask)["action"] = "Edit docs/setup/runner.md"
		}, "ask.human_steps[0] names the file docs/setup/runner.md without a files entry with repo and a 40-character commit SHA"},
		{"files entry for another path", func(ask map[string]any) {
			step := firstStep(ask)
			step["command"] = "bash scripts/run-checks.sh MULTICA_SERVER_URL"
			step["files"] = []any{map[string]any{"path": "scripts/other.sh", "repo": "cheese-work/multica", "sha": goodSHA}}
		}, "ask.human_steps[0] names the file scripts/run-checks.sh without a files entry with repo and a 40-character commit SHA"},
		{"file without sha", func(ask map[string]any) {
			step := firstStep(ask)
			step["command"] = "bash scripts/run-checks.sh MULTICA_SERVER_URL"
			step["files"] = []any{map[string]any{"path": "scripts/run-checks.sh", "repo": "cheese-work/multica"}}
		}, "ask.human_steps[0].files[0] needs a repo and sha, or an attachment_id"},
		{"file with short sha", func(ask map[string]any) {
			step := firstStep(ask)
			step["command"] = "bash scripts/run-checks.sh MULTICA_SERVER_URL"
			step["files"] = []any{map[string]any{"path": "scripts/run-checks.sh", "repo": "cheese-work/multica", "sha": "abc1234"}}
		}, "ask.human_steps[0].files[0].sha must be a 40-character commit SHA"},
		{"file with bad repo", func(ask map[string]any) {
			step := firstStep(ask)
			step["command"] = "bash scripts/run-checks.sh MULTICA_SERVER_URL"
			step["files"] = []any{map[string]any{"path": "scripts/run-checks.sh", "repo": "multica", "sha": goodSHA}}
		}, "ask.human_steps[0].files[0].repo must be owner/name"},
		{"file without path", func(ask map[string]any) {
			firstStep(ask)["files"] = []any{map[string]any{"repo": "cheese-work/multica", "sha": goodSHA}}
		}, "ask.human_steps[0].files[0].path is required"},

		{"backtick-wrapped repo file in action", func(ask map[string]any) {
			firstStep(ask)["action"] = "Edit `docs/setup/runner.md`"
		}, "ask.human_steps[0] names the file docs/setup/runner.md without a files entry with repo and a 40-character commit SHA"},
		{"root file in action", func(ask map[string]any) {
			firstStep(ask)["action"] = "Edit setup.sh"
		}, "ask.human_steps[0] names the file setup.sh without a files entry with repo and a 40-character commit SHA"},
		{"dot-slash root file in command", func(ask map[string]any) {
			firstStep(ask)["command"] = "bash ./setup.sh MULTICA_SERVER_URL"
		}, "ask.human_steps[0] names the file ./setup.sh without a files entry with repo and a 40-character commit SHA"},

		// (c) a command that was not dry-run on the target host shape
		{"command written in action text without dry_run", func(ask map[string]any) {
			step := firstStep(ask)
			step["action"] = "Run `gh secret set MULTICA_SERVER_URL`"
			delete(step, "command")
			delete(step, "dry_run")
		}, "ask.human_steps[0].dry_run is required for a command; say the host shape you ran it on and what it printed"},
		{"run verb in action text without dry_run", func(ask map[string]any) {
			step := firstStep(ask)
			step["action"] = "Run gh secret set MULTICA_SERVER_URL"
			delete(step, "command")
			delete(step, "dry_run")
		}, "ask.human_steps[0].dry_run is required for a command; say the host shape you ran it on and what it printed"},
		{"placeholder in a command written in action text", func(ask map[string]any) {
			step := firstStep(ask)
			step["action"] = "Run `gh secret set MULTICA_SERVER_URL --body <token>`"
			step["values"] = []any{map[string]any{"name": "<token>", "value": "abc"}, map[string]any{"name": "MULTICA_SERVER_URL", "value": "x"}}
			delete(step, "command")
		}, "ask.human_steps[0].action has a placeholder (<token>); write the literal the human runs"},
		{"command without dry_run", func(ask map[string]any) { delete(firstStep(ask), "dry_run") },
			"ask.human_steps[0].dry_run is required for a command; say the host shape you ran it on and what it printed"},
		{"dry_run without host", func(ask map[string]any) {
			firstStep(ask)["dry_run"] = map[string]any{"result": "exit 0"}
		}, "ask.human_steps[0].dry_run.host is required"},
		{"dry_run without result", func(ask map[string]any) {
			firstStep(ask)["dry_run"] = map[string]any{"host": "x99 linux bash"}
		}, "ask.human_steps[0].dry_run.result is required"},
		{"dry_run says not run", func(ask map[string]any) {
			firstStep(ask)["dry_run"] = map[string]any{"host": "macOS zsh", "result": "Not run: no Mac in reach"}
		}, "ask.human_steps[0].dry_run.result says the command was not run; run it on the target host shape first"},
		{"dry_run not an object", func(ask map[string]any) { firstStep(ask)["dry_run"] = "ok" },
			"ask.human_steps[0].dry_run must be an object with host, result"},

		// (d) placeholders the human must edit
		{"ellipsis in command", func(ask map[string]any) {
			firstStep(ask)["command"] = "gh secret set MULTICA_SERVER_URL …"
		}, "ask.human_steps[0].command has a placeholder (…); write the literal the human runs"},
		{"three dots after a space in command", func(ask map[string]any) {
			firstStep(ask)["command"] = "gh secret set MULTICA_SERVER_URL ..."
		}, "ask.human_steps[0].command has a placeholder (...); write the literal the human runs"},
		{"declared angle placeholder in command", func(ask map[string]any) {
			step := firstStep(ask)
			step["command"] = "gh secret set X --body <token>"
			step["values"] = []any{map[string]any{"name": "<token>", "value": "abc"}}
		}, "ask.human_steps[0].command has a placeholder (<token>); write the literal the human runs"},
		{"YOUR_ word in command", func(ask map[string]any) {
			step := firstStep(ask)
			step["command"] = "gh secret set MULTICA_SERVER_URL --body YOUR_URL"
			step["values"] = []any{map[string]any{"name": "MULTICA_SERVER_URL and YOUR_URL", "value": "x"}}
		}, "ask.human_steps[0].command has a placeholder (YOUR_URL); write the literal the human runs"},
		{"TODO in command", func(ask map[string]any) {
			firstStep(ask)["command"] = "gh secret set MULTICA_SERVER_URL --body TODO"
		}, "ask.human_steps[0].command has a placeholder (TODO); write the literal the human runs"},
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

func TestHumanAskGuardAccepts(test *testing.T) {
	cases := map[string]func(map[string]any){
		"ui path with observed": func(ask map[string]any) {
			step := firstStep(ask)
			step["action"] = "Open Settings > API Tokens"
			step["observed"] = "web app 19443, Settings page, seen 14 Nov 2026 20:10 ICT"
		},
		"repo file with repo and sha": func(ask map[string]any) {
			step := firstStep(ask)
			step["command"] = "bash scripts/run-checks.sh MULTICA_SERVER_URL"
			step["files"] = []any{map[string]any{"path": "scripts/run-checks.sh", "repo": "cheese-work/multica", "sha": goodSHA}}
		},
		"file from an attachment": func(ask map[string]any) {
			step := firstStep(ask)
			step["command"] = "bash scripts/run-checks.sh MULTICA_SERVER_URL"
			step["files"] = []any{map[string]any{"path": "scripts/run-checks.sh", "attachment_id": "01a1ca54-f86b-7081-8cf6-bef63814cfb6"}}
		},
		"repo slug is not a file": func(ask map[string]any) {
			firstStep(ask)["command"] = "gh secret set MULTICA_SERVER_URL -R cheese-work/multica-dotfiles"
		},
		"url with a file path is not a repo file": func(ask map[string]any) {
			firstStep(ask)["command"] = "curl https://example.com/files/run.sh MULTICA_SERVER_URL"
		},
		"three-dot git range": func(ask map[string]any) {
			firstStep(ask)["command"] = "git diff main...HEAD MULTICA_SERVER_URL"
		},
		"absolute and home paths are the human's own host": func(ask map[string]any) {
			firstStep(ask)["command"] = "bash ~/Downloads/run.sh /opt/tools/a.sh MULTICA_SERVER_URL"
		},
		"step with no command needs no dry_run": func(ask map[string]any) {
			step := firstStep(ask)
			delete(step, "command")
			delete(step, "dry_run")
			step["values"] = []any{map[string]any{"name": "MULTICA_SERVER_URL", "value": "https://congvc-c00.taila6fa8a.ts.net:19444"}}
		},
		"dry_run that mentions a failed first attempt": func(ask map[string]any) {
			firstStep(ask)["dry_run"] = map[string]any{"host": "x99 linux bash", "result": "exit 0 after fixing a missing --repo flag; the first try was not accepted by gh"}
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

func TestContentRendersHumanAskEvidence(test *testing.T) {
	raw := stepsAsk(test, "pass")
	step := firstStep(raw)
	step["observed"] = "web app 19443, seen 14 Nov 2026 20:10 ICT"
	step["files"] = []any{map[string]any{"path": "scripts/run-checks.sh", "repo": "cheese-work/multica", "sha": goodSHA}}
	step["command"] = "bash scripts/run-checks.sh MULTICA_SERVER_URL"
	ask, err := decodeMap(raw)
	if err != nil {
		test.Fatal(err)
	}
	content := ask.Content("acme")
	for _, want := range []string{
		"- Seen at: web app 19443, seen 14 Nov 2026 20:10 ICT",
		"- File: scripts/run-checks.sh in cheese-work/multica at " + goodSHA,
		"- Dry run on x99 linux bash, gh 2.x: ran with",
	} {
		if !strings.Contains(content, want) {
			test.Fatalf("%q missing in %q", want, content)
		}
	}
}

// The action kind (`multica decision request --kind action`) carries its steps
// as plain strings; the same four classes are refused there (CHE-5753 review round 1).
func TestActionAskGuardRejects(test *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"ui path without observed", func(ask map[string]any) {
			ask["steps"] = []any{"Open Workspace settings → Delegated governed writes and switch it on"}
		}, "ask.steps cites a UI path with no observed; say where and when you saw the control"},
		{"repo file without files entry", func(ask map[string]any) {
			ask["steps"] = []any{"Edit docs/setup/runner.md"}
		}, "ask.steps names the file docs/setup/runner.md without a files entry with repo and a 40-character commit SHA"},
		{"backtick root file without files entry", func(ask map[string]any) {
			ask["steps"] = []any{"Open `setup.sh`"}
		}, "ask.steps names the file setup.sh without a files entry with repo and a 40-character commit SHA"},
		{"command without dry_run", func(ask map[string]any) {
			delete(ask, "dry_run")
		}, "ask.dry_run is required for a command; say the host shape you ran it on and what it printed"},
		{"bare run verb without dry_run", func(ask map[string]any) {
			ask["steps"] = []any{"Run bash setup.sh"}
			ask["files"] = []any{map[string]any{"path": "setup.sh", "repo": "cheese-work/multica", "sha": goodSHA}}
			delete(ask, "dry_run")
		}, "ask.dry_run is required for a command; say the host shape you ran it on and what it printed"},
		{"dry_run says not run", func(ask map[string]any) {
			ask["dry_run"] = map[string]any{"host": "macOS zsh", "result": "not run"}
		}, "ask.dry_run.result says the command was not run; run it on the target host shape first"},
		{"placeholder in a command", func(ask map[string]any) {
			ask["steps"] = []any{"Run `multica property create --name <name>`."}
		}, "ask.steps has a placeholder (<name>); write the literal the human runs"},
		{"file without sha", func(ask map[string]any) {
			ask["steps"] = []any{"Edit docs/setup/runner.md"}
			ask["files"] = []any{map[string]any{"path": "docs/setup/runner.md", "repo": "cheese-work/multica"}}
		}, "ask.files[0] needs a repo and sha, or an attachment_id"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			ask := validActionAskMap()
			tc.mutate(ask)
			raw, _ := json.Marshal(ask)
			_, err := DecodeAskKind(raw, KindAction)
			if got := errString(err); got != tc.want {
				test.Fatalf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestActionAskGuardAccepts(test *testing.T) {
	cases := map[string]func(map[string]any){
		"ui path with observed": func(ask map[string]any) {
			ask["steps"] = []any{"Open Settings > API Tokens"}
			ask["observed"] = "web app 19443, Settings page, seen 14 Nov 2026 20:10 ICT"
		},
		"repo file with repo and sha": func(ask map[string]any) {
			ask["steps"] = []any{"Edit `docs/setup/runner.md`"}
			ask["files"] = []any{map[string]any{"path": "docs/setup/runner.md", "repo": "cheese-work/multica", "sha": goodSHA}}
		},
		"steps with no command need no dry_run": func(ask map[string]any) {
			ask["steps"] = []any{"Open the issue page and press Approve."}
			delete(ask, "dry_run")
		},
	}
	for name, mutate := range cases {
		test.Run(name, func(test *testing.T) {
			ask := validActionAskMap()
			mutate(ask)
			raw, _ := json.Marshal(ask)
			if _, err := DecodeAskKind(raw, KindAction); err != nil {
				test.Fatal(err)
			}
		})
	}
}
