package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
)

// This file implements a single, narrowly-scoped, human-approved exception to
// CHE-455's default-deny rule (rejectGovernedFieldForAgentActor in
// governed_instruction_fields.go): a machine actor may never write a governed
// instruction field.
//
// Provenance — read this before touching anything below:
//
//   - Issue: CHE-764.
//   - Human approval: Cheese (member f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff)
//     answered "yes" on 2026-09-24 to the precisely-scoped question asked in
//     CHE-763 comment 01a0d26c-1945-758e-b568-ddf630be0628. The question that
//     was approved is exactly the predicate hermesExceptionIdentityMatches +
//     hermesExceptionMatchesWorkspaceContext / hermesExceptionMatchesSquadInstructions
//     implement together below — nothing wider.
//   - This is NOT an agent self-authorizing. The approval is a specific
//     human's specific "yes" to a specific written question, recorded on a
//     specific issue comment, cited here so any future reader can go verify
//     the provenance themselves instead of taking this file's word for it.
//
// Why this is safe despite being an exception to a security control:
//
//  1. Identity is scoped to exactly one agent. hermesExceptionAgentID names
//     the ONE agent (c00-hermes-devops) this exception ever applies to, and
//     that identity is established the same way every other CHE-455 check
//     establishes actor identity: via h.resolveActor's return value, which is
//     itself derived only from server-stamped headers (X-Actor-Source,
//     X-Agent-ID) that the auth middleware strips from client input before
//     re-stamping — see resolveActor's doc comment in handler.go and
//     actor_guards.go's RequireHumanActor comment. This file never reads a
//     header or body field to decide "who is calling"; it only compares the
//     actorID resolveActor already computed against a constant.
//  2. Target is scoped to exactly two fields on two specific rows —
//     workspace.context on one workspace ID, squad.instructions on one squad
//     ID. hermesExceptionMatchesWorkspaceContext and
//     hermesExceptionMatchesSquadInstructions hard-code both the row ID and
//     the field; there is no parameterization that could widen this to
//     another workspace, squad, or field.
//  3. The write is a compare-and-swap on exact bytes, not a free-text
//     accept. The caller must present a sha256 digest of the value it
//     believes is currently live (expected-before digest); the server
//     re-reads the live value and its own freshly computed digest inside the
//     same transaction that performs the write, and refuses to write at all
//     if they disagree (see hermesExceptionVerifyAndSwap callers in
//     workspace.go / squad.go). This is what "reject unreviewed bytes"
//     reduces to in a codebase with no local reviewed-candidate registry:
//     the only bytes this path will ever accept are bytes whose digest
//     matches a value the caller already knows was live a moment ago, and
//     the after-digest is always returned so the caller (or a human
//     reviewing the audit log) can confirm byte-for-byte what actually
//     landed.
//  4. Every successful and rejected attempt is audit-logged with actor,
//     target, before-digest and after-digest, so this exception's entire
//     usage history is reconstructable from logs alone.
//
// What this file deliberately does NOT do: it does not add a new credential
// type, does not accept human/member credentials, does not touch any other
// field/workspace/squad/agent, and does not itself apply any real content —
// CHE-764 is infrastructure only.

// hermesExceptionAgentID is the one agent identity this exception ever
// applies to: c00-hermes-devops. Named as a package-level constant (not
// inlined) so (a) a future audit can grep one symbol to find every place
// this identity is special-cased, and (b) tests can seed a fixture agent row
// with this exact literal ID and exercise the real code path end-to-end
// instead of only unit-testing the predicate in isolation.
const hermesExceptionAgentID = "b2b52f93-32e6-4caf-80ad-1b48cf60b821"

// hermesExceptionWorkspaceID is the one workspace this exception applies to,
// and only for its `context` field.
const hermesExceptionWorkspaceID = "f2b734e0-b6de-4414-8a27-a2f69b0ef843"

// hermesExceptionSquadID is the one squad (the "dev team" squad) this
// exception applies to, and only for its `instructions` field.
const hermesExceptionSquadID = "a1073a68-4965-44ff-a8fb-244341935110"

// digestHexLen is the length of a lowercase-hex-encoded sha256 digest
// (32 bytes -> 64 hex characters). Used to fail closed on a malformed
// expected-before digest before it ever reaches a comparison or a query.
const digestHexLen = sha256.Size * 2

// hermesExceptionDigest matches the digest convention already established by
// daemon_workspace.go's daemonWorkspacesETag: fmt.Sprintf("%x",
// sha256.Sum256(data)) — the same convention issue_snapshot.go's sha256Hex
// helper uses too, just over a []byte instead of a string, since this file's
// callers already hold their values as []byte ([]byte(*currentContext) /
// []byte(req.NewContent)). Named distinctly from that existing sha256Hex to
// avoid a package-level redeclaration; both compute the identical digest.
func hermesExceptionDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

// isWellFormedDigestHex reports whether s could plausibly be a sha256 hex
// digest: exactly 64 lowercase-or-uppercase hex characters. This is a format
// check only — it does not claim the digest matches anything — used to
// reject a malformed expected-before digest with 400 before it is ever
// compared against a live value (spec requirement: malformed digest -> 400,
// distinct from a well-formed-but-stale digest -> 409).
func isWellFormedDigestHex(s string) bool {
	if len(s) != digestHexLen {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// hermesExceptionIdentityMatches reports whether the actor resolveActor
// identified for this request is exactly hermesExceptionAgentID.
//
// actorType and actorID MUST come from the same h.resolveActor(...) call the
// caller already uses for the CHE-455 rejectGovernedFieldForAgentActor check
// — this function does not re-derive identity from the request, it only
// narrows an already-server-validated agent identity down to the one
// permitted agent. actorType must be exactly "agent": resolveActor returns
// "member" for every non-agent caller (including a cloud-node PAT — see its
// doc comment), so a "member" actorType can never match here regardless of
// actorID, which keeps this exception from ever being reachable by a human
// credential or an mcn_ cloud-node PAT.
func hermesExceptionIdentityMatches(actorType, actorID string) bool {
	return actorType == "agent" && actorID == hermesExceptionAgentID
}

// hermesExceptionMatchesWorkspaceContext reports whether this request is
// exactly "write workspace.context on hermesExceptionWorkspaceID" — the only
// workspace-side write this exception ever covers. workspaceID must be the
// path-derived workspace ID the handler already resolved (never a body
// field), and fieldWillBeWritten must be computed the same way CHE-455
// computes it (the decoded struct field, e.g. req.Context != nil) so this
// can never disagree with what the write path actually branches on.
func hermesExceptionMatchesWorkspaceContext(workspaceID string, fieldWillBeWritten bool) bool {
	return fieldWillBeWritten && workspaceID == hermesExceptionWorkspaceID
}

// hermesExceptionMatchesSquadInstructions is the squad-side analogue of
// hermesExceptionMatchesWorkspaceContext: "write squad.instructions on
// hermesExceptionSquadID", and nothing else.
func hermesExceptionMatchesSquadInstructions(squadID string, fieldWillBeWritten bool) bool {
	return fieldWillBeWritten && squadID == hermesExceptionSquadID
}

// hermesExceptionRequest is the subset of the exception's request shape
// shared by both call sites (workspace context, squad instructions):
// the new content plus a digest of what the caller believes is currently
// live. Handlers decode these two fields directly off their existing
// request structs (see UpdateWorkspaceRequest.ExpectedBeforeDigest /
// the squad update struct's analogous field) — this type exists only to
// give hermesExceptionVerifyAndSwap one shared parameter shape rather than
// four positional strings.
type hermesExceptionRequest struct {
	// NewContent is the caller-supplied exact replacement bytes.
	NewContent string
	// ExpectedBeforeDigest is the sha256 hex digest the caller believes
	// matches the field's current live value.
	ExpectedBeforeDigest string
}

// hermesExceptionSwapResult carries the digests a successful compare-and-swap
// computed, for the handler to return to the caller (before_digest /
// after_digest in the response body) and to log.
type hermesExceptionSwapResult struct {
	BeforeDigest string
	AfterDigest  string
}

// hermesExceptionRejectReason enumerates why hermesExceptionVerifyAndSwap
// refused to write, so callers can pick the right HTTP status without
// string-matching an error message.
type hermesExceptionRejectReason int

const (
	// hermesExceptionRejectNone means the swap succeeded.
	hermesExceptionRejectNone hermesExceptionRejectReason = iota
	// hermesExceptionRejectMalformedDigest means ExpectedBeforeDigest was
	// not well-formed hex of the right length. Maps to 400.
	hermesExceptionRejectMalformedDigest
	// hermesExceptionRejectStaleDigest means ExpectedBeforeDigest was
	// well-formed but did not match the live value's current digest — the
	// live value moved since the caller last read it, or the caller never
	// actually observed the live value. Maps to 409.
	hermesExceptionRejectStaleDigest
	// hermesExceptionRejectNotFound means the target row no longer exists.
	// Maps to 404.
	hermesExceptionRejectNotFound
	// hermesExceptionRejectInternal means the read or write itself failed
	// for reasons unrelated to the digest contract. Maps to 500.
	hermesExceptionRejectInternal
)

// hermesExceptionVerifyAndSwapWorkspaceContext performs the exact-bytes
// compare-and-swap for workspace.context: inside a fresh transaction, it
// locks and re-reads the live row, computes that row's current digest, and
// refuses to write anything unless it matches req.ExpectedBeforeDigest
// exactly. This closes the "reject changed baselines" race the spec calls
// out — a concurrent human edit (or a second Hermes call using a stale
// digest) between the caller's last read and this write is caught, not
// silently overwritten.
//
// A transaction (rather than the two-query "read, compare in Go, then
// UPDATE" shape that would look simpler) is required for correctness here:
// `SELECT ... FOR UPDATE` inside the transaction blocks a concurrent writer
// from changing the row between our read and our write, so the digest we
// compare against is guaranteed still current at the moment we write. A
// bare `UPDATE ... WHERE context = $expected` without the prior locked read
// would be equivalent for correctness but would not let us distinguish "not
// found" from "digest mismatch" for the error response, and would not let
// us assert the workspace ID in the same round trip.
func (h *Handler) hermesExceptionVerifyAndSwapWorkspaceContext(ctx context.Context, workspaceID string, req hermesExceptionRequest) (hermesExceptionSwapResult, hermesExceptionRejectReason, error) {
	if !isWellFormedDigestHex(req.ExpectedBeforeDigest) {
		return hermesExceptionSwapResult{}, hermesExceptionRejectMalformedDigest, nil
	}

	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}

	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}
	defer tx.Rollback(ctx)

	var currentContext *string
	err = tx.QueryRow(ctx, `SELECT context FROM workspace WHERE id = $1 FOR UPDATE`, wsUUID).Scan(&currentContext)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return hermesExceptionSwapResult{}, hermesExceptionRejectNotFound, nil
		}
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}

	var currentBytes []byte
	if currentContext != nil {
		currentBytes = []byte(*currentContext)
	}
	beforeDigest := hermesExceptionDigest(currentBytes)
	if beforeDigest != req.ExpectedBeforeDigest {
		return hermesExceptionSwapResult{}, hermesExceptionRejectStaleDigest, nil
	}

	newBytes := []byte(req.NewContent)
	afterDigest := hermesExceptionDigest(newBytes)

	if _, err := tx.Exec(ctx, `UPDATE workspace SET context = $1 WHERE id = $2`, req.NewContent, wsUUID); err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}

	if err := tx.Commit(ctx); err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}

	return hermesExceptionSwapResult{BeforeDigest: beforeDigest, AfterDigest: afterDigest}, hermesExceptionRejectNone, nil
}

// hermesExceptionVerifyAndSwapSquadInstructions is the squad.instructions
// analogue of hermesExceptionVerifyAndSwapWorkspaceContext. squad.instructions
// is a plain non-nullable string column (unlike workspace.context), so there
// is no NULL case to handle.
func (h *Handler) hermesExceptionVerifyAndSwapSquadInstructions(ctx context.Context, squadID string, req hermesExceptionRequest) (hermesExceptionSwapResult, hermesExceptionRejectReason, error) {
	if !isWellFormedDigestHex(req.ExpectedBeforeDigest) {
		return hermesExceptionSwapResult{}, hermesExceptionRejectMalformedDigest, nil
	}

	squadUUID, err := util.ParseUUID(squadID)
	if err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}

	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}
	defer tx.Rollback(ctx)

	var currentInstructions string
	err = tx.QueryRow(ctx, `SELECT instructions FROM squad WHERE id = $1 FOR UPDATE`, squadUUID).Scan(&currentInstructions)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return hermesExceptionSwapResult{}, hermesExceptionRejectNotFound, nil
		}
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}

	beforeDigest := hermesExceptionDigest([]byte(currentInstructions))
	if beforeDigest != req.ExpectedBeforeDigest {
		return hermesExceptionSwapResult{}, hermesExceptionRejectStaleDigest, nil
	}

	afterDigest := hermesExceptionDigest([]byte(req.NewContent))

	if _, err := tx.Exec(ctx, `UPDATE squad SET instructions = $1 WHERE id = $2`, req.NewContent, squadUUID); err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}

	if err := tx.Commit(ctx); err != nil {
		return hermesExceptionSwapResult{}, hermesExceptionRejectInternal, err
	}

	return hermesExceptionSwapResult{BeforeDigest: beforeDigest, AfterDigest: afterDigest}, hermesExceptionRejectNone, nil
}

// hermesExceptionWriteRejection maps a hermesExceptionRejectReason to the
// HTTP status and message this endpoint returns for it, and writes it.
// Centralized so both call sites use identical status codes:
//
//   - malformed digest        -> 400 (request itself is invalid)
//   - stale/mismatched digest -> 409 (request is well-formed, but the
//     precondition "you are editing the value you think you are editing"
//     failed — the same 409 convention chat.go already uses for
//     "a newer reply arrived — refresh it instead")
//   - not found               -> 404
//   - anything else           -> 500
func hermesExceptionWriteRejection(w http.ResponseWriter, r *http.Request, reason hermesExceptionRejectReason, err error) {
	switch reason {
	case hermesExceptionRejectMalformedDigest:
		writeError(w, http.StatusBadRequest, "expected_before_digest must be a 64-character hex-encoded sha256 digest")
	case hermesExceptionRejectStaleDigest:
		writeError(w, http.StatusConflict, "expected_before_digest does not match the current live value; re-read and retry")
	case hermesExceptionRejectNotFound:
		writeError(w, http.StatusNotFound, "target row not found")
	default:
		slog.Warn("hermes exception write failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to apply governed field exception write")
	}
}

// logHermesExceptionOutcome writes one structured audit line per attempted
// use of this exception, success or failure, so its entire usage history is
// reconstructable from logs alone (spec requirement: "full audit logging of
// exactly what changed"). Called for both the allowed compare-and-swap path
// and for every rejection reason, so a rejected attempt is exactly as
// visible in logs as a successful one.
func logHermesExceptionOutcome(r *http.Request, targetKind, targetID string, result hermesExceptionSwapResult, reason hermesExceptionRejectReason, err error) {
	attrs := append(logger.RequestAttrs(r),
		"che_issue", "CHE-764",
		"actor_agent_id", hermesExceptionAgentID,
		"target_kind", targetKind,
		"target_id", targetID,
		"reject_reason", int(reason),
	)
	if result.BeforeDigest != "" {
		attrs = append(attrs, "before_digest", result.BeforeDigest)
	}
	if result.AfterDigest != "" {
		attrs = append(attrs, "after_digest", result.AfterDigest)
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	if reason == hermesExceptionRejectNone {
		slog.Info("hermes governed-field exception write applied", attrs...)
		return
	}
	slog.Warn("hermes governed-field exception write rejected", attrs...)
}
