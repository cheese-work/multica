package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

// Fork behaviours kept where the upstream workflow CLI (MUL-8386) met the
// fork's one: decision-bound step edits (CHE-2448), run-rule flags, and the
// machine-token boundary on create, delete and project workflow set.

func newHandoffDecisionCmd() *cobra.Command {
	cmd := newWorkflowTestCmd("handler", "instructions", "instructions-file", "next", "back", "copy-for-project", "decision-id")
	cmd.Flags().Bool("print-hash", false, "")
	return cmd
}

func handoffRecorder(t *testing.T, put *map[string]any, puts *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(hashFixtureWorkflow())
			return
		}
		if r.Method != http.MethodPut || r.URL.Path != "/api/issue-workflows/"+testWorkflowID+"/handoffs/todo" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		atomic.AddInt32(puts, 1)
		_ = json.NewDecoder(r.Body).Decode(put)
		_ = json.NewEncoder(w).Encode(hashFixtureWorkflow())
	}))
	setCLITestServerEnv(t, srv.URL)
	return srv
}

func TestWorkflowHandoffSetDecisionIDSendsBoundBody(t *testing.T) {
	var put map[string]any
	var puts int32
	defer handoffRecorder(t, &put, &puts).Close()

	cmd := newHandoffDecisionCmd()
	_ = cmd.Flags().Set("instructions", "S1 new")
	_ = cmd.Flags().Set("decision-id", testDecisionID)
	if _, err := captureStdout(t, func() error { return runWorkflowHandoffSet(cmd, []string{testWorkflowID, "todo"}) }); err != nil {
		t.Fatal(err)
	}
	if put["decision_id"] != testDecisionID {
		t.Errorf("decision_id = %v", put["decision_id"])
	}
	if put["before_sha256"] != sha256Hex(hashFixtureCanonical) || put["after_sha256"] != sha256Hex(hashFixtureAfter) {
		t.Errorf("hashes = %v / %v, want %s / %s", put["before_sha256"], put["after_sha256"], sha256Hex(hashFixtureCanonical), sha256Hex(hashFixtureAfter))
	}
}

func TestWorkflowHandoffSetPrintHashWritesNothing(t *testing.T) {
	var put map[string]any
	var puts int32
	defer handoffRecorder(t, &put, &puts).Close()

	cmd := newHandoffDecisionCmd()
	_ = cmd.Flags().Set("instructions", "S1 new")
	_ = cmd.Flags().Set("print-hash", "true")
	out, err := captureStdout(t, func() error { return runWorkflowHandoffSet(cmd, []string{testWorkflowID, "todo"}) })
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&puts) != 0 {
		t.Errorf("PUT calls = %d, want 0", puts)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", out, err)
	}
	if got["before_sha256"] != sha256Hex(hashFixtureCanonical) || got["after_sha256"] != sha256Hex(hashFixtureAfter) {
		t.Errorf("hashes = %v", got)
	}
}

func TestWorkflowDecisionBoundWriteRefusesOwnerOnlyFlagsBeforeAnyCall(t *testing.T) {
	var put map[string]any
	var puts int32
	var gets int32
	srv := handoffRecorder(t, &put, &puts)
	defer srv.Close()
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&gets, 1)
		srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer counting.Close()
	setCLITestServerEnv(t, counting.URL)

	handoff := newHandoffDecisionCmd()
	_ = handoff.Flags().Set("decision-id", testDecisionID)
	_ = handoff.Flags().Set("copy-for-project", "44444444-4444-4444-4444-444444444444")
	if err := runWorkflowHandoffSet(handoff, []string{testWorkflowID, "todo"}); err == nil || !strings.Contains(err.Error(), "--copy-for-project") {
		t.Errorf("handoff err = %v, want --copy-for-project refused", err)
	}

	update := newWorkflowDecisionCmd()
	update.Flags().String("name", "", "")
	_ = update.Flags().Set("steps-file", writeTemp(t, `[{"status_key":"todo"}]`))
	_ = update.Flags().Set("name", "Renamed")
	_ = update.Flags().Set("decision-id", testDecisionID)
	if err := runWorkflowUpdate(update, []string{testWorkflowID}); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Errorf("update err = %v, want --name refused", err)
	}
	if n := atomic.LoadInt32(&gets) + atomic.LoadInt32(&puts); n != 0 {
		t.Errorf("HTTP calls = %d, want 0", n)
	}
}

// A ref that is not a UUID resolves to the workflow, and the run-rule flags
// still reach the PATCH.
func TestWorkflowUpdateResolvesNameAndKeepsRunRules(t *testing.T) {
	_, reqs := workflowTestServer(t)
	cmd := newWorkflowRulesCmd()
	cmd.Flags().String("name", "", "")
	_ = cmd.Flags().Set("max-runs-per-issue", "5")

	if _, err := captureStdout(t, func() error { return runWorkflowUpdate(cmd, []string{"Review flow"}) }); err != nil {
		t.Fatal(err)
	}
	patch := lastRequest(t, *reqs, http.MethodPatch)
	if patch.Path != "/api/issue-workflows/"+testWorkflowUUID || patch.Body["max_runs_per_issue"] != float64(5) {
		t.Fatalf("patch = %+v, want max_runs_per_issue=5 on the resolved workflow", patch)
	}
}

func TestDecisionFlagsOnStepEditsOnly(t *testing.T) {
	// Looked up through the command tree, not a package symbol, so a take-upstream
	// resolution that drops the verb fails on behaviour instead of compiling out.
	setInstructions, _, _ := workflowCmd.Find([]string{"set-instructions"})
	if setInstructions == nil || setInstructions == workflowCmd {
		t.Fatal("workflow set-instructions verb is missing")
	}
	for _, c := range []*cobra.Command{workflowUpdateCmd, setInstructions, workflowHandoffSetCmd} {
		if c.Flags().Lookup("decision-id") == nil || c.Flags().Lookup("print-hash") == nil {
			t.Errorf("%s lacks --decision-id/--print-hash", c.CommandPath())
		}
	}
	for _, c := range []*cobra.Command{workflowCreateCmd, workflowDeleteCmd, projectWorkflowSetCmd, projectWorkflowGetCmd} {
		if c.Flags().Lookup("decision-id") != nil || c.Flags().Lookup("print-hash") != nil {
			t.Errorf("%s must not take a decision binding", c.CommandPath())
		}
	}
}
