package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Decision-bound agent writes on the workflow routes (CHE-2456). An agent task
// token may PATCH a workflow's steps or PUT a handoff only when the body names
// a decision (decision_id) and the hashes of the workflow before and after the
// write; verifyDecisionBinding then proves a workspace owner approved exactly
// that change. A change to step instructions only may carry a PASS receipt
// (receipt_issue) instead, see verifyWorkflowReceipt. A task token with
// neither keeps the 403, and so does every other machine credential.
//
// Hash recipe, which an agent follows byte for byte:
//
//  1. Take the workflow's steps as GET /api/issue-workflows/{id} returns them
//     (the "steps" array).
//  2. Wrap them with the workflow's id: {"steps": <that array>,
//     "workflow_id": "<the {id} in the route>"}. The id binds an approval to
//     one workflow, so identical steps on another workflow hash differently.
//  3. Write that value as canonical JSON: UTF-8, object keys sorted bytewise,
//     no whitespace, no HTML escaping (< > & stay literal), numbers as written.
//     Go writes U+2028 and U+2029 as   and  .
//  4. sha256 the bytes; the hash is lowercase hex.
//
// before_sha256 is that hash of the live workflow. after_sha256 is that hash of
// the workflow as the server will store it, so an agent whose steps the server
// normalizes differently is refused, not written. Only steps are bound: a
// bound write carries no name, description, run rule, status mapping, new
// status or dry run. A bound write also leaves every governed:human block of
// every step byte-identical (see protectedStepBlocksUnchanged).
type DecisionBindingFields struct {
	DecisionID   string `json:"decision_id,omitempty"`
	BeforeSHA256 string `json:"before_sha256,omitempty"`
	AfterSHA256  string `json:"after_sha256,omitempty"`
	// ReceiptIssue names the issue whose comments carry a PASS receipt for a
	// workflow.instructions change (CHE-3580). It replaces decision_id.
	ReceiptIssue string `json:"receipt_issue,omitempty"`
}

// Fields a bound write may delegate. A write that changes only step
// instructions is workflow.instructions; anything else is workflow.steps.
const (
	workflowStepsField        = "workflow.steps"
	workflowInstructionsField = "workflow.instructions"
)

// workflowStepsHash is the recipe above for steps of the workflow with id.
func workflowStepsHash(id pgtype.UUID, steps []issueworkflow.Step) string {
	payload := make([]IssueWorkflowStepPayload, len(steps))
	for i, s := range steps {
		payload[i] = stepToPayload(s)
	}
	raw, _ := json.Marshal(map[string]any{"steps": payload, "workflow_id": uuidToString(id)}) // a plain struct tree always marshals
	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&generic)
	var canonical bytes.Buffer
	enc := json.NewEncoder(&canonical) // a map encodes with its keys sorted
	enc.SetEscapeHTML(false)
	_ = enc.Encode(generic)
	sum := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

// workflowWriteField names the governed field a change from before to after
// writes: workflow.instructions when only step instructions differ.
func workflowWriteField(before, after []issueworkflow.Step) string {
	withoutInstructions := func(steps []issueworkflow.Step) string {
		stripped := make([]issueworkflow.Step, len(steps))
		for i, s := range steps {
			s.Instructions = ""
			stripped[i] = s
		}
		return workflowStepsHash(pgtype.UUID{}, stripped)
	}
	if withoutInstructions(before) == withoutInstructions(after) {
		return workflowInstructionsField
	}
	return workflowStepsField
}

// refuseUnboundMachineWrite answers 403 for a machine credential that is not
// an agent task token carrying a decision id or receipt issue, and reports whether it did. A
// human caller passes.
func refuseUnboundMachineWrite(w http.ResponseWriter, r *http.Request, binding DecisionBindingFields) bool {
	if !isMachineCredentialActor(r) {
		return false
	}
	if r.Header.Get("X-Actor-Source") != "task_token" || (strings.TrimSpace(binding.DecisionID) == "" && strings.TrimSpace(binding.ReceiptIssue) == "") {
		writeError(w, http.StatusForbidden, "agents may change a workflow only with a decision-bound write (decision_id, before_sha256, after_sha256) or, for step instructions, a PASS receipt (receipt_issue, before_sha256, after_sha256)")
		return true
	}
	return false
}

// verifyWorkflowWrite proves an agent's workflow write: the live workflow still
// hashes to before_sha256 (409 when it moved), the stored result hashes to
// after_sha256 (403 when the agent wrote something else), and a workspace owner
// approved exactly that change for this agent. It records the single-use row in
// qtx's transaction (tx), so the caller must hold the catalog lock and commit.
func (h *Handler) verifyWorkflowWrite(ctx context.Context, tx db.DBTX, qtx *db.Queries, r *http.Request, workspaceID, workflowID pgtype.UUID, binding DecisionBindingFields, before, after []issueworkflow.Step) error {
	if strings.TrimSpace(binding.ReceiptIssue) != "" {
		return h.verifyWorkflowReceipt(ctx, tx, r, workspaceID, workflowID, binding, before, after)
	}
	decisionID, err := util.ParseUUID(binding.DecisionID)
	if err != nil {
		return decisionIntentRefusal{http.StatusBadRequest, "decision_id must be a decision request id"}
	}
	agentID, err := util.ParseUUID(r.Header.Get("X-Agent-ID"))
	if err != nil {
		return decisionIntentRefusal{http.StatusForbidden, "agent identity is required"}
	}
	if !protectedStepBlocksUnchanged(before, after) {
		return decisionIntentRefusal{http.StatusForbidden, "a governed:human block would change"}
	}
	if binding.BeforeSHA256 != workflowStepsHash(workflowID, before) {
		return decisionIntentRefusal{http.StatusConflict, "before_sha256 does not match the live workflow; read it again and ask for a new decision"}
	}
	if binding.AfterSHA256 != workflowStepsHash(workflowID, after) {
		return decisionIntentRefusal{http.StatusForbidden, "after_sha256 does not match the steps this write would store"}
	}
	gate := func(ctx context.Context, _ *db.Queries, ws, agent pgtype.UUID, field string) (bool, bool) {
		return h.delegatedWriteAllowed(ctx, ws, uuidToString(agent), field)
	}
	return verifyDecisionBinding(ctx, qtx, gate, workspaceID, decisionID, agentID, workflowWriteField(before, after), binding.BeforeSHA256, binding.AfterSHA256)
}

// verifyWorkflowReceipt proves a workflow.instructions write by a PASS receipt
// from an agent of another lab (CHE-3250 design, checks 1 to 10) and records
// the single-use row in tx. A write that changes anything but step
// instructions is workflow.steps, which a receipt never covers.
func (h *Handler) verifyWorkflowReceipt(ctx context.Context, tx db.DBTX, r *http.Request, workspaceID, workflowID pgtype.UUID, binding DecisionBindingFields, before, after []issueworkflow.Step) error {
	deny := func(reason string) error {
		return decisionIntentRefusal{http.StatusForbidden, "receipt rejected: " + reason}
	}
	if strings.TrimSpace(binding.DecisionID) != "" {
		return decisionIntentRefusal{http.StatusBadRequest, receiptIssueKey + " and decision_id are alternatives; send one"}
	}
	issueID, err := util.ParseUUID(binding.ReceiptIssue)
	if err != nil || !isWellFormedDigestHex(binding.BeforeSHA256) || !isWellFormedDigestHex(binding.AfterSHA256) {
		return decisionIntentRefusal{http.StatusBadRequest, receiptIssueKey + " must be an issue id and " + voteBeforeKey + "/" + voteAfterKey + " 64-character hex sha256 digests"}
	}
	writerID := r.Header.Get("X-Agent-ID")
	if _, err := util.ParseUUID(writerID); err != nil {
		return decisionIntentRefusal{http.StatusForbidden, "agent identity is required"}
	}
	if field := workflowWriteField(before, after); field != workflowInstructionsField {
		return deny("a receipt covers " + workflowInstructionsField + " only, not " + field)
	}
	if !protectedStepBlocksUnchanged(before, after) {
		return decisionIntentRefusal{http.StatusForbidden, "a governed:human block would change"}
	}
	if binding.BeforeSHA256 != workflowStepsHash(workflowID, before) {
		return decisionIntentRefusal{http.StatusConflict, "before_sha256 does not match the live workflow; read it again and ask for a new receipt"}
	}
	if binding.AfterSHA256 != workflowStepsHash(workflowID, after) {
		return decisionIntentRefusal{http.StatusForbidden, "after_sha256 does not match the steps this write would store"}
	}
	if _, allowed := h.delegatedWriteAllowed(ctx, workspaceID, writerID, workflowInstructionsField); !allowed {
		return deny("the writer is not delegated to write " + workflowInstructionsField)
	}
	_, reason, err := claimPassReceipt(ctx, tx, workspaceID, issueID, workflowID, writerID, r.Header.Get("X-Task-ID"), workflowInstructionsField, binding.BeforeSHA256, binding.AfterSHA256)
	switch {
	case err != nil:
		return err
	case reason == receiptUsed:
		return decisionIntentRefusal{http.StatusConflict, receiptUsed}
	case reason != "":
		return deny(reason)
	}
	return nil
}

// protectedStepBlocksUnchanged applies CHE-2448's rule to a workflow write:
// every step's governed:human blocks stay byte-identical. A step that holds a
// block cannot be removed, and a step cannot gain one. Instructions the write
// leaves as they are pass without a look, so a step that already carries a
// malformed marker is not blocked from unrelated edits.
func protectedStepBlocksUnchanged(before, after []issueworkflow.Step) bool {
	live := make(map[string]string, len(before))
	for _, s := range before {
		live[s.StatusKey] = s.Instructions
	}
	kept := make(map[string]bool, len(after))
	for _, s := range after {
		kept[s.StatusKey] = true
		if was := live[s.StatusKey]; was != s.Instructions && !protectedBlocksUnchanged(was, s.Instructions) {
			return false
		}
	}
	for key, instructions := range live {
		if blocks, ok := governedHumanBlocks(instructions); !kept[key] && (!ok || len(blocks) > 0) {
			return false
		}
	}
	return true
}
