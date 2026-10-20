package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-1862: vote-receipt exception to CHE-455. An agent may update a governed
// field (workspace.context, squad.instructions, agent.instructions,
// project.description) when the request carries a receipt for the exact change:
//
//	{<field>: <new value>, "vote_issue": <issue id>, "before_sha256": ..., "after_sha256": ...}
//
// Design: CHE-1862 design-vote-gate.md. Every check must pass, else 403:
//
//  1. before_sha256 is the sha256 of the live value, after_sha256 of the new value.
//  2. vote_issue holds two PASS votes: a comment by an agent with exactly one
//     `Vote:` line for this field, entity and after digest —
//     `Vote: PASS <field>:<entity-id> <after_sha256>` — posted after a
//     `Proposal: <field>:<entity-id> <after_sha256>` comment.
//  3. The two voters are distinct agents and neither is the writer.
//  4. One voter runs on a different runtime provider than the writer (RGS-1).
//  5. One voter leads the squad that owns vote_issue.
//  6. No `Vote: BLOCK ...` comment for the same line, from an agent or a member.
//  7. Every governed:human block is byte-identical before and after.
//
// Only an agent identity (resolveActor "agent") enters; a cloud-node PAT and
// create endpoints stay denied. The write is the CHE-1300 compare-and-swap
// on before_sha256, so a value that moved after the checks is refused (409).
// Rules that must stay human (deny-list, vote rule) live in governed:human
// blocks, which check 7 protects.

const (
	voteIssueKey  = "vote_issue"
	voteBeforeKey = "before_sha256"
	voteAfterKey  = "after_sha256"

	governedHumanOpen  = "<!-- governed:human -->"
	governedHumanClose = "<!-- /governed:human -->"
)

var ownerCASProjectDescription = ownerCASColumn{"project.description", "description", "project", "description", "workspace_id"}

// voteEntered reports whether an agent update of a governed field names a
// vote issue. Any other agent write keeps the CHE-455 denial.
func voteEntered(actorType string, fieldWillBeWritten bool, raw map[string]json.RawMessage) bool {
	return actorType == "agent" && fieldWillBeWritten && rawFieldsHasKeyFold(raw, voteIssueKey)
}

// governedPublisher is the actor recorded on the update event.
func governedPublisher(r *http.Request, actorType, actorID string) (string, string) {
	if actorType == "agent" {
		return "agent", actorID
	}
	return "member", requestUserID(r)
}

// governedHumanBlocks returns every governed:human block in s, markers
// included. ok is false for an unbalanced, nested or stray marker, so such a
// value is refused rather than guessed at.
func governedHumanBlocks(s string) (blocks []string, ok bool) {
	for {
		open := strings.Index(s, governedHumanOpen)
		closeAt := strings.Index(s, governedHumanClose)
		if open < 0 && closeAt < 0 {
			return blocks, true
		}
		if open < 0 || (closeAt >= 0 && closeAt < open) {
			return nil, false
		}
		rest := s[open+len(governedHumanOpen):]
		end := strings.Index(rest, governedHumanClose)
		if end < 0 || strings.Contains(rest[:end], governedHumanOpen) {
			return nil, false
		}
		blocks = append(blocks, s[open:open+len(governedHumanOpen)+end+len(governedHumanClose)])
		s = rest[end+len(governedHumanClose):]
	}
}

// protectedBlocksUnchanged is check 7.
func protectedBlocksUnchanged(before, after string) bool {
	b, okB := governedHumanBlocks(before)
	a, okA := governedHumanBlocks(after)
	if !okB || !okA || len(a) != len(b) {
		return false
	}
	for i := range b {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// voteLines returns the trimmed `Vote:` lines of a comment.
func voteLines(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "Vote:") {
			out = append(out, line)
		}
	}
	return out
}

type voteComment struct {
	authorType, authorID, content string
	createdAt                     time.Time
}

// voteTally applies checks 2 (comments only), 3 and 6 to the comments that
// mention the after digest: it returns the distinct non-writer agents with a
// valid PASS vote and whether any BLOCK exists.
func voteTally(comments []voteComment, writerID, label, after string) (voters []string, blocked bool) {
	pass := "Vote: PASS " + label + " " + after
	block := "Vote: BLOCK " + label + " " + after
	proposal := "Proposal: " + label + " " + after
	var proposedAt time.Time
	for _, c := range comments {
		for _, line := range strings.Split(c.content, "\n") {
			if strings.TrimSpace(line) == proposal && (proposedAt.IsZero() || c.createdAt.Before(proposedAt)) {
				proposedAt = c.createdAt
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range comments {
		lines := voteLines(c.content)
		for _, l := range lines {
			if l == block {
				blocked = true
			}
		}
		if len(lines) != 1 || lines[0] != pass || c.authorType != "agent" || c.authorID == writerID ||
			proposedAt.IsZero() || !c.createdAt.After(proposedAt) || seen[c.authorID] {
			continue
		}
		seen[c.authorID] = true
		voters = append(voters, c.authorID)
	}
	return voters, blocked
}

// voteReceiptReason returns why the votes on issueID do not authorize the
// change, or "" when checks 2 to 6 pass.
func (h *Handler) voteReceiptReason(ctx context.Context, workspaceID, issueID, writerID, label, after string) (string, []string, error) {
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return "", nil, err
	}
	issueUUID, err := util.ParseUUID(issueID)
	if err != nil {
		return "", nil, err
	}
	rows, err := h.DB.Query(ctx, `
		SELECT author_type, author_id::text, content, created_at FROM comment
		WHERE issue_id = $1 AND workspace_id = $2 AND deleted_at IS NULL
		  AND author_type IN ('agent', 'member') AND position($3 in content) > 0`,
		issueUUID, wsUUID, after)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var comments []voteComment
	for rows.Next() {
		var c voteComment
		if err := rows.Scan(&c.authorType, &c.authorID, &c.content, &c.createdAt); err != nil {
			return "", nil, err
		}
		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	voters, blocked := voteTally(comments, writerID, label, after)
	if blocked {
		return "a BLOCK vote exists", nil, nil
	}

	// Voter and writer providers; a voter that is not a live agent of this
	// workspace does not count.
	providers := map[string]string{}
	arows, err := h.DB.Query(ctx, `
		SELECT a.id::text, COALESCE(r.provider, '') FROM agent a
		LEFT JOIN agent_runtime r ON r.id = a.runtime_id
		WHERE a.workspace_id = $1 AND a.archived_at IS NULL AND a.id::text = ANY($2)`,
		wsUUID, append([]string{writerID}, voters...))
	if err != nil {
		return "", nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var id, provider string
		if err := arows.Scan(&id, &provider); err != nil {
			return "", nil, err
		}
		providers[id] = provider
	}
	if err := arows.Err(); err != nil {
		return "", nil, err
	}

	var leaderID string
	err = h.DB.QueryRow(ctx, `
		SELECT s.leader_id::text FROM issue i
		JOIN squad s ON s.id = i.assignee_id AND s.workspace_id = i.workspace_id
		WHERE i.id = $1 AND i.workspace_id = $2 AND i.assignee_type = 'squad' AND s.archived_at IS NULL`,
		issueUUID, wsUUID).Scan(&leaderID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", nil, err
	}

	var live []string
	crossLab, leads := false, false
	for _, v := range voters {
		provider, ok := providers[v]
		if !ok {
			continue
		}
		live = append(live, v)
		crossLab = crossLab || (provider != "" && providers[writerID] != "" && provider != providers[writerID])
		leads = leads || v == leaderID
	}
	switch {
	case len(live) < 2:
		return "fewer than two valid PASS votes from distinct non-writer agents", nil, nil
	case !crossLab:
		return "no PASS vote from a different provider than the writer", nil, nil
	case !leads:
		return "no PASS vote from the leader of the squad that owns the vote issue", nil, nil
	}
	return "", live, nil
}

// governedApply routes an update to the vote path (an agent that passed
// voteEntered) or the owner-only CAS path.
func (h *Handler) governedApply(w http.ResponseWriter, r *http.Request, actorType, actorID, role string, raw map[string]json.RawMessage, c ownerCASColumn, workspaceID, id string) (hermesExceptionSwapResult, bool) {
	if actorType == "agent" && rawFieldsHasKeyFold(raw, voteIssueKey) {
		return h.voteApply(w, r, actorID, raw, c, workspaceID, id)
	}
	return h.ownerCASApply(w, r, actorType, role, raw, c, workspaceID, id)
}

// voteApply runs checks 1 to 7 and the write for one agent request; ok is true
// only after the write committed. Every failure response is already written.
func (h *Handler) voteApply(w http.ResponseWriter, r *http.Request, writerID string, raw map[string]json.RawMessage, c ownerCASColumn, workspaceID, id string) (hermesExceptionSwapResult, bool) {
	deny := func(reason string, err error) (hermesExceptionSwapResult, bool) {
		logVoteOutcome(r, c, writerID, id, "", nil, reason, err)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to verify vote receipt")
		} else {
			writeError(w, http.StatusForbidden, "vote receipt rejected: "+reason)
		}
		return hermesExceptionSwapResult{}, false
	}

	if !hermesExceptionOnlyAllowedKeys(raw, c.field, voteIssueKey, voteBeforeKey, voteAfterKey) {
		writeError(w, http.StatusBadRequest, "this request may only include "+c.field+", "+voteIssueKey+", "+voteBeforeKey+" and "+voteAfterKey)
		return hermesExceptionSwapResult{}, false
	}
	content, ok := ownerCASRawString(raw, c.field)
	if !ok || strings.ContainsRune(content, 0) {
		writeError(w, http.StatusBadRequest, c.field+" must be present exactly once as a string without NUL bytes")
		return hermesExceptionSwapResult{}, false
	}
	issueID, _ := ownerCASRawString(raw, voteIssueKey)
	before, _ := ownerCASRawString(raw, voteBeforeKey)
	after, _ := ownerCASRawString(raw, voteAfterKey)
	if _, err := util.ParseUUID(issueID); err != nil || !isWellFormedDigestHex(before) || !isWellFormedDigestHex(after) {
		writeError(w, http.StatusBadRequest, voteIssueKey+" must be an issue id and "+voteBeforeKey+"/"+voteAfterKey+" 64-character hex sha256 digests")
		return hermesExceptionSwapResult{}, false
	}
	before, after = strings.ToLower(before), strings.ToLower(after)

	if !h.instructionTokenLimitOK(w, r, c, workspaceID, id, content) {
		return hermesExceptionSwapResult{}, false
	}

	wsUUID, _ := util.ParseUUID(workspaceID)
	idUUID, err := util.ParseUUID(id)
	if err != nil {
		return deny("", err)
	}
	var live string
	err = h.DB.QueryRow(r.Context(), fmt.Sprintf(`SELECT COALESCE(%[1]s, '') FROM %[2]s WHERE id = $1 AND %[3]s = $2`, c.col, c.table, c.scopeCol), idUUID, wsUUID).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "target row not found")
		return hermesExceptionSwapResult{}, false
	}
	if err != nil {
		return deny("", err)
	}

	switch {
	case hermesExceptionDigest([]byte(live)) != before:
		return deny(voteBeforeKey+" does not match the live value", nil)
	case hermesExceptionDigest([]byte(content)) != after:
		return deny(voteAfterKey+" does not match the new value", nil)
	case !protectedBlocksUnchanged(live, content):
		return deny("a governed:human block changed", nil)
	}
	reason, voters, err := h.voteReceiptReason(r.Context(), workspaceID, issueID, writerID, c.field+":"+id, after)
	if err != nil || reason != "" {
		return deny(reason, err)
	}

	result, rejected, err := h.ownerCASWrite(r.Context(), c, workspaceID, id, content, before)
	logVoteOutcome(r, c, writerID, id, issueID, voters, "", err)
	if rejected != hermesExceptionRejectNone {
		hermesExceptionWriteRejection(w, r, rejected, err)
		return hermesExceptionSwapResult{}, false
	}
	return result, true
}

// logVoteOutcome writes one audit line per attempt. Ids and digests only.
func logVoteOutcome(r *http.Request, c ownerCASColumn, writerID, id, issueID string, voters []string, denied string, err error) {
	attrs := append(logger.RequestAttrs(r),
		"che_issue", "CHE-1862",
		"actor_agent_id", writerID,
		"task_id", r.Header.Get("X-Task-ID"),
		"target_kind", c.kind,
		"target_id", id,
		"vote_issue", issueID,
		"voters", strings.Join(voters, ","),
	)
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	if denied != "" || err != nil {
		slog.Warn("vote-gated governed-field write refused", append(attrs, "denied", denied)...)
		return
	}
	slog.Info("vote-gated governed-field write applied", attrs...)
}

// voteUpdateProject is the project.description analogue of ownerCASUpdateAgent.
func (h *Handler) voteUpdateProject(w http.ResponseWriter, r *http.Request, workspaceID string, prev db.Project, actorID string, raw map[string]json.RawMessage) {
	result, ok := h.voteApply(w, r, actorID, raw, ownerCASProjectDescription, workspaceID, uuidToString(prev.ID))
	if !ok {
		return
	}
	wsUUID, _ := util.ParseUUID(workspaceID)
	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{ID: prev.ID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reload project after guarded write")
		return
	}
	resp := projectToResponse(project)
	resp.IssueCount, resp.DoneCount = h.loadProjectIssueStats(r.Context(), wsUUID, project.ID)
	resp.ResourceCount = h.loadProjectResourceCount(r.Context(), project.ID)
	h.publish(protocol.EventProjectUpdated, workspaceID, "agent", actorID, map[string]any{"project": resp})
	writeJSON(w, http.StatusOK, struct {
		ProjectResponse
		BeforeDigest string `json:"before_digest"`
		AfterDigest  string `json:"after_digest"`
	}{resp, result.BeforeDigest, result.AfterDigest})
}
