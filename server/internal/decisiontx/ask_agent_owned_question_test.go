package decisiontx

import "testing"

// CHE-10344 (CHE-7035 R3): the question and the options may not hand a human a
// merge-queue, re-queue, rebase or re-run-checks step. These are the four asks
// that reached Cheese in the S8 window although an agent owns the step.
var agentOwnedQuestions = []struct{ name, decision, what string }{
	{"che8760 requeue", "Do you re-queue PR 2022 for merge now?", "You add PR 2022 to the merge queue again on the same head."},
	{"che8760 requeue same head", "Do you re-queue PR 2022 for merge now, on the same head?", "You add PR 2022 to the merge queue again on the same head."},
	{"che8760 who adds to queue", "Who adds PR 2022 to the merge queue on head a51329774b?", "You add PR 2022 to the merge queue on the same head."},
	{"che2948 agent rebase", "May an agent resolve the conflict on the pull request 21 branch (rebase onto main), re-run the checks and get a fresh review on the new head, so you can then merge it as you chose?", "An agent resolves the conflict, pushes the new head and runs the checks."},
	{"merge-queue spelling", "Should the PR go into the merge-queue?", "The PR is queued."},
	{"requeue verb forms", "Has PR 7 been re-queued?", "It is re-queued."},
	{"rebasing", "Is rebasing the branch fine?", "The branch is rebased."},
	{"rerun ci", "Should CI be re-run on PR 7?", "CI runs again."},
	{"rerun checks", "Do you rerun the failed checks?", "The checks run again."},
	{"human step in an option", "Ship PR 7?", "You re-queue PR 7 and wait for the queue run."},
	{"human step in an option, rebase", "Ship PR 7?", "You rebase the branch onto main."},
	{"human step in an option, rerun", "Ship PR 7?", "You re-run the failed checks."},
}

func TestDecisionAsksRefuseAgentOwnedQueueAndRebaseSteps(test *testing.T) {
	for _, tc := range agentOwnedQuestions {
		test.Run(tc.name, func(test *testing.T) {
			ask := validAskMap()
			ask["decision"] = tc.decision
			ask["options"].([]any)[0].(map[string]any)["what_happens"] = tc.what
			_, err := decodeMap(ask)
			got := errString(err)
			if got != "ask.decision "+agentOwnedMessage && got != "ask.options[0].what_happens "+agentOwnedMessage {
				test.Fatalf("error = %q", got)
			}
		})
	}
}

// The escalation classes stay askable even when the text names a queue, a
// rebase or a re-run: irreversible migration, C00 config or secrets, public
// publication, spend, design PR, release branch.
func TestDecisionAsksAllowEscalationClassQueueAndRebaseSteps(test *testing.T) {
	for _, tc := range []struct{ name, decision, what string }{
		{"irreversible migration", "Do you re-queue PR 9, which drops a column (an irreversible migration), for merge?", "You add PR 9 to the merge queue."},
		{"c00 config", "Do you re-queue PR 12, which changes C00 config, for merge?", "You add PR 12 to the merge queue."},
		{"c00 secrets", "Do you re-run the checks after the secrets rotation?", "The checks run again."},
		{"public publication", "May we re-queue the PR that publishes the public release notes?", "You add the PR to the merge queue."},
		{"spend", "Do you approve $40 of runner time to re-run the checks?", "The checks run again."},
		{"design pr", "Do you merge the design lock PR after it was rebased?", "You merge the design PR."},
		{"release branch", "Do you approve the rebase of release/1.2 onto the new tag?", "The release branch is rebased."},
		{"production deploy", "Do you approve the production deploy after the queue run?", "You approve the deploy."},
		{"escalation named only in an option", "Do you re-queue PR 12?", "You approve the spend of one more queue run."},
	} {
		test.Run(tc.name, func(test *testing.T) {
			ask := validAskMap()
			ask["decision"] = tc.decision
			ask["options"].([]any)[0].(map[string]any)["what_happens"] = tc.what
			if _, err := decodeMap(ask); err != nil {
				test.Fatal(err)
			}
		})
	}
}

// Asks that mention these words without handing the step to a human stay valid.
func TestDecisionAsksAllowAgentDoneQueueAndRebaseSteps(test *testing.T) {
	for _, tc := range []struct{ name, decision, what string }{
		{"ship", "Ship now?", "I re-run the checks and rebase the branch after your pick."},
		{"agent re-queues", "Which fix do you pick?", "An agent re-queues the PR after the fix."},
		{"discusses a queue", "Do you want the queue rank score in the first release?", "The score is added to the merge queue view."},
		{"you pick", "Which branch name do you choose?", "You pick the name; the agent rebases nothing."},
		{"requeue-free", "Do you accept the flaky test as known?", "You accept it; a re-run of the checks is not needed."},
	} {
		test.Run(tc.name, func(test *testing.T) {
			ask := validAskMap()
			ask["decision"] = tc.decision
			ask["options"].([]any)[0].(map[string]any)["what_happens"] = tc.what
			if _, err := decodeMap(ask); err != nil {
				test.Fatal(err)
			}
		})
	}
}
