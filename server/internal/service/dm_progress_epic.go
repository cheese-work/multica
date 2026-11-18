package service

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// EpicState is one evaluation of the epic-done predicate (CHE-3473 M13).
type EpicState struct {
	Done      bool  // the predicate holds now
	Fire      bool  // this call is the first to see it hold in this episode
	Revision  int32 // the completion episode the answer belongs to
	Cancelled int64 // tracked descendants in a closed category, reported apart
	Tracked   int64 // tracked descendants of the root, whatever their status
}

// EvaluateEpicDone answers whether root's epic is done in lane: the root is in
// a done category, it has tracked descendants and every one is terminal (done or closed
// category; closed ones are counted apart). Lifecycle category decides, never
// a status name; an unresolvable status counts as unfinished. The first call
// that sees the predicate hold in an episode returns Fire once; a reopen or a
// new unfinished tracked child ends the episode in the writer's transaction
// (migration 999p), so the next completion is a new revision. Run it on the
// transaction that will act on the answer: it takes the lane lock. This is the
// authoritative snapshot the epic-done summary validates before any model call.
func EvaluateEpicDone(ctx context.Context, q *db.Queries, lane, root pgtype.UUID) (EpicState, error) {
	if _, err := q.LockDMProgressLane(ctx, lane); err != nil {
		return EpicState{}, err
	}
	rootIssue, err := q.GetIssue(ctx, root)
	if err != nil {
		return EpicState{}, err
	}
	episode, err := q.EnsureDMProgressEpicEpisode(ctx, db.EnsureDMProgressEpicEpisodeParams{LineageID: lane, RootIssueID: root, WorkspaceID: rootIssue.WorkspaceID})
	if err != nil {
		return EpicState{}, err
	}
	state := EpicState{Revision: episode.Revision}
	rootCategory, _ := issuestatus.CategoryWithError(ctx, q, rootIssue.WorkspaceID, rootIssue.Status)
	groups, err := q.CountDMProgressDescendantsByStatus(ctx, db.CountDMProgressDescendantsByStatusParams{LineageID: lane, ReportingRootID: root})
	if err != nil {
		return EpicState{}, err
	}
	unfinished := false
	for _, g := range groups {
		state.Tracked += g.Total
		switch category, _ := issuestatus.CategoryWithError(ctx, q, rootIssue.WorkspaceID, g.Status); category {
		case issuestatus.CategoryDone:
		case issuestatus.CategoryClosed:
			state.Cancelled += g.Total
		default:
			unfinished = true
		}
	}
	// A root with no tracked descendant is a single issue, not an epic.
	state.Done = rootCategory == issuestatus.CategoryDone && state.Tracked > 0 && !unfinished
	switch {
	case state.Done && !episode.FiredAt.Valid:
		n, err := q.FireDMProgressEpicEpisode(ctx, db.FireDMProgressEpicEpisodeParams{LineageID: lane, RootIssueID: root})
		state.Fire = n == 1
		return state, err
	case !state.Done && episode.FiredAt.Valid:
		// The triggers end a fired episode when the tree reopens; this catches
		// a write that skipped them.
		if _, err := q.InvalidateDMProgressEpicEpisode(ctx, db.InvalidateDMProgressEpicEpisodeParams{LineageID: lane, RootIssueID: root}); err != nil {
			return state, err
		}
		state.Revision++
	}
	return state, nil
}
