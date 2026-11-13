package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-3490 (layer 1 of CHE-3250): PASS-receipt write for agent.instructions.
// An agent may write it when one agent of another lab has recorded, on
// receipt_issue, the two lines below (design: CHE-3250):
//
//	Proposal: agent.instructions:<agent-id> <after_sha256>
//	Vote: PASS agent.instructions:<agent-id> <after_sha256> from <before_sha256>
//
// Request: {"instructions": <new value>, "receipt_issue": <issue id>, "before_sha256": ..., "after_sha256": ...}
//
// Lines come from governed_vote_record (migration 667), so editing or deleting
// the comment later changes nothing. Checks, all in one transaction, else 403:
//
//  1. Only agent.instructions; any other governed field is refused.
//  2. The writer is in delegated_governed_writes.allowed_agents for the field.
//  3. before_sha256 is the live value, after_sha256 the new value.
//  4. Every governed:human block is byte-identical, and the live value holds one.
//  5. The PASS line is the only vote line of an agent comment posted after the
//     Proposal line, and the digests in it are the ones in the request.
//  6. The reviewer is not the writer and is a live agent of the workspace.
//  7. Reviewer lab and writer lab are both known and differ (RGS-1).
//  8. No `Vote: BLOCK` line for the same target and digest, from anyone.
//  9. Single use: governed_pass_receipt.comment_id is inserted in the write's
//     transaction, so a refused write consumes nothing; a used receipt is 409.
//  10. The write is the CHE-1300 compare-and-swap on before_sha256.
//
// The BLOCK trigger takes the same per-issue advisory lock as the vote path.

const receiptIssueKey = "receipt_issue"

func receiptEntered(raw map[string]json.RawMessage) bool {
	return rawFieldsHasKeyFold(raw, receiptIssueKey)
}

// receiptRecord is one governed_vote_record row plus its comment id.
type receiptRecord struct {
	voteComment
	commentID string
}

// proposalBeforePass reports whether one comment's recorded lines hold the
// Proposal line above the PASS line: the design's receipt is a single comment
// by the reviewer, and both lines carry the comment's one timestamp.
func proposalBeforePass(lines, proposal, pass string) bool {
	proposalAt := -1
	for i, line := range strings.Split(lines, "\n") {
		switch strings.TrimSpace(line) {
		case proposal:
			if proposalAt < 0 {
				proposalAt = i
			}
		case pass:
			return proposalAt >= 0
		}
	}
	return false
}

// receiptTally returns the PASS records that bind this exact change and
// whether any BLOCK exists. A PASS counts once per author and only after the
// earliest matching Proposal: in a later comment, or above it in the same one.
func receiptTally(records []receiptRecord, writerID, label, before, after string) (passes []receiptRecord, blocked bool) {
	proposal := "Proposal: " + label + " " + after
	block := "Vote: BLOCK " + label + " " + after
	pass := "Vote: PASS " + label + " " + after + " from " + before
	var proposedAt time.Time
	for _, rec := range records {
		for _, line := range strings.Split(rec.lines, "\n") {
			if strings.TrimSpace(line) == proposal && (proposedAt.IsZero() || rec.createdAt.Before(proposedAt)) {
				proposedAt = rec.createdAt
			}
		}
	}
	seen := map[string]bool{}
	for _, rec := range records {
		lines := voteLines(rec.lines)
		for _, l := range lines {
			blocked = blocked || strings.HasPrefix(l, block)
		}
		if len(lines) != 1 || lines[0] != pass || rec.authorType != "agent" || rec.authorID == writerID ||
			proposedAt.IsZero() || !(rec.createdAt.After(proposedAt) || proposalBeforePass(rec.lines, proposal, pass)) || seen[rec.authorID] {
			continue
		}
		seen[rec.authorID] = true
		passes = append(passes, rec)
	}
	return passes, blocked
}

// receiptReviewers returns the receipts whose reviewer may sign for the
// writer, or why none may (checks 5 to 8).
func receiptReviewers(ctx context.Context, q db.DBTX, wsUUID, issueUUID pgtype.UUID, writerID, writerTaskID, label, before, after string) ([]receiptRecord, string, error) {
	rows, err := q.Query(ctx, `
		SELECT comment_id::text, author_type, author_id::text, vote_lines, runtime_provider, model, created_at FROM governed_vote_record
		WHERE issue_id = $1 AND workspace_id = $2 AND position($3 in vote_lines) > 0`,
		issueUUID, wsUUID, after)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var records []receiptRecord
	for rows.Next() {
		var rec receiptRecord
		if err := rows.Scan(&rec.commentID, &rec.authorType, &rec.authorID, &rec.lines, &rec.provider, &rec.model, &rec.createdAt); err != nil {
			return nil, "", err
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	passes, blocked := receiptTally(records, writerID, label, before, after)
	if blocked {
		return nil, "a BLOCK vote exists", nil
	}
	if len(passes) == 0 {
		return nil, "no PASS receipt from an agent other than the writer for this change", nil
	}
	writerLab, reason, err := writerTaskLab(ctx, q, wsUUID, writerID, writerTaskID)
	if err != nil || reason != "" {
		return nil, reason, err
	}
	ids := make([]string, len(passes))
	for i, p := range passes {
		ids[i] = p.authorID
	}
	live, err := liveAgentSet(ctx, q, wsUUID, ids)
	if err != nil {
		return nil, "", err
	}
	known, eligible := false, []receiptRecord{}
	for _, p := range passes {
		lab := voteLab(p.provider, p.model)
		if !live[p.authorID] || lab == "" {
			continue
		}
		known = true
		if lab != writerLab {
			eligible = append(eligible, p)
		}
	}
	switch {
	case !known:
		return nil, "the reviewer is not a live agent of known lab", nil
	case len(eligible) == 0:
		return nil, "the reviewer is of the same lab as the writer", nil
	}
	return eligible, "", nil
}

// receiptApply runs checks 1 to 10 and the write for one agent request; ok is
// true only after the write committed. Every failure response is already written.
func (h *Handler) receiptApply(w http.ResponseWriter, r *http.Request, writerID string, raw map[string]json.RawMessage, c ownerCASColumn, workspaceID, id string) (hermesExceptionSwapResult, bool) {
	var issueID string
	fail := func(status int, message, denied string, err error) (hermesExceptionSwapResult, bool) {
		attrs := append(logger.RequestAttrs(r), "che_issue", "CHE-3490", "actor_agent_id", writerID,
			"task_id", r.Header.Get("X-Task-ID"), "target_kind", c.kind, "target_id", id, "receipt_issue", issueID, "denied", denied)
		if err != nil {
			attrs = append(attrs, "error", err)
		}
		slog.Warn("pass-receipt governed-field write refused", attrs...)
		writeError(w, status, message)
		return hermesExceptionSwapResult{}, false
	}
	deny := func(reason string) (hermesExceptionSwapResult, bool) {
		return fail(http.StatusForbidden, "receipt rejected: "+reason, reason, nil)
	}
	internal := func(err error) (hermesExceptionSwapResult, bool) {
		return fail(http.StatusInternalServerError, "failed to verify receipt", "", err)
	}

	if c.kind != ownerCASAgentInstructions.kind {
		return deny("the receipt path is not enabled for " + c.kind)
	}
	if !hermesExceptionOnlyAllowedKeys(raw, c.field, receiptIssueKey, voteBeforeKey, voteAfterKey) {
		return fail(http.StatusBadRequest, "this request may only include "+c.field+", "+receiptIssueKey+", "+voteBeforeKey+" and "+voteAfterKey, "bad keys", nil)
	}
	content, ok := ownerCASRawString(raw, c.field)
	if !ok || strings.ContainsRune(content, 0) {
		return fail(http.StatusBadRequest, c.field+" must be present exactly once as a string without NUL bytes", "bad content", nil)
	}
	issueRaw, _ := ownerCASRawString(raw, receiptIssueKey)
	before, _ := ownerCASRawString(raw, voteBeforeKey)
	after, _ := ownerCASRawString(raw, voteAfterKey)
	issueUUID, err := util.ParseUUID(issueRaw)
	if err != nil || !isWellFormedDigestHex(before) || !isWellFormedDigestHex(after) {
		return fail(http.StatusBadRequest, receiptIssueKey+" must be an issue id and "+voteBeforeKey+"/"+voteAfterKey+" 64-character hex sha256 digests", "bad ids", nil)
	}
	before, after = strings.ToLower(before), strings.ToLower(after)
	issueID = uuidToString(issueUUID) // the BLOCK trigger locks the id as Postgres prints it

	if !h.instructionTokenLimitOK(w, r, c, workspaceID, id, content) {
		return hermesExceptionSwapResult{}, false
	}
	wsUUID, _ := util.ParseUUID(workspaceID)
	idUUID, err := util.ParseUUID(id)
	if err != nil {
		return internal(err)
	}
	requiresHuman, allowed := h.delegatedWriteAllowed(r.Context(), wsUUID, writerID, c.kind)
	if !allowed {
		return deny("the writer is not delegated to write " + c.kind)
	}
	if requiresHuman {
		// A peer receipt is not a human decision: only an owner who set
		// requires_human_decision to false opens this path.
		return deny("the workspace requires a human decision for " + c.kind)
	}
	live, err := h.governedLiveValue(r.Context(), c, wsUUID, idUUID)
	if err != nil {
		return internal(err)
	}
	if reason := governedChangeRefusal(live, content, before, after); reason != "" {
		return deny(reason)
	}
	if !hasGovernedHumanBlock(live) {
		return deny("the live value has no governed:human block")
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		return internal(err)
	}
	defer tx.Rollback(r.Context())
	reviewerID, reason, err := claimPassReceipt(r.Context(), tx, wsUUID, issueUUID, idUUID, writerID, r.Header.Get("X-Task-ID"), c.kind, before, after)
	if err != nil {
		return internal(err)
	}
	if reason == receiptUsed {
		return fail(http.StatusConflict, receiptUsed, receiptUsed, nil)
	}
	if reason != "" {
		return deny(reason)
	}

	result, rejected, err := ownerCASWriteTx(r.Context(), tx, c, workspaceID, id, content, before)
	if rejected == hermesExceptionRejectNone {
		if err = tx.Commit(r.Context()); err != nil {
			rejected = hermesExceptionRejectInternal
		}
	}
	if rejected != hermesExceptionRejectNone {
		hermesExceptionWriteRejection(w, r, rejected, err)
		return hermesExceptionSwapResult{}, false
	}
	slog.Info("pass-receipt governed-field write applied", append(logger.RequestAttrs(r), "che_issue", "CHE-3490",
		"actor_agent_id", writerID, "target_kind", c.kind, "target_id", id, "receipt_issue", issueID, "reviewer", reviewerID)...)
	return result, true
}

const receiptUsed = "receipt already used"

// claimPassReceipt locks the receipt issue, checks 5 to 8 for the change to
// field:targetID, and records one single-use row in tx. reason is non-empty
// when no receipt may sign (receiptUsed when every one was spent); the caller
// commits tx with the write, so a refused write consumes nothing.
func claimPassReceipt(ctx context.Context, tx db.DBTX, wsUUID, issueUUID, targetUUID pgtype.UUID, writerID, writerTaskID, field, before, after string) (reviewerID, reason string, err error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "governed_vote:"+uuidToString(issueUUID)); err != nil {
		return "", "", err
	}
	reviewers, reason, err := receiptReviewers(ctx, tx, wsUUID, issueUUID, writerID, writerTaskID, field+":"+uuidToString(targetUUID), before, after)
	if err != nil || reason != "" {
		return "", reason, err
	}
	for _, rec := range reviewers {
		tag, err := tx.Exec(ctx, `
			INSERT INTO governed_pass_receipt (comment_id, workspace_id, issue_id, reviewer_id, writer_id, field, target_id, before_sha256, after_sha256)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid, $6, $7, $8, $9) ON CONFLICT (comment_id) DO NOTHING`,
			rec.commentID, wsUUID, issueUUID, rec.authorID, writerID, field, targetUUID, before, after)
		if err != nil {
			return "", "", err
		}
		if tag.RowsAffected() == 1 {
			return rec.authorID, "", nil
		}
	}
	return "", receiptUsed, nil
}
