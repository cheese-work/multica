package mergequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DB is the part of a pool or transaction the intake writers need. The repo
// lock is a transaction lock, so callers that need it pass a transaction.
type DB interface {
	Execer
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// IntakeInput names one PR and the delivery or removal that creates its row.
// Owner and Repo are stored lower-cased.
type IntakeInput struct {
	WS         uuid.UUID
	Owner      string
	Repo       string
	PR         int
	DeliveryID string
	Routing    string // the workspace value at the intake commit: off, warn or block
}

func (in IntakeInput) owner() string { return strings.ToLower(in.Owner) }
func (in IntakeInput) repo() string  { return strings.ToLower(in.Repo) }

// LockRepo takes the repo-level transaction lock every merge-queue write uses.
func LockRepo(ctx context.Context, db DB, ws uuid.UUID, owner, repo string) error {
	key := "merge-queue:" + ws.String() + "/" + strings.ToLower(owner) + "/" + strings.ToLower(repo)
	_, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

// InsertIntake stores the pending intake row of a dequeued delivery. It stores
// nothing with routing off, and nothing for a delivery id it already stored.
// Call it in a transaction: the repo lock lasts until the transaction ends.
func InsertIntake(ctx context.Context, db DB, in IntakeInput) (id uuid.UUID, created bool, err error) {
	if in.Routing == "off" {
		return uuid.Nil, false, nil
	}
	if err := LockRepo(ctx, db, in.WS, in.Owner, in.Repo); err != nil {
		return uuid.Nil, false, err
	}
	err = db.QueryRow(ctx, `
		INSERT INTO merge_queue_intake (workspace_id, repo_owner, repo_name, pr_number, delivery_id, routing)
		SELECT $1, $2, $3, $4, NULLIF($5::text, ''), $6
		WHERE $5::text = '' OR NOT EXISTS (
			SELECT 1 FROM merge_queue_intake
			WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND delivery_id = $5::text)
		RETURNING id`, in.WS, in.owner(), in.repo(), in.PR, in.DeliveryID, in.Routing).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}

// EnsureIntakeForRemoval stores the pending row of a removal timeline node the
// record read, unless a row for that node exists. Either way it returns the
// row's id.
func EnsureIntakeForRemoval(ctx context.Context, db DB, in IntakeInput, nodeID string, removedAt time.Time) (id uuid.UUID, created bool, err error) {
	if nodeID == "" {
		return uuid.Nil, false, errors.New("mergequeue: a removal needs its timeline node id")
	}
	if err := LockRepo(ctx, db, in.WS, in.Owner, in.Repo); err != nil {
		return uuid.Nil, false, err
	}
	err = db.QueryRow(ctx, `
		INSERT INTO merge_queue_intake (workspace_id, repo_owner, repo_name, pr_number, removal_node_id, delivery_id, routing, removed_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6::text, ''), $7, $8)
		ON CONFLICT (removal_node_id) WHERE removal_node_id IS NOT NULL DO NOTHING
		RETURNING id`, in.WS, in.owner(), in.repo(), in.PR, nodeID, in.DeliveryID, in.Routing, removedAt).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, err
	}
	err = db.QueryRow(ctx, `SELECT id FROM merge_queue_intake WHERE removal_node_id = $1`, nodeID).Scan(&id)
	return id, false, err
}

// Queue states of a PR.
const (
	QueueEnqueued = "enqueued"
	QueueDequeued = "dequeued"
	QueueClosed   = "closed"
)

// SetQueueState records a PR's queue state under the repo lock. A change older
// than the stored one is ignored, so a late delivery never moves a PR back.
func SetQueueState(ctx context.Context, db DB, ws uuid.UUID, owner, repo string, pr int, state string, changedAt time.Time) error {
	switch state {
	case QueueEnqueued, QueueDequeued, QueueClosed:
	default:
		return fmt.Errorf("mergequeue: unknown queue state %q", state)
	}
	if err := LockRepo(ctx, db, ws, owner, repo); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `
		INSERT INTO merge_queue_state (workspace_id, repo_owner, repo_name, pr_number, state, changed_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (workspace_id, repo_owner, repo_name, pr_number)
		DO UPDATE SET state = EXCLUDED.state, changed_at = EXCLUDED.changed_at
		WHERE merge_queue_state.changed_at <= EXCLUDED.changed_at`,
		ws, strings.ToLower(owner), strings.ToLower(repo), pr, state, changedAt)
	return err
}

// RoutingFromSettings reads the routing value of a workspace settings object.
// A missing, malformed or unknown value is the default, warn.
func RoutingFromSettings(settings []byte) string {
	var s map[string]any
	if json.Unmarshal(settings, &s) == nil {
		switch v, _ := s[RoutingKey].(string); v {
		case "off", "warn", "block":
			return v
		}
	}
	return "warn"
}
