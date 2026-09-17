package handler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/checkpoint"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// loadIssueCheckpointBlock is the claim-time entry point for CHE-489/CHE-593:
// it loads the prior checkpoint for (issue, agent) if one exists, diffs it
// against a live roots-only comment scan (the same substrate
// `comment list --roots-only --summary` reads), persists the refreshed
// checkpoint, and returns the rendered block for Task.CheckpointBlock.
//
// It never invokes a model and never drops an Obligation/Blocker/Evidence
// entry (CHE-482 v2 stop condition) — those fields are carried forward
// verbatim from the prior checkpoint, since this claim-time step only
// updates the coverage cursor, not the content contract fields themselves.
// A later run that resolves an obligation is expected to write a fresh
// checkpoint (via the same upsert path) with that obligation removed; this
// function's job is coverage tracking, not obligation resolution.
//
// Best-effort: this function's load-bearing safety property is that it
// never fails the claim — it returns string, not (string, error). What a
// failure actually returns varies by stage: a failed live scan or a
// truncated/incomplete one returns empty rather than risk laundering a
// degraded read into durable coverage; a failed prior-checkpoint decode
// falls back to cold start; a failed next-checkpoint encode returns the
// PRIOR block (not empty) since the render already succeeded and only the
// write failed. In every case the claim itself proceeds.
func (h *Handler) loadIssueCheckpointBlock(ctx context.Context, issue db.Issue, agentID pgtype.UUID) string {
	if !agentID.Valid {
		return ""
	}

	live, truncated, err := h.scanRootCoverageForIssue(ctx, issue)
	if err != nil {
		slog.Warn("checkpoint: live comment scan failed; skipping checkpoint block",
			"issue_id", uuidToString(issue.ID), "agent_id", uuidToString(agentID), "error", err)
		return ""
	}
	if truncated {
		// A checkpoint's coverage cursor asserts it has seen every thread. A
		// truncated scan (commentHardCap) cannot back that assertion — the
		// oldest root(s) past the cap are invisible to this read — so
		// persisting it would silently drop them from future coverage
		// instead of leaving them correctly "uncovered". Skip the write
		// entirely rather than launder a partial read into durable state;
		// the existing prior checkpoint (if any) is left untouched for the
		// next claim to retry against.
		slog.Warn("checkpoint: live comment scan truncated; skipping checkpoint refresh this claim",
			"issue_id", uuidToString(issue.ID), "agent_id", uuidToString(agentID))
		return ""
	}

	candidateID := h.TaskService.ResolveIssueReviewSHA(ctx, issue.ID)

	priorRow, err := h.Queries.GetIssueCheckpoint(ctx, db.GetIssueCheckpointParams{
		IssueID:     issue.ID,
		AgentID:     agentID,
		WorkspaceID: issue.WorkspaceID,
	})
	hadPrior := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("checkpoint: load prior checkpoint failed; treating as cold start",
			"issue_id", uuidToString(issue.ID), "agent_id", uuidToString(agentID), "error", err)
	}

	var (
		prior       checkpoint.Checkpoint
		changed     []string
		block       string
		carryOver   checkpoint.Checkpoint
		priorExists bool
	)
	if hadPrior {
		prior, err = checkpoint.FromRow(uuidToString(issue.ID), uuidToString(agentID), rowFromDB(priorRow))
		if err != nil {
			slog.Warn("checkpoint: decode prior checkpoint failed; treating as cold start",
				"issue_id", uuidToString(issue.ID), "agent_id", uuidToString(agentID), "error", err)
		} else {
			priorExists = true
		}
	}

	if priorExists {
		changed, _ = checkpoint.EvaluateAgainstScan(prior, issue.Revision, candidateID, live)
		carryOver = prior
		block = checkpoint.Render(prior, changed)
	}
	// Cold start (no usable prior checkpoint): every live thread is
	// "changed" from the perspective of the next checkpoint's coverage
	// cursor, but there is no content-contract carry-over and nothing to
	// render — the daemon falls back to the ordinary comment-scan
	// instructions already in the brief, exactly as an old server would.

	next := checkpoint.BuildFromScan(uuidToString(issue.ID), uuidToString(agentID), issue.Revision, candidateID, live, carryOver)
	nextRow, err := next.ToRow()
	if err != nil {
		slog.Warn("checkpoint: encode next checkpoint failed; not persisted",
			"issue_id", uuidToString(issue.ID), "agent_id", uuidToString(agentID), "error", err)
		return block
	}
	if _, err := h.Queries.UpsertIssueCheckpoint(ctx, db.UpsertIssueCheckpointParams{
		WorkspaceID:         issue.WorkspaceID,
		IssueID:             issue.ID,
		AgentID:             agentID,
		IssueRevision:       nextRow.IssueRevision,
		CandidateID:         nextRow.CandidateID,
		Coverage:            nextRow.Coverage,
		AcceptedDecisions:   nextRow.AcceptedDecisions,
		Obligations:         nextRow.Obligations,
		Blockers:            nextRow.Blockers,
		NextPermittedAction: nextRow.NextPermittedAction,
		Evidence:            nextRow.Evidence,
		ResolvedThreads:     nextRow.ResolvedThreads,
		BuiltAt:             pgtype.Timestamptz{Time: nextRow.BuiltAt, Valid: true},
	}); err != nil {
		slog.Warn("checkpoint: persist next checkpoint failed",
			"issue_id", uuidToString(issue.ID), "agent_id", uuidToString(agentID), "error", err)
	}

	return block
}

// scanRootCoverageForIssue runs the same roots-only comment scan
// `comment list --roots-only --summary` uses (fetchCommentsForList /
// ListRootCommentsForIssue) and projects it into the
// (root id, reply_count, last_activity_at, resolved) tuple a checkpoint
// diffs against. No new traversal — this is the reuse point CHE-489
// documented.
//
// truncated mirrors fetchCommentsResult.CommentsTruncated: true when
// commentHardCap dropped the oldest root(s) from this read. The caller must
// not persist a checkpoint built from a truncated scan — see the guard in
// loadIssueCheckpointBlock.
func (h *Handler) scanRootCoverageForIssue(ctx context.Context, issue db.Issue) (live []checkpoint.ThreadCoverage, truncated bool, err error) {
	result, err := h.fetchCommentsForList(ctx, fetchCommentsArgs{
		Issue:     issue,
		RootsOnly: true,
	})
	if err != nil {
		return nil, false, err
	}
	live = make([]checkpoint.ThreadCoverage, 0, len(result.Comments))
	for _, c := range result.Comments {
		stat := result.RootStats[uuidToString(c.ID)]
		live = append(live, checkpoint.ThreadCoverage{
			ThreadRootID:   uuidToString(c.ID),
			ReplyCount:     stat.ReplyCount,
			LastActivityAt: stat.LastActivityAt.Time,
			Resolved:       c.ResolvedAt.Valid,
		})
	}
	return live, result.CommentsTruncated, nil
}

// rowFromDB converts a sqlc-generated IssueCheckpoint row into the
// checkpoint package's storage-agnostic Row, keeping pgtype/db imports out
// of the checkpoint package itself.
func rowFromDB(r db.IssueCheckpoint) checkpoint.Row {
	builtAt := r.BuiltAt.Time
	if !r.BuiltAt.Valid {
		builtAt = time.Time{}
	}
	return checkpoint.Row{
		IssueRevision:       r.IssueRevision,
		CandidateID:         r.CandidateID,
		Coverage:            r.Coverage,
		AcceptedDecisions:   r.AcceptedDecisions,
		Obligations:         r.Obligations,
		Blockers:            r.Blockers,
		NextPermittedAction: r.NextPermittedAction,
		Evidence:            r.Evidence,
		ResolvedThreads:     r.ResolvedThreads,
		BuiltAt:             builtAt,
	}
}
