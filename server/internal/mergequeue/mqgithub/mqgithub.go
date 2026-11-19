// Package mqgithub connects the merge-queue worker to GitHub (read only) and to
// the CI watcher's budgeted log collector.
package mqgithub

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/ciwatch"
	"github.com/multica-ai/multica/server/internal/ghbudget"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/mergequeue"
)

// Adapter implements mergequeue.Source and mergequeue.Evidencer. Evidence goes
// through the ciwatch collector, so every GitHub call of it spends the
// installation's ghbudget ledger.
type Adapter struct {
	Pool   *pgxpool.Pool
	Client *ghsnapshot.Client
	Coll   *ciwatch.Collector
}

// New builds the adapter for an App client; nil when the client is off.
func New(pool *pgxpool.Pool, client *ghsnapshot.Client) *Adapter {
	if client == nil || !client.Enabled() {
		return nil
	}
	ledger := ghbudget.New(pool, ghbudget.FetcherFunc(client.RateLimit))
	return &Adapter{Pool: pool, Client: client, Coll: &ciwatch.Collector{
		GH: ciwatch.AppGitHub{C: client}, Budget: ciwatch.LedgerBudget{L: ledger},
	}}
}

// installation is the App installation that mirrors the repo for the workspace.
func (a *Adapter) installation(ctx context.Context, r mergequeue.Repo) (int64, error) {
	var id int64
	err := a.Pool.QueryRow(ctx, `SELECT installation_id FROM github_pull_request
		WHERE workspace_id = $1 AND lower(repo_owner) = lower($2) AND lower(repo_name) = lower($3)
		ORDER BY updated_at DESC LIMIT 1`, r.WS, r.Owner, r.Name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("mqgithub: no installation for %s/%s", r.Owner, r.Name)
	}
	return id, err
}

// Timeline implements mergequeue.Source.
func (a *Adapter) Timeline(ctx context.Context, r mergequeue.Repo, pr int) ([]mergequeue.TimelineItem, error) {
	inst, err := a.installation(ctx, r)
	if err != nil {
		return nil, err
	}
	raw, err := a.Client.PRQueueTimeline(ctx, inst, r.Owner, r.Name, pr)
	if err != nil {
		return nil, err
	}
	items := make([]mergequeue.TimelineItem, 0, len(raw))
	for _, it := range raw {
		k := mergequeue.ItemCommit
		switch it.Type {
		case "RemovedFromMergeQueueEvent":
			k = mergequeue.ItemRemoval
		case "HeadRefForcePushedEvent":
			k = mergequeue.ItemForcePush
		}
		items = append(items, mergequeue.TimelineItem{Kind: k, NodeID: it.NodeID, BeforeOID: it.BeforeOID, Reason: it.Reason, At: it.At})
	}
	return items, nil
}

// QueueRuns implements mergequeue.Source.
func (a *Adapter) QueueRuns(ctx context.Context, r mergequeue.Repo, headSHA string) ([]mergequeue.BuildRun, error) {
	inst, err := a.installation(ctx, r)
	if err != nil {
		return nil, err
	}
	raw, err := a.Client.ListMergeGroupRuns(ctx, inst, r.Owner, r.Name, headSHA)
	if err != nil {
		return nil, err
	}
	runs := make([]mergequeue.BuildRun, 0, len(raw))
	for _, x := range raw {
		done := x.Status == "completed"
		b := mergequeue.BuildRun{ID: x.ID, Attempt: x.Attempt, Completed: done, Conclusion: x.Conclusion, CreatedAt: x.CreatedAt}
		if done {
			b.CompletedAt = x.UpdatedAt
		}
		runs = append(runs, b)
	}
	return runs, nil
}

// QueueBuild implements mergequeue.Evidencer.
func (a *Adapter) QueueBuild(ctx context.Context, r mergequeue.Repo, run mergequeue.BuildRun) (mergequeue.Collected, error) {
	inst, err := a.installation(ctx, r)
	if err != nil {
		return mergequeue.Collected{}, err
	}
	return a.Coll.QueueBuild(ctx, ciwatch.Run{
		WorkspaceID: r.WS.String(), InstallationID: inst, RepoOwner: r.Owner, RepoName: r.Name,
		RunID: run.ID, RunAttempt: run.Attempt, Event: "merge_group", Conclusion: run.Conclusion,
	})
}
