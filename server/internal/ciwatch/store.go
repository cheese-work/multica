package ciwatch

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// EventWriter is the one sqlc query Store needs.
type EventWriter interface {
	InsertCIFailureEvent(ctx context.Context, arg db.InsertCIFailureEventParams) (int64, error)
}

// Store writes ev as a failure event. It reports false when the run attempt
// was already stored (a redelivered event), which is not an error.
func Store(ctx context.Context, w EventWriter, ev *FailureEvent) (bool, error) {
	var ws pgtype.UUID
	if err := ws.Scan(ev.WorkspaceID); err != nil {
		return false, err
	}
	excerpts, err := json.Marshal(append([]JobEvidence{}, ev.Jobs...))
	if err != nil {
		return false, err
	}
	n, err := w.InsertCIFailureEvent(ctx, db.InsertCIFailureEventParams{
		WorkspaceID: ws, RepoOwner: ev.RepoOwner, RepoName: ev.RepoName, RunID: ev.RunID,
		RunAttempt: ev.RunAttempt, WorkflowName: ev.WorkflowName, Event: ev.Event,
		HeadBranch: ev.HeadBranch, HeadSha: ev.HeadSHA, Conclusion: ev.Conclusion,
		SignatureHash: ev.SignatureHash, Evidence: ev.Evidence, PartialReason: ev.PartialReason,
		FailedJobCount: int32(ev.FailedJobCount), Excerpts: excerpts,
	})
	return n == 1, err
}
