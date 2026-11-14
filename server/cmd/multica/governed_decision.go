package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// CHE-2458: client side of the CHE-2448 decision-bound write. The CLI computes
// before_sha256 and after_sha256 itself, so the ask to Cheese can name the
// hash and the server compares like with like.
//
// Instruction fields hash the raw UTF-8 bytes (governed_cas.go). Workflow
// steps hash the canonical JSON recipe documented on DecisionBindingFields in
// server/internal/handler/issue_workflow_bound_write.go; workflowStepsHash
// below repeats it, because this package must not import internal/handler.

func printHashRequested(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("print-hash")
	return v
}

// checkDecisionID rejects a --decision-id that is not a decision request UUID.
func checkDecisionID(cmd *cobra.Command) error {
	if !voteIssueUUID.MatchString(mustGetString(cmd, "decision-id")) {
		return fmt.Errorf("--decision-id must be a decision request UUID")
	}
	return nil
}

// printHashOnly prints the hashes of a digest-mode body and reports true when
// --print-hash asked for that instead of a write.
func printHashOnly(cmd *cobra.Command, body map[string]any, field string) (bool, error) {
	if !printHashRequested(cmd) {
		return false, nil
	}
	return true, printHashes(bodyBefore(body), hexSHA256([]byte(bodyText(body, field))))
}

func printHashes(before, after string) error {
	return cli.PrintJSON(os.Stdout, map[string]string{"before_sha256": before, "after_sha256": after})
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// workflowStepsHash is the server's recipe: {"steps": [...], "workflow_id": id}
// as canonical JSON (sorted keys, no whitespace, no HTML escaping), sha256,
// lowercase hex. steps is the "steps" array of "workflow get --output json"
// or of a --steps-file; fields the server does not store are dropped.
func workflowStepsHash(workflowID string, steps []any) (string, error) {
	typed, err := decodeSteps(steps)
	if err != nil {
		return "", err
	}
	return hashSteps(workflowID, typed)
}

// workflowStoredStepsHash hashes steps as the server stores them: trimmed,
// defaulted and arranged by issueworkflow.Normalize, the same call the PATCH
// handler makes before it checks after_sha256.
func workflowStoredStepsHash(workflowID string, steps []any) (string, error) {
	typed, err := decodeSteps(steps)
	if err != nil {
		return "", err
	}
	_, _, stored := issueworkflow.Normalize("", "", typed)
	return hashSteps(workflowID, stored)
}

func decodeSteps(steps []any) ([]issueworkflow.Step, error) {
	raw, err := json.Marshal(steps)
	if err != nil {
		return nil, err
	}
	var typed []issueworkflow.Step
	if err := json.Unmarshal(raw, &typed); err != nil {
		return nil, fmt.Errorf("steps: %w", err)
	}
	return typed, nil
}

func hashSteps(workflowID string, steps []issueworkflow.Step) (string, error) {
	raw, err := json.Marshal(map[string]any{"steps": steps, "workflow_id": workflowID})
	if err != nil {
		return "", err
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return "", err
	}
	var canonical bytes.Buffer
	enc := json.NewEncoder(&canonical) // a map encodes with its keys sorted
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return "", err
	}
	return hexSHA256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n"))), nil
}

// workflowBinding is the decision-bound part of a workflow write. A nil
// binding means the plain owner/admin path.
type workflowBinding struct {
	decisionID string
	before     string
	after      string
}

// workflowBindingRequested reports whether the command asks for a decision-bound
// write or a hash dry run.
func workflowBindingRequested(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("decision-id") || printHashRequested(cmd)
}

// checkWorkflowBindingFlags rejects a malformed decision-bound workflow
// request before any HTTP call. The server accepts steps (with
// expected_version) or the run budget only, so rename and start-status flags
// are refused here instead of as a 400.
func checkWorkflowBindingFlags(cmd *cobra.Command) error {
	if !workflowBindingRequested(cmd) {
		return nil
	}
	if cmd.Flags().Changed("decision-id") {
		if err := checkDecisionID(cmd); err != nil {
			return err
		}
	}
	for _, other := range []string{"name", "description", "map", "dry-run", "copy-for-project", "start-status"} {
		if cmd.Flags().Changed(other) {
			return fmt.Errorf("a decision-bound write edits steps or the run budget only; --%s needs the owner/admin path", other)
		}
	}
	return nil
}

// apply adds the binding to a workflow PATCH body, or prints the hashes and
// reports true for --print-hash.
func (b workflowBinding) apply(cmd *cobra.Command, body map[string]any) (bool, error) {
	if printHashRequested(cmd) {
		return true, printHashes(b.before, b.after)
	}
	body["decision_id"], body["before_sha256"], body["after_sha256"] = b.decisionID, b.before, b.after
	return false, nil
}

// bindBudgetWrite binds a budget-only workflow write (CHE-5228) to the owner's
// workflow.budget decision, or prints its hashes for --print-hash. The digests
// come from workflowBudgetProposal, the same read and recipe as
// "config propose workflow.budget", so the card and the write agree.
func bindBudgetWrite(cmd *cobra.Command, ctx context.Context, client *cli.APIClient, workflowID string, body map[string]any) error {
	var maxRuns *int
	var includeChildPRs *bool
	if v, ok := body["max_runs_per_issue"].(int); ok {
		maxRuns = &v
	}
	if v, ok := body["include_child_prs"].(bool); ok {
		includeChildPRs = &v
	}
	p, err := workflowBudgetProposal(ctx, client, workflowID, maxRuns, includeChildPRs)
	if err != nil {
		return err
	}
	_, err = workflowBinding{mustGetString(cmd, "decision-id"), p.BaseSHA256, p.AfterSHA256}.apply(cmd, body)
	return err
}

// proposesWorkflowBudget is true for a task token that names a budget flag
// without a decision to bind: the call opens the card (CHE-5504).
func proposesWorkflowBudget(cmd *cobra.Command) bool {
	return strings.HasPrefix(resolveToken(cmd), cli.TaskTokenPrefix) && !workflowBindingRequested(cmd) &&
		(cmd.Flags().Changed("max-runs-per-issue") || cmd.Flags().Changed("include-child-prs"))
}

// proposeWorkflowBudget opens one workflow.budget.set card and writes nothing.
// workflowBudgetProposal is the read and recipe "config propose workflow.budget"
// uses; an equal call replays the open card on the server.
func proposeWorkflowBudget(cmd *cobra.Command, workflow string) error {
	for _, flag := range []string{"name", "description", "steps-file", "start-status", "map", "dry-run"} {
		if cmd.Flags().Changed(flag) {
			return fmt.Errorf("--%s cannot go with the budget flags: a budget change is approved on its own card. Run two commands: "+
				"`multica workflow update <workflow> --%s ...` and `multica workflow update <workflow> --max-runs-per-issue N --include-child-prs=BOOL --issue <issue>`", flag, flag)
		}
	}
	issueArg, _ := cmd.Flags().GetString("issue")
	if strings.TrimSpace(issueArg) == "" {
		return errors.New("--issue is required: the approval card sits on that issue")
	}
	var maxRuns *int
	var includeChildPRs *bool
	if cmd.Flags().Changed("max-runs-per-issue") {
		n, _ := cmd.Flags().GetInt("max-runs-per-issue")
		maxRuns = &n
	}
	if cmd.Flags().Changed("include-child-prs") {
		b, _ := cmd.Flags().GetBool("include-child-prs")
		includeChildPRs = &b
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	id, err := workflowRefID(ctx, client, workflow)
	if err != nil {
		return fmt.Errorf("resolve workflow: %w", err)
	}
	issue, err := resolveIssueRef(ctx, client, issueArg)
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	p, err := workflowBudgetProposal(ctx, client, id, maxRuns, includeChildPRs)
	if err != nil {
		return err
	}
	params := make(map[string]json.RawMessage, len(p.Params))
	for key, value := range p.Params {
		params[key], _ = json.Marshal(value)
	}
	forUser, _ := cmd.Flags().GetString("for")
	raw, err := postActionProposal(ctx, client, p.Type, 1, issue.ID, forUser, "", params)
	if err != nil {
		return err
	}
	// The URL is the point of this call; keep the update default of json only
	// when the caller asks for it.
	if !cmd.Flags().Changed("output") {
		_ = cmd.Flags().Set("output", "table")
	}
	return printActionProposal(ctx, cmd, client, p.Type, issue, raw)
}

// CHE-5228: an agent widens an issue's run budget (a workflow override on an
// update, a wider workflow on a create) with an owner-approved issue.workflow
// decision. The digests bind the issue, its project and the workflow it
// follows; decisionaction.IssueWorkflowDigest is the server's recipe.

// checkIssueWorkflowBindingFlags rejects --decision-id and --print-hash without
// --workflow, or a malformed decision id, before any HTTP call.
func checkIssueWorkflowBindingFlags(cmd *cobra.Command) error {
	if !workflowBindingRequested(cmd) {
		return nil
	}
	if cmd.Flags().Changed("decision-id") {
		if err := checkDecisionID(cmd); err != nil {
			return err
		}
	}
	if v, _ := cmd.Flags().GetString("workflow"); v == "" {
		return fmt.Errorf("--decision-id and --print-hash bind a --workflow change; pass --workflow <workflow-id>")
	}
	return nil
}

// liveIssueWorkflow reads an issue's project and its workflow override ("" when
// it has none).
func liveIssueWorkflow(ctx context.Context, client *cli.APIClient, issueID string) (projectID, override string, err error) {
	var issue map[string]any
	if err := client.GetJSON(ctx, "/api/issues/"+url.PathEscape(issueID), &issue); err != nil {
		return "", "", fmt.Errorf("read issue: %w", err)
	}
	return strVal(issue, "project_id"), strVal(issue, "workflow_id"), nil
}

// bindIssueWorkflow adds the decision binding for moving an issue ("" while it
// is being created) from its live workflow override to body["workflow_id"], or
// prints the hashes and reports true for --print-hash.
func bindIssueWorkflow(cmd *cobra.Command, body map[string]any, issueID, projectID, liveOverride string) (bool, error) {
	target, _ := body["workflow_id"].(string)
	return workflowBinding{
		decisionID: mustGetString(cmd, "decision-id"),
		before:     decisionaction.IssueWorkflowDigest(issueID, projectID, liveOverride),
		after:      decisionaction.IssueWorkflowDigest(issueID, projectID, target),
	}.apply(cmd, body)
}
