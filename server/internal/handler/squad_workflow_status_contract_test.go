package handler

import (
	"strings"
	"testing"
)

// Contract tests for MUL-8145, in the style of
// squad_parent_status_contract_test.go. When the issue follows a project
// workflow, the squad protocol (Agent Identity, which outranks the runtime
// brief) must move the parent only through [Project workflow] and must not
// tell the owning leader to make the built-in in_progress / in_review moves,
// which the workflow may not list and which can hand the parent to another
// step's handler mid-dispatch.
//
// Fork note: upstream also asserts the daemon runtime brief half
// (execenv.TaskContextForEnv.HasProjectWorkflow). The fork's daemon does not
// carry that half and does not advertise
// protocol.DaemonCapabilityWorkflowStatusRulesV1, so a fork claim always takes
// leaderWorkflowLegacyDaemon and the protocol overrides the runtime brief's
// built-in moves itself. See docs/fork/UPSTREAM-OVERLAP.md G23.
//
// They use the protocol text directly rather than a seeded squad, so they need
// no database; the roster is irrelevant to the status contract.

// builtInParentMoves are the built-in status moves a leader must not be told
// to make on a workflow issue, in either half of its instructions.
var builtInParentMoves = []string{
	"multica issue status <issue-id> in_review",
	"move the parent to `in_progress`",
	"set `in_progress` FIRST",
	"moves to `in_review`",
	"always lands here",
}

func TestOwningLeaderInWorkflowMovesOnlyByWorkflowCommands(t *testing.T) {
	for _, workflow := range []leaderWorkflow{leaderWorkflowCurrentDaemon, leaderWorkflowLegacyDaemon} {
		combined := strings.Join(strings.Fields(squadOperatingProtocolFor(true, workflow)), " ")
		provider := map[leaderWorkflow]string{leaderWorkflowCurrentDaemon: "current daemon", leaderWorkflowLegacyDaemon: "legacy daemon"}[workflow]
		for _, move := range builtInParentMoves {
			if strings.Contains(combined, move) {
				t.Errorf("%s: workflow leader instructions still carry built-in move %q", provider, move)
			}
		}
		for _, want := range []string{
			// The ownership grant and its standing wrap-up authorization survive.
			"Own the parent issue status — through its project workflow.",
			"Delegate only the current step's work",
			"pass its instructions and constraints to members",
			"confirm the current step's goal is met",
			"run that block's Step done command",
			"do it even when no comment asked you to",
			// Dispatch is not delivery, in workflow terms.
			"The parent stays at its current step while members work",
		} {
			if !strings.Contains(combined, want) {
				t.Errorf("%s: workflow leader instructions missing %q", provider, want)
			}
		}
	}
}

// Without a workflow the owning leader keeps the built-in arc, so issues
// outside a handoff workflow see no change.
func TestOwningLeaderWithoutWorkflowKeepsBuiltInMoves(t *testing.T) {
	protocol := squadOperatingProtocolFor(true, leaderNoWorkflow)
	if strings.Contains(protocol, "through its project workflow") {
		t.Fatalf("owner protocol without a workflow must be the built-in form:\n%s", protocol)
	}
	compact := strings.Join(strings.Fields(protocol), " ")
	for _, want := range []string{"move the parent to `in_progress`", "multica issue status <issue-id> in_review"} {
		if !strings.Contains(compact, want) {
			t.Errorf("owner protocol without a workflow lost %q", want)
		}
	}
}

// A guest leader gets the prohibition whether or not the issue has a
// workflow: the workflow never widens status authority.
func TestGuestLeaderProtocolIgnoresWorkflow(t *testing.T) {
	for _, workflow := range []leaderWorkflow{leaderWorkflowCurrentDaemon, leaderWorkflowLegacyDaemon, leaderWorkflowStepMoved} {
		if squadOperatingProtocolFor(false, workflow) != squadOperatingProtocolFor(false, leaderNoWorkflow) {
			t.Fatal("guest leader protocol must not vary with the project workflow")
		}
	}
	if strings.Contains(squadOperatingProtocolFor(false, leaderWorkflowCurrentDaemon), "Step done") {
		t.Fatal("guest leader protocol must not hand out the Step done move")
	}
}

// A current daemon's runtime brief states no built-in moves, so the protocol
// forbids none; only an older daemon's brief, which still states them, gets
// the override (MUL-8145).
func TestOwningLeaderInWorkflowOverridesBuiltInMovesOnlyForLegacyDaemons(t *testing.T) {
	current := strings.Join(strings.Fields(squadOperatingProtocolFor(true, leaderWorkflowCurrentDaemon)), " ")
	legacy := strings.Join(strings.Fields(squadOperatingProtocolFor(true, leaderWorkflowLegacyDaemon)), " ")
	for _, word := range []string{"in_progress", "in_review"} {
		if strings.Contains(current, word) {
			t.Errorf("current-daemon protocol names %q, which nothing in its brief asks for", word)
		}
	}
	if !strings.Contains(legacy, "This overrides the in_progress / in_review rules in your runtime instructions") {
		t.Errorf("legacy-daemon protocol must override the built-in moves:\n%s", legacy)
	}
	if !strings.Contains(legacy, strings.Join(strings.Fields(squadParentStatusOwnedInWorkflow), " ")) {
		t.Errorf("legacy-daemon protocol must keep the workflow responsibility it extends")
	}
}

// A run whose workflow binding no longer matches the issue leaves status
// alone, even for the owning leader; a guest never sees the rule.
func TestOwningLeaderWithMovedStepLeavesStatusUnchanged(t *testing.T) {
	owner := strings.Join(strings.Fields(squadOperatingProtocolFor(true, leaderWorkflowStepMoved)), " ")
	if !strings.Contains(owner, "Leave the parent issue's status unchanged for this run") {
		t.Errorf("moved-step owner protocol must hold status:\n%s", owner)
	}
	for _, move := range builtInParentMoves {
		if strings.Contains(owner, move) {
			t.Errorf("moved-step owner protocol still carries built-in move %q", move)
		}
	}
	if strings.Contains(owner, "Step done") {
		t.Error("moved-step owner protocol must not hand out the Step done move")
	}
}
