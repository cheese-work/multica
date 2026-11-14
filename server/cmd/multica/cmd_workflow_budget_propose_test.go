package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

// CHE-5504: with a task token, "workflow update" budget flags open the
// workflow.budget.set card in one call instead of the 3-step digest path.

const budgetApproverID = "33333333-3333-3333-3333-333333333333"

// budgetFlow records what the CLI sent: proposal bodies, writes and any other
// request. The server mirrors main for a machine PATCH: budget_requires_decision.
type budgetFlow struct {
	mu        sync.Mutex
	proposals []map[string]any
	patches   int
	other     []string
}

func (f *budgetFlow) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.proposals) + f.patches + len(f.other)
}

func budgetFlowServer(t *testing.T) *budgetFlow {
	t.Helper()
	flow := &budgetFlow{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flow.mu.Lock()
		defer flow.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/issue-workflows/"+testWorkflowID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testWorkflowID, "max_runs_per_issue": 15, "include_child_prs": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/decision-actions/proposals":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			flow.proposals = append(flow.proposals, body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "req-1", "action": map[string]any{"id": "act-1"}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/ws-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"slug": "acme"})
		case r.Method == http.MethodPatch:
			flow.patches++
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"budget_requires_decision"}`))
		default:
			flow.other = append(flow.other, r.Method+" "+r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	return flow
}

func newWorkflowBudgetCmd(set map[string]string) *cobra.Command {
	cmd := newWorkflowTestCmd("name", "description", "steps-file", "start-status", "decision-id", "issue", "for")
	cmd.Flags().StringArray("map", nil, "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().Int("max-runs-per-issue", 0, "")
	cmd.Flags().Bool("include-child-prs", true, "")
	cmd.Flags().Bool("print-hash", false, "")
	for k, v := range set {
		if err := cmd.Flags().Set(k, v); err != nil {
			panic(err)
		}
	}
	return cmd
}

var proposeBudget = map[string]string{"max-runs-per-issue": "40", "issue": testIssueUUID, "for": budgetApproverID}

func TestWorkflowUpdateBudgetTaskTokenProposesTheCard(t *testing.T) {
	flow := budgetFlowServer(t)
	out, err := captureStdout(t, func() error {
		return runWorkflowUpdate(newWorkflowBudgetCmd(proposeBudget), []string{testWorkflowID})
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if flow.patches != 0 {
		t.Errorf("PATCH calls = %d, want 0: the workflow stays unchanged until approval", flow.patches)
	}
	live := 15
	baseDigest := decisionaction.WorkflowBudgetDigest(testWorkflowID, &live, true)
	if len(flow.proposals) != 1 {
		t.Fatalf("proposals = %d, want 1", len(flow.proposals))
	}
	want := map[string]any{
		"action_type": "workflow.budget.set", "version": float64(1), "issue_id": testIssueUUID, "for": budgetApproverID,
		"params": map[string]any{"workflow_id": testWorkflowID, "base_sha256": baseDigest, "max_runs_per_issue": float64(40)},
	}
	if !reflect.DeepEqual(flow.proposals[0], want) {
		t.Errorf("proposal = %v, want %v (only the named budget field)", flow.proposals[0], want)
	}
	if !strings.Contains(out, "/acme/issues/"+testIssueUUID+"?approve=act-1") {
		t.Errorf("stdout %q lacks the card URL", out)
	}
}

func TestWorkflowUpdateBudgetRepeatSendsTheSameProposal(t *testing.T) {
	flow := budgetFlowServer(t)
	var outs []string
	for i := 0; i < 2; i++ {
		out, err := captureStdout(t, func() error {
			return runWorkflowUpdate(newWorkflowBudgetCmd(proposeBudget), []string{testWorkflowID})
		})
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		outs = append(outs, out)
	}
	// The server's proposal fingerprint replays the open card for an equal body.
	if len(flow.proposals) != 2 || !reflect.DeepEqual(flow.proposals[0], flow.proposals[1]) || outs[0] != outs[1] {
		t.Errorf("repeat differs: bodies %v, outputs %q", flow.proposals, outs)
	}
}

func TestWorkflowUpdateBudgetChildPRsAloneSendsOnlyThatField(t *testing.T) {
	flow := budgetFlowServer(t)
	set := map[string]string{"include-child-prs": "false", "issue": testIssueUUID, "for": budgetApproverID}
	if _, err := captureStdout(t, func() error {
		return runWorkflowUpdate(newWorkflowBudgetCmd(set), []string{testWorkflowID})
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	params, _ := flow.proposals[0]["params"].(map[string]any)
	if params["include_child_prs"] != false || params["max_runs_per_issue"] != nil {
		t.Errorf("params = %v, want include_child_prs=false only", params)
	}
}

func TestWorkflowUpdateBudgetMixedFieldsAreRefusedBeforeAnyRequest(t *testing.T) {
	for _, other := range []struct{ flag, value string }{
		{"name", "x"}, {"description", "x"}, {"steps-file", "x.json"}, {"start-status", "todo"}, {"map", "a=b"}, {"dry-run", "true"},
	} {
		t.Run(other.flag, func(t *testing.T) {
			flow := budgetFlowServer(t)
			set := map[string]string{other.flag: other.value}
			for k, v := range proposeBudget {
				set[k] = v
			}
			err := runWorkflowUpdate(newWorkflowBudgetCmd(set), []string{testWorkflowID})
			if err == nil {
				t.Fatal("want a refusal")
			}
			if !strings.Contains(err.Error(), "--"+other.flag) || strings.Count(err.Error(), "multica workflow update") != 2 {
				t.Errorf("error %q must name --%s and the two commands to run instead", err, other.flag)
			}
			if n := flow.requests(); n != 0 {
				t.Errorf("requests = %d, want 0", n)
			}
		})
	}
}

func TestWorkflowUpdateBudgetNeedsAnIssueBeforeAnyRequest(t *testing.T) {
	flow := budgetFlowServer(t)
	err := runWorkflowUpdate(newWorkflowBudgetCmd(map[string]string{"max-runs-per-issue": "40"}), []string{testWorkflowID})
	if err == nil || !strings.Contains(err.Error(), "--issue") {
		t.Fatalf("err = %v, want a --issue refusal", err)
	}
	if n := flow.requests(); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

func TestWorkflowUpdateBudgetPrintHashPrintsBothDigests(t *testing.T) {
	flow := budgetFlowServer(t)
	set := map[string]string{"max-runs-per-issue": "40", "print-hash": "true"}
	out, err := captureStdout(t, func() error {
		return runWorkflowUpdate(newWorkflowBudgetCmd(set), []string{testWorkflowID})
	})
	if err != nil {
		t.Fatalf("print-hash: %v", err)
	}
	live, after := 15, 40
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", out, err)
	}
	if got["before_sha256"] != decisionaction.WorkflowBudgetDigest(testWorkflowID, &live, true) ||
		got["after_sha256"] != decisionaction.WorkflowBudgetDigest(testWorkflowID, &after, true) {
		t.Errorf("digests = %v", got)
	}
	if len(flow.proposals) != 0 || flow.patches != 0 {
		t.Errorf("proposals = %d, patches = %d, want none", len(flow.proposals), flow.patches)
	}
}
