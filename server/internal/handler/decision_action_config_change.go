package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A config change edits an instruction or setting an agent could use to widen
// its own authority, so the server decides every fact the approver signs:
//
//   - the concrete action reads the live value and computes the after value and
//     both digests itself (configChange.Read);
//   - the agent's params carry base_sha256 and after_sha256, which the server
//     only compares with its own;
//   - the preview, its digest and both digests are stored in the hash-covered
//     preconditions, and the denylist, drift and digests are checked again at
//     approval, under a try-lock per field and target plus a row lock on the target.
//
// Concrete actions register through registerConfigChange. Their Definition
// must set RequiresPreview.

// configChange is the per-action part of the shared core.
type configChange struct {
	// Lock takes the target row FOR UPDATE NOWAIT before the approval's first
	// read, so an ordinary edit cannot commit between that read and Apply. The
	// core calls it first; a busy row answers busy_retry.
	Lock func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) error
	// Read returns the live value, the value the params would write and the
	// digests of both, resolved inside the workspace. Called at request and
	// again, under the lock, at approval and after Apply.
	Read func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) (configChangeRead, error)
	// Apply writes the change in the approval transaction. It runs after the row
	// lock, and the execution it gets carries no target snapshot (Agent and
	// Workflow are zeroed): those were read before the lock and an ordinary edit
	// may have changed other columns since. Apply must reload the target under
	// the lock, or write only the columns it owns.
	Apply func(ctx context.Context, execution decisionActionExecution, read configChangeRead) error
	// Publish sends the realtime event after commit; it must be idempotent.
	Publish func(ctx context.Context, handler *Handler, action db.DecisionAction) error
	// QueueField, when set, makes approvals of this field on one target wait
	// for each other, for at most the transaction's lock timeout, instead of
	// answering busy_retry. A waiting approval holds no other lock, and it
	// reads the value the earlier one committed, so it answers
	// precondition_drift. Leave it empty to keep busy_retry.
	QueueField string
	// ServerAfter drops after_sha256 from the params: the server's own after
	// digest stands in for the agent's claim, since it computes the whole change.
	ServerAfter bool
	// StaleBaseCode, when set, turns drift at approval into a failed action with
	// this code (HTTP 200) instead of the 409 precondition_drift refusal.
	StaleBaseCode string
	// CatalogLock makes an approval take the status catalog lock before any other
	// lock, in the order a workflow save does, because the change validates against
	// the catalog.
	CatalogLock bool
	// DelegatedField, when set, requires the proposing agent to pass the
	// delegated-write gate for this field at request time.
	DelegatedField string
}

type configChangeRead struct {
	// Field is a name from decisionaction.CheckConfigChange; Keys names the
	// settings keys of a workspace.settings change.
	Field    string
	Keys     []string
	TargetID string
	// Before and After are what the approver sees; they must hold no secret.
	Before, After any
	// LiveSHA256 digests the live value, AfterSHA256 the value Apply writes.
	LiveSHA256, AfterSHA256 string
	// HumanBlocksIntact is false when the change alters a governed:human block.
	HumanBlocksIntact bool
}

var configChanges = map[string]configChange{}

func registerConfigChange(actionType string, change configChange) {
	if change.Lock == nil {
		panic("config change " + actionType + " must lock its target row")
	}
	configChanges[actionType] = change
	decisionActionExecutors[actionType] = func(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
		return executeConfigChange(ctx, change, execution)
	}
}

// configChangeLockKey names the try-lock that serialises approvals of one field
// on one target; the row lock in configChange.Lock covers ordinary edits.
func configChangeLockKey(workspaceID, field, targetID string) string {
	return workspaceID + ":config:" + field + ":" + targetID
}

// configChangeClaim is what the agent asserted in its params.
type configChangeClaim struct {
	BaseSHA256  string `json:"base_sha256"`
	AfterSHA256 string `json:"after_sha256"`
}

// configChangeWant is the field, target and live digest the change was built on.
type configChangeWant struct{ Field, TargetID, Before string }

// checkConfigChange applies the rules shared by the request and the approval,
// in this order: denylist, governed blocks, drift of the live value, and the
// agent's after digest against the server's own. An action with a stale-base
// code checks drift before the governed blocks: a block a person edited after
// the request is a changed base, not a fault of the proposal. status is the
// code for the two refusals that differ between the request (400) and the
// approval (409).
func checkConfigChange(read configChangeRead, claim configChangeClaim, want configChangeWant, status int, driftFirst bool) error {
	if err := decisionaction.CheckConfigChange(read.Field, read.Keys); err != nil {
		return decisionIntentRefusal{http.StatusForbidden, decisionaction.CodeDenylisted + ": " + err.Error()}
	}
	drifted := read.Field != want.Field || read.TargetID != want.TargetID || read.LiveSHA256 != want.Before
	if driftFirst && drifted {
		return errPreconditionDrift
	}
	if !read.HumanBlocksIntact {
		return decisionIntentRefusal{status, "governed_blocks_changed: a governed:human block may not change"}
	}
	if drifted {
		return errPreconditionDrift
	}
	if read.AfterSHA256 != claim.AfterSHA256 {
		return decisionIntentRefusal{status, "after_digest_mismatch: after_sha256 is not the digest of the change the server computes"}
	}
	return nil
}

// configChangePreconditions validates the agent's claim against the server's
// own reading and stores the preview the approver must be shown.
func configChangePreconditions(ctx context.Context, queries *db.Queries, workspaceID, agentID pgtype.UUID, draft *decisionActionDraft, change configChange) (map[string]any, error) {
	if !draft.Definition.RequiresPreview {
		return nil, errors.New("config change " + draft.Type + " must require a preview")
	}
	if change.DelegatedField != "" {
		if _, allowed := (&Handler{Queries: queries}).delegatedWriteAllowed(ctx, workspaceID, uuidToString(agentID), change.DelegatedField); !allowed {
			return nil, decisionIntentRefusal{http.StatusForbidden, "not_permitted: this agent may not propose " + change.DelegatedField + " writes"}
		}
	}
	var claim configChangeClaim
	if err := json.Unmarshal(draft.Params, &claim); err != nil {
		return nil, decisionIntentRefusal{http.StatusBadRequest, "invalid_params: params must be an object"}
	}
	read, err := change.Read(ctx, queries, workspaceID, draft.Params)
	if err != nil {
		return nil, err
	}
	if change.ServerAfter {
		claim.AfterSHA256 = read.AfterSHA256
	}
	if err := checkConfigChange(read, claim, configChangeWant{read.Field, read.TargetID, claim.BaseSHA256}, http.StatusBadRequest, change.StaleBaseCode != ""); err != nil {
		return nil, err
	}
	diff, err := configChangeDiff(read)
	if err != nil {
		return nil, err
	}
	keys := read.Keys
	if keys == nil {
		keys = []string{}
	}
	preview, err := json.Marshal(map[string]any{
		"field": read.Field, "keys": keys, "target_id": read.TargetID,
		"before": read.Before, "after": read.After, "diff": diff,
		"before_sha256": read.LiveSHA256, "after_sha256": read.AfterSHA256,
	})
	if err != nil {
		return nil, err
	}
	canonical, err := decisionaction.Canonical(preview)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	return map[string]any{
		"field": read.Field, "keys": keys, "target_id": read.TargetID,
		"before_sha256": read.LiveSHA256, "after_sha256": read.AfterSHA256,
		"preview": json.RawMessage(canonical), "preview_sha256": hex.EncodeToString(digest[:]),
	}, nil
}

// configChangeDiff renders both values as indented JSON (map keys sorted) and
// diffs the lines, so the approver reads the server's own change.
func configChangeDiff(read configChangeRead) ([]decisiontx.DiffHunk, error) {
	before, err := json.MarshalIndent(read.Before, "", "  ")
	if err != nil {
		return nil, err
	}
	after, err := json.MarshalIndent(read.After, "", "  ")
	if err != nil {
		return nil, err
	}
	hunks, err := decisiontx.Diff(string(before), string(after))
	if errors.Is(err, decisiontx.ErrDiffTooLarge) {
		return nil, decisionIntentRefusal{http.StatusBadRequest, "diff_too_large: split the change into smaller ones"}
	}
	if hunks == nil {
		hunks = []decisiontx.DiffHunk{}
	}
	return hunks, err
}

// executeConfigChange runs in the approval savepoint. It takes the try-lock and
// the target row lock, reads and checks everything again, applies, reads the result back in the same
// transaction and writes the audit row. A readback that is not the approved
// value fails the action, which undoes the write.
func executeConfigChange(ctx context.Context, change configChange, execution decisionActionExecution) (json.RawMessage, error) {
	action, queries := execution.Action, execution.Queries
	var stored struct {
		Field        string `json:"field"`
		TargetID     string `json:"target_id"`
		BeforeSHA256 string `json:"before_sha256"`
	}
	if err := json.Unmarshal(action.Preconditions, &stored); err != nil {
		return nil, err
	}
	workspaceID := uuidToString(action.WorkspaceID)
	locked, err := queries.TryLockConfigChange(ctx, configChangeLockKey(workspaceID, stored.Field, stored.TargetID))
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errBusyRetry
	}
	// The row lock comes before the first read: an ordinary edit holds the row
	// until it commits, so it cannot land between this read and Apply.
	if err := change.Lock(ctx, queries, action.WorkspaceID, action.Params); err != nil {
		return nil, err
	}
	var claim configChangeClaim
	if err := json.Unmarshal(action.Params, &claim); err != nil {
		return nil, err
	}
	read, err := change.Read(ctx, queries, action.WorkspaceID, action.Params)
	if err != nil {
		return nil, change.staleBase(err)
	}
	if change.ServerAfter {
		claim.AfterSHA256 = read.AfterSHA256
	}
	if err := checkConfigChange(read, claim, configChangeWant{stored.Field, stored.TargetID, stored.BeforeSHA256}, http.StatusConflict, change.StaleBaseCode != ""); err != nil {
		return nil, change.staleBase(err)
	}
	execution.Agent, execution.Workflow = db.Agent{}, db.IssueWorkflow{}
	if err := change.Apply(ctx, execution, read); err != nil {
		return nil, err
	}
	readback, err := change.Read(ctx, queries, action.WorkspaceID, action.Params)
	if err != nil {
		return nil, err
	}
	if readback.LiveSHA256 != read.AfterSHA256 {
		return nil, actionFailure{Code: "readback_mismatch", Message: "the stored value is not the approved value; the change was undone"}
	}
	details, err := json.Marshal(map[string]any{
		"field": read.Field, "keys": nonNilKeys(read.Keys), "target_id": read.TargetID,
		"before_sha256": read.LiveSHA256, "after_sha256": read.AfterSHA256,
		"decision_request_id": uuidToString(action.DecisionRequestID), "decision_action_id": uuidToString(action.ID),
		"agent_id": uuidToString(action.RequestedByAgentID), "task_id": uuidToString(action.RequestedByTaskID),
		"credential_id": uuidToString(execution.CredentialID),
	})
	if err != nil {
		return nil, err
	}
	if _, err := queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: action.WorkspaceID, IssueID: execution.IssueID,
		ActorType: pgtype.Text{String: "member", Valid: true}, ActorID: execution.Approver.UserID,
		Action: "config_change_applied", Details: details,
	}); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"field": read.Field, "target_id": read.TargetID, "before_sha256": read.LiveSHA256, "after_sha256": read.AfterSHA256, "readback_sha256": readback.LiveSHA256})
}

// staleBase turns drift at approval into the action's own failure code, when it has one.
func (change configChange) staleBase(err error) error {
	if change.StaleBaseCode != "" && errors.Is(err, errPreconditionDrift) {
		return actionFailure{Code: change.StaleBaseCode, Message: "the target changed since the request; ask again"}
	}
	return err
}

func nonNilKeys(keys []string) []string {
	if keys == nil {
		return []string{}
	}
	return keys
}
