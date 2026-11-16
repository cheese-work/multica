package ciwatch

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

// Watcher is the production entry point: the webhook hands it a completed run.
type Watcher struct {
	// FlagOn is read first; when off, no CI table is touched.
	FlagOn   func(ctx context.Context) bool
	Config   ConfigReader
	Pipeline Pipeline
}

// Watch records the run if its repo is watched. An error means the delivery
// should be shown as failed so it is redelivered; the pipeline is idempotent.
func (w *Watcher) Watch(ctx context.Context, workspace pgtype.UUID, run Run, defaultBranch string) error {
	if !w.FlagOn(ctx) {
		return nil
	}
	cfg, found, err := readConfig(ctx, w.Config, workspace, run.RepoOwner, run.RepoName)
	if err != nil || !found {
		return err
	}
	run.WorkspaceID = util.UUIDToString(workspace)
	// History stays empty until the flake counters land in a later slice.
	_, err = w.Pipeline.Process(ctx, Input{FlagOn: true, DefaultBranch: defaultBranch, Config: cfg, Run: run}, History{})
	return err
}
