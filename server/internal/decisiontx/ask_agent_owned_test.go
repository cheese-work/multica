package decisiontx

import (
	"encoding/json"
	"testing"
)

// CHE-4869: an ask may not hand a human a step an agent owns under Standing
// Delivery Authorization. The five escalation classes stay askable.
var agentOwnedActions = []struct{ name, text string }{
	{"click ready for review", "Click Ready for review on PR #7"},
	{"mark ready", "Mark the PR ready"},
	{"undraft", "Undraft the PR"},
	{"gh pr ready", "Run `gh pr ready 7`"},
	{"retarget", "Retarget PR #7 to the parent branch"},
	{"retarget by command", "Run `gh pr edit 7 --base develop`"},
	{"rebase", "Rebase the branch onto the parent"},
	{"rerun ci", "Re-run the failed checks"},
	{"rerun by command", "Run `gh run rerun 123`"},
	{"close issue", "Close CHE-4869"},
	{"link issue", "Link PR #7 to the issue"},
	{"merge into develop", "Merge PR #7 into develop"},
	{"merge into a named branch", "Merge PR #7 into `release/1.2`"},
	{"dotted source branch", "Merge PR #7 from release/1.2 into develop"},
	{"dotted target branch", "Merge PR #7 into feature/foo.bar"},
	{"version tag target", "Merge PR #7 into v1.2.3-rc.1"},
	{"sentence-ending target", "Merge PR #7 into develop."},
	{"dotted branch between branches", "Merge release/1.2 into release/1.3."},
	{"named default without repository context", "Merge PR #7 into main"},
	{"capitalised default name", "Merge PR #7 into Main."},
	{"default name with suffix", "Merge PR #7 into main.release."},
	{"bare default word", "Merge PR #7 into default."},
	{"merge without a destination", "Merge PR #7"},
	{"merge without a destination, sentence end", "Merge PR #7."},
	{"second merge after an allowed first", "Merge PR #7 into the default branch; merge PR #8 into develop."},
	{"second merge after a sentence end", "Merge PR #7 into the default branch. Merge PR #8 into develop."},
	{"second merge without a destination", "Merge PR #7 into the default branch, then merge PR #8."},
	{"vietnamese merge without a destination", "Gộp PR #7"},
	{"retarget by short flag", "Run gh pr edit 7 -B develop"},
	{"retarget by short flag in backticks", "Run `gh pr edit 7 -B develop`"},
	{"retarget by short flag with equals", "Run `gh pr edit 7 -B=develop`"},
	{"retarget by short flag after another flag", "Run `gh pr edit 7 --title x -B develop`"},
	{"default phrase in a parenthetical before another destination", "Merge PR #7 (originally meant to go into the default branch) into develop"},
	{"default phrase in a parenthetical after another destination", "Merge PR #7 into develop (originally meant to go into the default branch)."},
	{"default branch named as the source", "Merge PR #7 (the default branch) into develop."},
	{"two destinations in one clause", "Merge PR #7 into the default branch, then into develop."},
	{"mark dotted branch ready", "Mark release/1.2 ready"},
	{"rerun on dotted branch", "Re-run the checks on release/1.2"},
}

func TestHumanStepsRefuseAgentOwnedActions(test *testing.T) {
	for _, tc := range agentOwnedActions {
		for _, field := range []string{"action", "command"} {
			test.Run(tc.name+" in "+field, func(test *testing.T) {
				ask := stepsAsk(test, "pass")
				step := firstStep(ask)
				step["action"] = "Set the secret"
				step["command"] = "echo MULTICA_SERVER_URL"
				step[field] = tc.text + " MULTICA_SERVER_URL"
				_, err := decodeMap(ask)
				if got := errString(err); got != "ask.human_steps[0]."+field+" is an agent-owned step; do it yourself and ask a human only for spend, production deploy or release, public publication, an irreversible action or an open product choice" {
					test.Fatalf("error = %q", got)
				}
			})
		}
	}
}

func TestActionStepsRefuseAgentOwnedActions(test *testing.T) {
	for _, tc := range agentOwnedActions {
		test.Run(tc.name, func(test *testing.T) {
			ask := validActionAskMap()
			ask["steps"] = []any{"Open the dashboard.", tc.text}
			raw, _ := json.Marshal(ask)
			_, err := DecodeAskKind(raw, KindAction)
			if got := errString(err); got != "ask.steps[1] is an agent-owned step; do it yourself and ask a human only for spend, production deploy or release, public publication, an irreversible action or an open product choice" {
				test.Fatalf("error = %q", got)
			}
		})
	}
}

func TestAgentOwnedAcceptsEscalationClassSteps(test *testing.T) {
	for _, text := range []string{
		"Merge PR #7 into the default branch",
		"Merge PR #7 from release/1.2 into the default branch",
		"Merge PR #7 from feature/foo.bar into the default branch.",
		"Merge PR #7 into the default branch; merge PR #8 into the default branch.",
		"Merge PR #7 into the default branch, using `gh pr merge 7 --squash`.",
		"Run `gh pr merge 7`",
		"Run `gh pr edit 7 -b 'new body'`",
		"Merge the 4 PRs into the default branch, then start slice 5.",
		"Approve the production deploy of the multica fork",
		"Publish the release notes to the public site",
		"Run `multica property create --name Readiness`",
		"Close the browser tab",
		"Pay the invoice",
	} {
		test.Run(text, func(test *testing.T) {
			ask := validActionAskMap()
			ask["steps"] = []any{text}
			raw, _ := json.Marshal(ask)
			if _, err := DecodeAskKind(raw, KindAction); err != nil {
				test.Fatal(err)
			}
		})
	}
}
