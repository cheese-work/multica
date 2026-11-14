package main

import (
	"strings"
	"sync/atomic"
	"testing"
)

// CHE-5843: --decision-id on a field only a passkey action can write stops
// before any API call with one line that names the working command.

func wantPasskeyOnlyRefusal(t *testing.T, err error, command string, calls int32) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "--decision-id cannot authorize this write") || !strings.Contains(err.Error(), command) || strings.Contains(err.Error(), "\n") {
		t.Fatalf("err = %v, want one line naming %q", err, command)
	}
	if calls != 0 {
		t.Errorf("HTTP calls = %d, want 0", calls)
	}
}

func TestWorkflowSetInstructionsDecisionIDRefusedBeforeAnyCall(t *testing.T) {
	var patched map[string]any
	var patches int32
	srv := workflowDecisionServer(t, &patched, &patches)
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newWorkflowDecisionCmd()
	_ = cmd.Flags().Set("status", "todo")
	_ = cmd.Flags().Set("file", writeTemp(t, "S1 new"))
	_ = cmd.Flags().Set("decision-id", testDecisionID)
	err := runWorkflowSetInstructions(cmd, []string{testWorkflowID})
	wantPasskeyOnlyRefusal(t, err, "multica config propose workflow.step.instructions "+testWorkflowID+" --status-key todo --file <path>", atomic.LoadInt32(&patches))
}

func TestWorkflowUpdateStepsDecisionIDRefusedBeforeAnyCall(t *testing.T) {
	var patched map[string]any
	var patches int32
	srv := workflowDecisionServer(t, &patched, &patches)
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newWorkflowDecisionCmd()
	_ = cmd.Flags().Set("steps-file", writeTemp(t, `[{"status_key":"todo"}]`))
	_ = cmd.Flags().Set("decision-id", testDecisionID)
	err := runWorkflowUpdate(cmd, []string{testWorkflowID})
	wantPasskeyOnlyRefusal(t, err, "multica config propose workflow.steps "+testWorkflowID+" --file <path>", atomic.LoadInt32(&patches))
}

func TestInstructionFieldsDecisionIDRefusedBeforeAnyCall(t *testing.T) {
	for _, tc := range []struct {
		name, command, server string
	}{
		{"agent", "multica config propose agent.instructions agent-123 --file <path>", "agent"},
		{"squad", "multica config propose squad.instructions squad-123 --file <path>", "squad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			var calls int32
			srv := voteTestServer(t, tc.server, &body, &calls)
			defer srv.Close()
			setSquadUpdateServerEnv(t, srv.URL)
			var err error
			if tc.name == "agent" {
				cmd := addDecisionFlags(newAgentCASTestCmd())
				_ = cmd.Flags().Set("instructions-file", writeCASFile(t, "text\n"))
				_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
				_ = cmd.Flags().Set("decision-id", testDecisionID)
				err = runAgentUpdate(cmd, []string{"agent-123"})
			} else {
				cmd := addDecisionFlags(newSquadUpdateTestCmd())
				_ = cmd.Flags().Set("instructions-file", writeCASFile(t, "text"))
				_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
				_ = cmd.Flags().Set("decision-id", testDecisionID)
				err = runSquadUpdate(cmd, []string{"squad-123"})
			}
			wantPasskeyOnlyRefusal(t, err, tc.command, atomic.LoadInt32(&calls))
		})
	}
}

func TestWorkspaceUpdateDecisionIDRefusedBeforeAnyCall(t *testing.T) {
	var body map[string]any
	var calls int32
	srv := voteTestServer(t, "workspace", &body, &calls)
	defer srv.Close()
	setWorkspaceUpdateServerEnv(t, srv.URL)

	cmd := addDecisionFlags(newWorkspaceVoteTestCmd())
	_ = cmd.Flags().Set("context-file", writeCASFile(t, "context text\n"))
	_ = cmd.Flags().Set("expected-before-digest", testDigestHex)
	_ = cmd.Flags().Set("decision-id", testDecisionID)
	err := runWorkspaceUpdate(cmd, []string{testWorkspaceUUID})
	wantPasskeyOnlyRefusal(t, err, "multica config propose workspace.context --file <path>", atomic.LoadInt32(&calls))
}
