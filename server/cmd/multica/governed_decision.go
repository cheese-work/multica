package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
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
// request before any HTTP call. The server accepts steps and expected_version
// only, so rule flags are refused here instead of as a 400.
func checkWorkflowBindingFlags(cmd *cobra.Command) error {
	if !workflowBindingRequested(cmd) {
		return nil
	}
	if cmd.Flags().Changed("decision-id") {
		if err := checkDecisionID(cmd); err != nil {
			return err
		}
	}
	for _, rule := range []string{"max-runs-per-issue", "include-child-prs"} {
		if cmd.Flags().Changed(rule) {
			return fmt.Errorf("a decision-bound write carries --steps-file only; --%s changes a run rule, which needs the owner/admin path", rule)
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
