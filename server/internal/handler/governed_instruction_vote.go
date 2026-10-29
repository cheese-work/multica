package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
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
//     `Proposal: <field>:<entity-id> <after_sha256>` comment. Votes are read
//     from governed_vote_record (migration 667), written when the comment is
//     created. Editing or deleting a comment afterwards changes nothing: it
//     cannot forge a PASS and cannot erase a BLOCK. Retract with a BLOCK.
//  3. The two voters are distinct agents and neither is the writer.
//  4. One voter is of a different lab than the writer (RGS-1). A lab is looked
//     up in MULTICA_VOTE_GATE_LABS by runtime provider and model. Both come
//     from the executing task, not the agent row: the runtime the task runs
//     on and the model recorded when the daemon claimed it (migration 668).
//     A voter's pair is recorded when its comment is created, from the
//     comment's source task; the writer's comes from its X-Task-ID task.
//     Unlisted or unrecorded pairs have no lab and never count (fail closed).
//  5. One voter leads the squad that owns vote_issue.
//  6. No `Vote: BLOCK ...` comment for the same line, from an agent or a member.
//     Rules 2 to 6 and the write run in one transaction under an advisory lock
//     that a BLOCK also takes, so a BLOCK cannot commit between check and write.
//  7. Every governed:human block is byte-identical before and after, and in a
//     value that holds one, all text around the blocks is unchanged except
//     inside governed:agent regions (so added text cannot override a block).
//
// Rollout: workspace.context and agent.instructions stay denied until the
// operator lists them in MULTICA_VOTE_GATE_FIELDS, and even then the live
// value must already hold a governed:human block. squad.instructions and
// project.description are open from the start.
//
// Only an agent identity (resolveActor "agent") enters; a cloud-node PAT and
// create endpoints stay denied. The write is the CHE-1300 compare-and-swap
// on before_sha256, so a value that moved after the checks is refused (409).
// Rules that must stay human (deny-list, vote rule) live in governed:human
// blocks, which check 7 protects.

const (
	voteIssueKey  = "vote_issue"
	decisionIDKey = "decision_id"
	voteBeforeKey = "before_sha256"
	voteAfterKey  = "after_sha256"

	governedHumanOpen  = "<!-- governed:human -->"
	governedHumanClose = "<!-- /governed:human -->"
	governedAgentOpen  = "<!-- governed:agent -->"
	governedAgentClose = "<!-- /governed:agent -->"
)

var ownerCASProjectDescription = ownerCASColumn{"project.description", "description", "project", "description", "workspace_id"}

// voteEntered reports whether an agent update of a governed field names a
// vote issue or a decision (CHE-2457, governed_instruction_decision.go). Any
// other agent write keeps the CHE-455 denial.
func voteEntered(actorType string, fieldWillBeWritten bool, raw map[string]json.RawMessage) bool {
	return actorType == "agent" && fieldWillBeWritten &&
		(rawFieldsHasKeyFold(raw, voteIssueKey) || rawFieldsHasKeyFold(raw, decisionIDKey))
}

// governedPublisher is the actor recorded on the update event.
func governedPublisher(r *http.Request, actorType, actorID string) (string, string) {
	if actorType == "agent" {
		return "agent", actorID
	}
	return "member", requestUserID(r)
}

// splitGovernedHuman returns every governed:human block in s, markers
// included, and the len(blocks)+1 text segments around them. ok is false for
// an unbalanced, nested or stray marker, so such a value is refused rather
// than guessed at.
func splitGovernedHuman(s string) (blocks, segments []string, ok bool) {
	for {
		open := strings.Index(s, governedHumanOpen)
		closeAt := strings.Index(s, governedHumanClose)
		if open < 0 && closeAt < 0 {
			return blocks, append(segments, s), true
		}
		if open < 0 || (closeAt >= 0 && closeAt < open) {
			return nil, nil, false
		}
		rest := s[open+len(governedHumanOpen):]
		end := strings.Index(rest, governedHumanClose)
		if end < 0 || strings.Contains(rest[:end], governedHumanOpen) {
			return nil, nil, false
		}
		segments = append(segments, s[:open])
		blocks = append(blocks, s[open:open+len(governedHumanOpen)+end+len(governedHumanClose)])
		s = rest[end+len(governedHumanClose):]
	}
}

func governedHumanBlocks(s string) (blocks []string, ok bool) {
	blocks, _, ok = splitGovernedHuman(s)
	return blocks, ok
}

// maskAgentRegions replaces each governed:agent region of a text segment with
// a fixed token. ok is false for an unbalanced or nested region marker.
func maskAgentRegions(s string) (masked string, ok bool) {
	var out strings.Builder
	for {
		open := strings.Index(s, governedAgentOpen)
		closeAt := strings.Index(s, governedAgentClose)
		if open < 0 && closeAt < 0 {
			out.WriteString(s)
			return out.String(), true
		}
		if open < 0 || (closeAt >= 0 && closeAt < open) {
			return "", false
		}
		rest := s[open+len(governedAgentOpen):]
		end := strings.Index(rest, governedAgentClose)
		if end < 0 || strings.Contains(rest[:end], governedAgentOpen) {
			return "", false
		}
		out.WriteString(s[:open])
		out.WriteString("\x00agent-region\x00")
		s = rest[end+len(governedAgentClose):]
	}
}

// protectedBlocksUnchanged is check 7. Every governed:human block must stay
// byte-identical and in order. Keeping the block bytes is not enough, because
// text added around a block can override it. So when the live value holds any
// human block, the text around the blocks must be unchanged too, except inside
// governed:agent regions, whose count and position must also be unchanged.
// A value with no human block has nothing to protect and may change freely.
func protectedBlocksUnchanged(before, after string) bool {
	b, segB, okB := splitGovernedHuman(before)
	a, segA, okA := splitGovernedHuman(after)
	if !okB || !okA || len(a) != len(b) {
		return false
	}
	for i := range b {
		if a[i] != b[i] {
			return false
		}
	}
	if len(b) == 0 {
		return true
	}
	for i := range segB {
		maskedB, okB := maskAgentRegions(segB[i])
		maskedA, okA := maskAgentRegions(segA[i])
		if !okB || !okA || maskedB != maskedA {
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

// voteComment is one governed_vote_record row: the vote and proposal lines of
// a comment as posted, with the author's runtime provider and model at that time.
type voteComment struct {
	authorType, authorID, lines, provider, model string
	createdAt                                    time.Time
}

// voteTally applies checks 2 (lines only), 3 and 6 to the records that mention
// the after digest: it returns the distinct non-writer agents with a valid PASS
// vote and whether any BLOCK exists.
func voteTally(comments []voteComment, writerID, label, after string) (voters []voteComment, blocked bool) {
	pass := "Vote: PASS " + label + " " + after
	block := "Vote: BLOCK " + label + " " + after
	proposal := "Proposal: " + label + " " + after
	var proposedAt time.Time
	for _, c := range comments {
		for _, line := range strings.Split(c.lines, "\n") {
			if strings.TrimSpace(line) == proposal && (proposedAt.IsZero() || c.createdAt.Before(proposedAt)) {
				proposedAt = c.createdAt
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range comments {
		lines := voteLines(c.lines)
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
		voters = append(voters, c)
	}
	return voters, blocked
}

// voteLab is the lab the operator registered for an exact runtime provider and
// model, or "" when unknown. MULTICA_VOTE_GATE_LABS is a JSON object such as
// {"codex/gpt-6.1-sol":"openai"}. A provider is a CLI family, not a lab, and no
// lab is inferred from a name, so an unlisted pair is never independent.
func voteLab(provider, model string) string {
	var labs map[string]string
	if provider == "" || model == "" || json.Unmarshal([]byte(os.Getenv("MULTICA_VOTE_GATE_LABS")), &labs) != nil {
		return ""
	}
	return labs[provider+"/"+model]
}

// voteFieldEnabled reports whether the vote path may write kind. Squad
// instructions and project descriptions are always open. Workspace context and
// agent instructions open only after the operator lists them in
// MULTICA_VOTE_GATE_FIELDS, which a human sets once the deny-list sections are
// wrapped in governed:human markers.
func voteFieldEnabled(kind string) bool {
	if kind == ownerCASSquadInstructions.kind || kind == ownerCASProjectDescription.kind {
		return true
	}
	for _, f := range strings.Split(os.Getenv("MULTICA_VOTE_GATE_FIELDS"), ",") {
		if strings.TrimSpace(f) == kind {
			return true
		}
	}
	return false
}

// voteNeedsMarker marks the fields that hold the deny-list: their live value
// must already carry a governed:human block before a vote can change them.
func voteNeedsMarker(kind string) bool {
	return kind == ownerCASWorkspaceContext.kind || kind == ownerCASAgentInstructions.kind
}

func hasGovernedHumanBlock(s string) bool {
	blocks, ok := governedHumanBlocks(s)
	return ok && len(blocks) > 0
}

// voteReceiptReason returns why the votes on issueID do not authorize the
// change, or "" when checks 2 to 6 pass.
func voteReceiptReason(ctx context.Context, q db.DBTX, workspaceID, issueID, writerID, writerTaskID, label, after string) (string, []string, error) {
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return "", nil, err
	}
	issueUUID, err := util.ParseUUID(issueID)
	if err != nil {
		return "", nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT author_type, author_id::text, vote_lines, runtime_provider, model, created_at FROM governed_vote_record
		WHERE issue_id = $1 AND workspace_id = $2 AND position($3 in vote_lines) > 0`,
		issueUUID, wsUUID, after)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var comments []voteComment
	for rows.Next() {
		var c voteComment
		if err := rows.Scan(&c.authorType, &c.authorID, &c.lines, &c.provider, &c.model, &c.createdAt); err != nil {
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

	// The writer's lab is that of the task its token belongs to: the runtime
	// the task runs on and the model recorded when the daemon claimed it. The
	// agent row is not read, because the writer can edit it mid-run. Voters'
	// labs were recorded the same way when they voted. A voter that is no
	// longer a live agent of this workspace does not count.
	taskUUID, err := util.ParseUUID(writerTaskID)
	if err != nil {
		return "the writing agent's task is unknown", nil, nil
	}
	var writerProvider, writerModel string
	err = q.QueryRow(ctx, `
		SELECT COALESCE(r.provider, ''), COALESCE(t.claimed_model, '') FROM agent_task_queue t
		JOIN agent a ON a.id = t.agent_id AND a.workspace_id = $3 AND a.archived_at IS NULL
		JOIN agent_runtime r ON r.id = t.runtime_id
		WHERE t.id = $1 AND t.agent_id = $2 AND t.status IN ('dispatched', 'running')`,
		taskUUID, writerID, wsUUID).Scan(&writerProvider, &writerModel)
	if errors.Is(err, pgx.ErrNoRows) {
		return "the writing agent has no live task", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	writerLab := voteLab(writerProvider, writerModel)
	if writerLab == "" {
		return "the writing task's provider and model are not in the lab registry", nil, nil
	}

	voterIDs := make([]string, len(voters))
	for i, v := range voters {
		voterIDs[i] = v.authorID
	}
	liveRows, err := q.Query(ctx, `SELECT id::text FROM agent WHERE workspace_id = $1 AND archived_at IS NULL AND id::text = ANY($2) FOR SHARE`, wsUUID, voterIDs)
	if err != nil {
		return "", nil, err
	}
	defer liveRows.Close()
	liveAgents := map[string]bool{}
	for liveRows.Next() {
		var id string
		if err := liveRows.Scan(&id); err != nil {
			return "", nil, err
		}
		liveAgents[id] = true
	}
	if err := liveRows.Err(); err != nil {
		return "", nil, err
	}

	var leaderID string
	err = q.QueryRow(ctx, `
		SELECT s.leader_id::text FROM issue i
		JOIN squad s ON s.id = i.assignee_id AND s.workspace_id = i.workspace_id
		WHERE i.id = $1 AND i.workspace_id = $2 AND i.assignee_type = 'squad' AND s.archived_at IS NULL FOR SHARE OF s`,
		issueUUID, wsUUID).Scan(&leaderID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", nil, err
	}

	var live []string
	crossLab, leads := false, false
	for _, v := range voters {
		lab := voteLab(v.provider, v.model)
		if !liveAgents[v.authorID] || lab == "" {
			continue
		}
		live = append(live, v.authorID)
		crossLab = crossLab || lab != writerLab
		leads = leads || v.authorID == leaderID
	}
	switch {
	case len(live) < 2:
		return "fewer than two valid PASS votes from distinct non-writer agents of known lab", nil, nil
	case !crossLab:
		return "no PASS vote from a different lab than the writer", nil, nil
	case !leads:
		return "no PASS vote from the leader of the squad that owns the vote issue", nil, nil
	}
	return "", live, nil
}

// governedApply routes an update to the agent path (an agent that passed
// voteEntered) or the owner-only CAS path.
func (h *Handler) governedApply(w http.ResponseWriter, r *http.Request, actorType, actorID, role string, raw map[string]json.RawMessage, c ownerCASColumn, workspaceID, id string) (hermesExceptionSwapResult, bool) {
	if voteEntered(actorType, true, raw) {
		return h.agentGovernedApply(w, r, actorID, raw, c, workspaceID, id)
	}
	return h.ownerCASApply(w, r, actorType, role, raw, c, workspaceID, id)
}

// agentGovernedApply sends an agent write to the vote receipt or to the
// decision binding. The two are alternatives: a body naming both is refused.
func (h *Handler) agentGovernedApply(w http.ResponseWriter, r *http.Request, writerID string, raw map[string]json.RawMessage, c ownerCASColumn, workspaceID, id string) (hermesExceptionSwapResult, bool) {
	hasVote, hasDecision := rawFieldsHasKeyFold(raw, voteIssueKey), rawFieldsHasKeyFold(raw, decisionIDKey)
	switch {
	case hasVote && hasDecision:
		writeError(w, http.StatusBadRequest, voteIssueKey+" and "+decisionIDKey+" are alternatives; send one")
		return hermesExceptionSwapResult{}, false
	case hasDecision:
		return h.decisionApply(w, r, writerID, raw, c, workspaceID, id)
	}
	return h.voteApply(w, r, writerID, raw, c, workspaceID, id)
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

	if !voteFieldEnabled(c.kind) {
		return deny("the vote path is not enabled for "+c.kind, nil)
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
	issueUUID, err := util.ParseUUID(issueID)
	if err != nil || !isWellFormedDigestHex(before) || !isWellFormedDigestHex(after) {
		writeError(w, http.StatusBadRequest, voteIssueKey+" must be an issue id and "+voteBeforeKey+"/"+voteAfterKey+" 64-character hex sha256 digests")
		return hermesExceptionSwapResult{}, false
	}
	before, after = strings.ToLower(before), strings.ToLower(after)
	// One spelling everywhere: the BLOCK trigger locks the issue id as Postgres
	// prints it, so the writer must lock the parsed id, not the client's text.
	issueID = uuidToString(issueUUID)

	if !h.instructionTokenLimitOK(w, r, c, workspaceID, id, content) {
		return hermesExceptionSwapResult{}, false
	}

	wsUUID, _ := util.ParseUUID(workspaceID)
	idUUID, err := util.ParseUUID(id)
	if err != nil {
		return deny("", err)
	}
	live, err := h.governedLiveValue(r.Context(), c, wsUUID, idUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "target row not found")
		return hermesExceptionSwapResult{}, false
	}
	if err != nil {
		return deny("", err)
	}

	if reason := governedChangeRefusal(live, content, before, after); reason != "" {
		return deny(reason, nil)
	}
	if voteNeedsMarker(c.kind) && !hasGovernedHumanBlock(live) {
		return deny("the live value has no governed:human block", nil)
	}
	// Vote check and write share one transaction under the vote issue's
	// advisory lock. A BLOCK holds the same lock until its own commit, so a
	// BLOCK committed before this point is seen here, and a later one waits
	// until the write has committed. Leader and voter rows are share-locked.
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		return deny("", err)
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "governed_vote:"+issueID); err != nil {
		return deny("", err)
	}
	reason, voters, err := voteReceiptReason(r.Context(), tx, workspaceID, issueID, writerID, r.Header.Get("X-Task-ID"), c.field+":"+id, after)
	if err != nil || reason != "" {
		return deny(reason, err)
	}

	result, rejected, err := ownerCASWriteTx(r.Context(), tx, c, workspaceID, id, content, before)
	if rejected == hermesExceptionRejectNone {
		if err = tx.Commit(r.Context()); err != nil {
			rejected = hermesExceptionRejectInternal
		}
	}
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
	result, ok := h.agentGovernedApply(w, r, actorID, raw, ownerCASProjectDescription, workspaceID, uuidToString(prev.ID))
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
