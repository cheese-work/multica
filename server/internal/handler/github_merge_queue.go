package handler

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/mergequeue"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type deliveryIDKey struct{}

// withDeliveryID carries the X-GitHub-Delivery header to the writers below.
func withDeliveryID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, deliveryIDKey{}, id)
}

// recordMergeQueue stores what a merge-queue pull_request delivery changes, in
// the webhook's transaction (plan 4.3): the PR's queue state for `enqueued`,
// `dequeued` and `closed`, and for `dequeued` a pending intake row that carries
// the workspace's routing at this commit. The worker, not the webhook, reads
// GitHub. The `closed` close row is B13's.
func recordMergeQueue(ctx context.Context, tx pgx.Tx, ws db.Workspace, p *ghPullRequestPayload) error {
	var state string
	switch p.Action {
	case "enqueued":
		state = mergequeue.QueueEnqueued
	case "dequeued":
		state = mergequeue.QueueDequeued
	case "closed":
		state = mergequeue.QueueClosed
	default:
		return nil
	}
	changedAt := parseGHTime(p.PullRequest.UpdatedAt).Time
	if changedAt.IsZero() {
		changedAt = time.Now().UTC()
	}
	owner, repo, pr := p.Repository.Owner.Login, p.Repository.Name, p.PullRequest.Number
	if err := mergequeue.SetQueueState(ctx, tx, uuid.UUID(ws.ID.Bytes), owner, repo, int(pr), state, changedAt); err != nil {
		return fmt.Errorf("github: merge queue state: %w", err)
	}
	if state != mergequeue.QueueDequeued {
		return nil
	}
	delivery, _ := ctx.Value(deliveryIDKey{}).(string)
	if _, _, err := mergequeue.InsertIntake(ctx, tx, mergequeue.IntakeInput{
		WS: uuid.UUID(ws.ID.Bytes), Owner: owner, Repo: repo, PR: int(pr), DeliveryID: delivery,
		Routing: mergequeue.RoutingFromSettings(ws.Settings),
	}); err != nil {
		return fmt.Errorf("github: merge queue intake: %w", err)
	}
	return nil
}
