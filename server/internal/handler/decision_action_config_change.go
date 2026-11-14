package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

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
	// Fields are the only governed fields Read may report for this action. A
	// read that names another field is refused, so a settings patch cannot be
	// relabelled as a dedicated field. A workflow write reports either of two.
	Fields []string
	// Authorize is the per-action proposer policy. The core calls it at request
	// time with the request's asking agent and again at approval, after the
	// locks and the re-Read, with the action's stored requesting agent; the
	// proposer never comes from params or the agent's note. An action with a
	// hook and no valid proposer fails closed.
	Authorize func(ctx context.Context, queries *db.Queries, workspaceID, proposer pgtype.UUID, params json.RawMessage, read configChangeRead) error
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
	// AfterMismatchIsDrift makes an after-digest mismatch at approval answer
	// precondition_drift: the claim matched at request time, so the server's
	// own after value moving since is a changed target, not a wrong claim.
	// The request still refuses a mismatch as after_digest_mismatch.
	AfterMismatchIsDrift bool
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
	// BeforeText and AfterText, when both set, are diffed as raw text, one line
	// per line of the text. BeforeLines and AfterLines, when either is set, are
	// diffed as a list with one item per line. Otherwise Before and After are
	// diffed as indented JSON.
	BeforeText, AfterText   *string
	BeforeLines, AfterLines []string
	// Meta is stored in the preview, inside its digest (for example the unit
	// and counts of a text). Nil is omitted.
	Meta map[string]any
}

// textConfigRead is the read of a plain text field: raw text diff, digests of
// the UTF-8 bytes, and counts in Unicode code points. The card shows these
// counts and never recounts. Callers set HumanBlocksIntact and may add to Meta
// (for example a cap).
func textConfigRead(field, targetID, before, after string) configChangeRead {
	return configChangeRead{
		Field: field, TargetID: targetID, Before: before, After: after,
		BeforeText: &before, AfterText: &after,
		LiveSHA256: decisionaction.TextDigest(before), AfterSHA256: decisionaction.TextDigest(after),
		Meta: map[string]any{
			"unit": "code_points", "chars_before": utf8.RuneCountInString(before), "chars_after": utf8.RuneCountInString(after),
		},
	}
}

var configChanges = map[string]configChange{}

func registerConfigChange(actionType string, change configChange) {
	if change.Lock == nil {
		panic("config change " + actionType + " must lock its target row")
	}
	if len(change.Fields) == 0 {
		panic("config change " + actionType + " must name the fields it writes")
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
const afterDigestMismatchCode = "after_digest_mismatch"

type configChangeWant struct{ Field, TargetID, Before string }

// checkConfigChange applies the rules shared by the request and the approval,
// in this order: denylist, the action's bound fields, governed blocks, drift
// of the live value, and the agent's after digest against the server's own. An action with a
// stale-base code checks drift before the governed blocks: a block a person
// edited after the request is a changed base, not a fault of the proposal.
// status is the code for the two refusals that differ between the request
// (400) and the approval (409).
func checkConfigChange(fields []string, read configChangeRead, claim configChangeClaim, want configChangeWant, status int, driftFirst bool) error {
	if err := decisionaction.CheckConfigChange(read.Field, read.Keys); err != nil {
		return decisionIntentRefusal{http.StatusForbidden, decisionaction.CodeDenylisted + ": " + err.Error()}
	}
	if !slices.Contains(fields, read.Field) {
		return decisionIntentRefusal{http.StatusForbidden, decisionaction.CodeDenylisted + ": this action may not write " + read.Field}
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
		return decisionIntentRefusal{status, afterDigestMismatchCode + ": after_sha256 is not the digest of the change the server computes"}
	}
	return nil
}

// configChangePreconditions validates the agent's claim against the server's
// own reading and stores the preview the approver must be shown.
func configChangePreconditions(ctx context.Context, queries *db.Queries, workspaceID, proposer pgtype.UUID, draft *decisionActionDraft, change configChange) (map[string]any, error) {
	if !draft.Definition.RequiresPreview {
		return nil, errors.New("config change " + draft.Type + " must require a preview")
	}
	if change.DelegatedField != "" {
		if _, allowed := (&Handler{Queries: queries}).delegatedWriteAllowed(ctx, workspaceID, uuidToString(proposer), change.DelegatedField); !allowed {
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
	if err := checkConfigChange(change.Fields, read, claim, configChangeWant{read.Field, read.TargetID, claim.BaseSHA256}, http.StatusBadRequest, change.StaleBaseCode != ""); err != nil {
		return nil, err
	}
	if err := authorizeConfigChange(ctx, change, queries, workspaceID, proposer, draft.Params, read); err != nil {
		return nil, err
	}
	kind, diff, err := configChangeDiff(read)
	if err != nil {
		return nil, err
	}
	meta := previewMeta(read, kind, diff)
	keys := read.Keys
	if keys == nil {
		keys = []string{}
	}
	fields := map[string]any{
		"field": read.Field, "keys": keys, "target_id": read.TargetID,
		"before": read.Before, "after": read.After, "diff": diff, "diff_kind": kind,
		"before_sha256": read.LiveSHA256, "after_sha256": read.AfterSHA256,
	}
	if meta != nil {
		fields["meta"] = meta
	}
	preview, err := json.Marshal(fields)
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

type configApprovalKey struct{}

// configChangeAtApproval tells an Authorize hook which phase it runs in: at
// approval a proposer who lost a grant was allowed once and is now revoked,
// which is a different refusal from never having been allowed.
func configChangeAtApproval(ctx context.Context) bool {
	at, _ := ctx.Value(configApprovalKey{}).(bool)
	return at
}

// authorizeConfigChange runs the action's proposer policy, if it has one. The
// proposer is the stored asking agent, never a value from params. An action
// with a policy and no valid proposer fails closed.
func authorizeConfigChange(ctx context.Context, change configChange, queries *db.Queries, workspaceID, proposer pgtype.UUID, params json.RawMessage, read configChangeRead) error {
	if change.Authorize == nil {
		return nil
	}
	if !proposer.Valid {
		return decisionIntentRefusal{http.StatusForbidden, "proposer_required: this change needs an identified proposing agent"}
	}
	return change.Authorize(ctx, queries, workspaceID, proposer, params, read)
}

// Preview diff kinds, stored in the preview so a card knows how to read it.
const (
	diffKindJSON  = "json"
	diffKindText  = "text"
	diffKindLines = "lines"
)

// configChangeDiff diffs what the approver reads: the raw text, the line list,
// or else both values as indented JSON (map keys sorted). It returns the kind.
func configChangeDiff(read configChangeRead) (string, []decisiontx.DiffHunk, error) {
	kind, before, after := diffKindJSON, "", ""
	switch {
	case read.BeforeText != nil && read.AfterText != nil:
		kind, before, after = diffKindText, *read.BeforeText, *read.AfterText
	case read.BeforeLines != nil || read.AfterLines != nil:
		kind, before, after = diffKindLines, strings.Join(read.BeforeLines, "\n"), strings.Join(read.AfterLines, "\n")
	default:
		beforeJSON, err := json.MarshalIndent(read.Before, "", "  ")
		if err != nil {
			return "", nil, err
		}
		afterJSON, err := json.MarshalIndent(read.After, "", "  ")
		if err != nil {
			return "", nil, err
		}
		before, after = string(beforeJSON), string(afterJSON)
	}
	hunks, err := decisiontx.Diff(before, after)
	if errors.Is(err, decisiontx.ErrDiffTooLarge) {
		return "", nil, decisionIntentRefusal{http.StatusBadRequest, "diff_too_large: split the change into smaller ones"}
	}
	if hunks == nil {
		hunks = []decisiontx.DiffHunk{}
	}
	return kind, hunks, err
}

// previewMeta is the read's Meta plus two flags for what the line diff cannot
// show, because it splits lines and drops line-ending and final-newline
// differences: line_endings_changed when the two texts differ there (hunks or
// not), and whitespace_only when that is the only change, so the digests
// differ but no changed line exists.
func previewMeta(read configChangeRead, kind string, hunks []decisiontx.DiffHunk) map[string]any {
	meta := maps.Clone(read.Meta)
	if kind != diffKindText || *read.BeforeText == *read.AfterText {
		return meta
	}
	before, after := *read.BeforeText, *read.AfterText
	endingsChanged := strings.HasSuffix(before, "\n") != strings.HasSuffix(after, "\n") || lineEndingStyleChanged(before, after)
	if !endingsChanged && len(hunks) > 0 {
		return meta
	}
	if meta == nil {
		meta = map[string]any{}
	}
	if endingsChanged {
		meta["line_endings_changed"] = true
	}
	if len(hunks) == 0 {
		meta["whitespace_only"] = true
	}
	return meta
}

// lineEndingStyleChanged compares the style (CRLF, bare LF or both), never the
// count: adding or removing a line in a CRLF file changes no line ending. A text
// with no line ending has no style, so it never counts as a change.
func lineEndingStyleChanged(before, after string) bool {
	style := func(text string) (crlf, lf bool) {
		return strings.Contains(text, "\r\n"), strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n")
	}
	beforeCRLF, beforeLF := style(before)
	afterCRLF, afterLF := style(after)
	if !beforeCRLF && !beforeLF || !afterCRLF && !afterLF {
		return false
	}
	return beforeCRLF != afterCRLF || beforeLF != afterLF
}

// executeConfigChange runs in the approval savepoint. It takes the try-lock and
// the target row lock, reads and checks everything again, applies, reads the result back in the same
// transaction and writes the audit row. A readback that is not the approved
// value fails the action, which undoes the write.
func executeConfigChange(ctx context.Context, change configChange, execution decisionActionExecution) (json.RawMessage, error) {
	ctx = context.WithValue(ctx, configApprovalKey{}, true)
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
		return nil, atStep("config_change", errBusyRetry)
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
	if err := checkConfigChange(change.Fields, read, claim, configChangeWant{stored.Field, stored.TargetID, stored.BeforeSHA256}, http.StatusConflict, change.StaleBaseCode != ""); err != nil {
		var refusal decisionIntentRefusal
		if change.AfterMismatchIsDrift && errors.As(err, &refusal) && strings.HasPrefix(refusal.message, afterDigestMismatchCode+":") {
			err = errPreconditionDrift
		}
		return nil, change.staleBase(err)
	}
	if err := authorizeConfigChange(ctx, change, queries, action.WorkspaceID, action.RequestedByAgentID, action.Params, read); err != nil {
		return nil, err
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
