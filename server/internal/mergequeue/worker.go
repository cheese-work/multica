package mergequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repo names a repository of one workspace.
type Repo struct {
	WS          uuid.UUID
	Owner, Name string
}

// Timeline item kinds the worker reads.
const (
	ItemRemoval   = "removal"
	ItemCommit    = "commit"
	ItemForcePush = "force_push"
)

// TimelineItem is one merge-queue removal, PR commit or force-push of a PR.
type TimelineItem struct {
	Kind      string
	NodeID    string
	BeforeOID string // removal only: the merge-group commit
	Reason    string // removal only
	At        time.Time
}

// BuildRun is a merge_group run.
type BuildRun struct {
	ID          int64
	Attempt     int32
	Completed   bool
	Conclusion  string
	CreatedAt   time.Time
	CompletedAt time.Time
}

// FailedCheck is one check that failed or did not finish.
type FailedCheck struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
}

// Collected is the evidence read from a failed queue build.
type Collected struct {
	Signature string
	Failing   []TestID
	Checks    []FailedCheck
}

// Source reads GitHub. The server never writes to it.
type Source interface {
	// Timeline returns the PR's removals, commits and force-pushes, oldest first.
	Timeline(ctx context.Context, r Repo, pr int) ([]TimelineItem, error)
	// QueueRuns returns the merge_group runs of a head SHA.
	QueueRuns(ctx context.Context, r Repo, headSHA string) ([]BuildRun, error)
}

// Evidencer collects a failed queue build's evidence.
type Evidencer interface {
	QueueBuild(ctx context.Context, r Repo, run BuildRun) (Collected, error)
}

// Intake statuses and the waits that end a pending row.
const (
	StatusPending      = "pending"
	StatusProcessed    = "processed"
	StatusStale        = "stale"
	StatusInconclusive = "inconclusive"

	// NoBuildTimeout is how long a removal waits for its queue build (plan 4.1, rule 4).
	NoBuildTimeout = 45 * time.Minute
	// noRemovalGrace is how long a dequeued delivery waits for GitHub's timeline to show its removal.
	noRemovalGrace = 5 * time.Minute
	sweepLimit     = 50
)

// Worker binds pending intake rows to their queue builds. Reads go through
// Src, so the server writes nothing to GitHub.
type Worker struct {
	Pool *pgxpool.Pool
	Src  Source
	Ev   Evidencer
	Now  func() time.Time
	Log  *slog.Logger
}

type intakeRow struct {
	ID        uuid.UUID
	WS        uuid.UUID
	Owner     string
	Repo      string
	PR        int
	Node      string
	Before    string
	CreatedAt time.Time
}

func (r intakeRow) repo() Repo { return Repo{WS: r.WS, Owner: r.Owner, Name: r.Repo} }

const intakeCols = `id, workspace_id, repo_owner, repo_name, pr_number, coalesce(removal_node_id, ''), coalesce(before_commit_oid, ''), created_at`

func scanIntake(row pgx.Row) (r intakeRow, err error) {
	err = row.Scan(&r.ID, &r.WS, &r.Owner, &r.Repo, &r.PR, &r.Node, &r.Before, &r.CreatedAt)
	return r, err
}

// Sweep processes the pending intake rows, oldest first, and reports how many
// it settled. A row it cannot settle yet stays pending for the next sweep; the
// first error of a row does not stop the others.
func (w *Worker) Sweep(ctx context.Context) (int, error) {
	rows, err := w.Pool.Query(ctx, `SELECT `+intakeCols+` FROM merge_queue_intake WHERE status = 'pending' ORDER BY created_at LIMIT $1`, sweepLimit)
	if err != nil {
		return 0, err
	}
	var pending []intakeRow
	for rows.Next() {
		r, err := scanIntake(rows)
		if err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	type prKey struct {
		Repo
		PR int
	}
	timelines := map[prKey][]TimelineItem{} // one GitHub read per PR per sweep
	var errs []error
	settled := 0
	for _, r := range pending {
		k := prKey{r.repo(), r.PR}
		items, ok := timelines[k]
		if !ok {
			if items, err = w.Src.Timeline(ctx, r.repo(), r.PR); err != nil {
				errs = append(errs, fmt.Errorf("intake %s: timeline: %w", r.ID, err))
				continue
			}
			timelines[k] = items
		}
		done, err := w.process(ctx, r, items)
		if err != nil {
			errs = append(errs, fmt.Errorf("intake %s: %w", r.ID, err))
			continue
		}
		if done {
			settled++
		}
	}
	return settled, errors.Join(errs...)
}

func (w *Worker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// process settles one pending row from the PR's timeline. It reports whether
// the row left pending.
func (w *Worker) process(ctx context.Context, row intakeRow, items []TimelineItem) (bool, error) {
	idx := -1 // position of this row's removal in items
	if row.Node == "" {
		for i, it := range items {
			if it.Kind == ItemRemoval {
				idx = i // the newest removal wins
			}
		}
		if idx < 0 {
			if w.Now().Sub(row.CreatedAt) < noRemovalGrace {
				return false, nil
			}
			return w.settle(ctx, row.ID, StatusStale, nil, nil)
		}
	} else {
		for i, it := range items {
			if it.NodeID == row.Node {
				idx = i
			}
		}
		if idx < 0 { // older than the 100 items read: a newer removal followed
			return w.settle(ctx, row.ID, StatusStale, nil, nil)
		}
	}
	rem := items[idx]
	row, err := w.bind(ctx, row, rem)
	if err != nil {
		return false, err
	}
	if row.ID == uuid.Nil { // attached to a row that is already settled
		return true, nil
	}
	if idx < len(items)-1 {
		// A later removal, push or force-push means this removal's build no
		// longer describes the PR (plan 4.1): stale evidence is never used.
		return w.settle(ctx, row.ID, StatusStale, nil, nil)
	}
	if rem.Reason != "CI_FAILURE" && rem.Reason != "CI_TIMEOUT" {
		return w.settle(ctx, row.ID, StatusProcessed, nil, nil) // the attribution stage reads the reason
	}
	runs, err := w.Src.QueueRuns(ctx, row.repo(), rem.BeforeOID)
	if err != nil {
		return false, fmt.Errorf("queue runs: %w", err)
	}
	var run *BuildRun
	for i := range runs {
		if runs[i].Completed && (run == nil || runs[i].ID > run.ID || (runs[i].ID == run.ID && runs[i].Attempt > run.Attempt)) {
			run = &runs[i]
		}
	}
	if run == nil {
		if w.Now().Sub(rem.At) >= NoBuildTimeout {
			return w.settle(ctx, row.ID, StatusInconclusive, nil, nil)
		}
		return false, nil
	}
	if run.Conclusion == "success" {
		return w.settle(ctx, row.ID, StatusProcessed, run, nil)
	}
	col, err := w.Ev.QueueBuild(ctx, row.repo(), *run)
	if err != nil {
		return false, fmt.Errorf("collect queue build %d: %w", run.ID, err)
	}
	return w.settle(ctx, row.ID, StatusProcessed, run, &col)
}

// bind ties the row to its removal node under the repo lock. When the record
// already stored a row for that node, the delivery attaches to it: the
// delivery's row is removed, its delivery id moves over, and the stored row is
// returned (uuid.Nil when that row is already settled), so the removal has one
// row and one attribution.
func (w *Worker) bind(ctx context.Context, row intakeRow, rem TimelineItem) (intakeRow, error) {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return row, err
	}
	defer tx.Rollback(ctx)
	if err := LockRepo(ctx, tx, row.WS, row.Owner, row.Repo); err != nil {
		return row, err
	}
	target := row.ID
	if row.Node == "" {
		var existing uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM merge_queue_intake WHERE removal_node_id = $1`, rem.NodeID).Scan(&existing)
		switch {
		case err == nil:
			target = existing
			if _, err := tx.Exec(ctx, `
				UPDATE merge_queue_intake t SET delivery_id = coalesce(t.delivery_id, d.delivery_id), updated_at = now()
				FROM merge_queue_intake d WHERE t.id = $1 AND d.id = $2`, existing, row.ID); err != nil {
				return row, err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM merge_queue_intake WHERE id = $1 AND status = 'pending' AND removal_node_id IS NULL`, row.ID); err != nil {
				return row, err
			}
		case errors.Is(err, pgx.ErrNoRows):
			if _, err := tx.Exec(ctx, `UPDATE merge_queue_intake SET removal_node_id = $2, updated_at = now() WHERE id = $1 AND removal_node_id IS NULL`, row.ID, rem.NodeID); err != nil {
				return row, err
			}
		default:
			return row, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE merge_queue_intake
		SET before_commit_oid = coalesce(before_commit_oid, NULLIF($2, '')),
		    removed_at = coalesce(removed_at, $3),
		    reason = coalesce(reason, NULLIF($4, ''))
		WHERE id = $1`, target, rem.BeforeOID, rem.At, rem.Reason); err != nil {
		return row, err
	}
	out, err := scanIntake(tx.QueryRow(ctx, `SELECT `+intakeCols+` FROM merge_queue_intake WHERE id = $1 AND status = 'pending'`, target))
	if errors.Is(err, pgx.ErrNoRows) {
		return intakeRow{}, tx.Commit(ctx)
	}
	if err != nil {
		return row, err
	}
	return out, tx.Commit(ctx)
}

// settle moves a pending row to a final status, with its evidence, under the
// repo lock. It reports false when another worker settled the row first. A
// removal that a newer removal of the same PR follows is stale whatever the
// caller asked for, so an older removal never writes evidence over a newer one.
func (w *Worker) settle(ctx context.Context, id uuid.UUID, status string, run *BuildRun, col *Collected) (bool, error) {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var ws uuid.UUID
	var owner, repo string
	var pr int
	var removedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT workspace_id, repo_owner, repo_name, pr_number, removed_at FROM merge_queue_intake WHERE id = $1`, id).Scan(&ws, &owner, &repo, &pr, &removedAt)
	if err != nil {
		return false, err
	}
	if err := LockRepo(ctx, tx, ws, owner, repo); err != nil {
		return false, err
	}
	var newer bool
	if removedAt != nil {
		err = tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM merge_queue_intake
			WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND pr_number = $4 AND id <> $5 AND removed_at > $6)`,
			ws, owner, repo, pr, id, *removedAt).Scan(&newer)
		if err != nil {
			return false, err
		}
	}
	if newer {
		status, col = StatusStale, nil
	}
	var runID *int64
	if run != nil && status == StatusProcessed {
		runID = &run.ID
	}
	tag, err := tx.Exec(ctx, `UPDATE merge_queue_intake SET status = $2, build_run_id = $3, updated_at = now() WHERE id = $1 AND status = 'pending'`, id, status, runID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if col != nil && run != nil {
		tests, checks := col.Failing, col.Checks
		if tests == nil {
			tests = []TestID{}
		}
		if checks == nil {
			checks = []FailedCheck{}
		}
		tj, _ := json.Marshal(tests)
		cj, _ := json.Marshal(checks)
		_, err = tx.Exec(ctx, `
			INSERT INTO merge_queue_evidence (workspace_id, repo_owner, repo_name, pr_number, kind, intake_id, run_id, run_attempt, signature, failing_tests, checks, run_created_at, run_completed_at)
			VALUES ($1, $2, $3, $4, 'queue_build', $5, $6, $7, NULLIF($8, ''), $9, $10, $11, $12)`,
			ws, owner, repo, pr, id, run.ID, run.Attempt, col.Signature, tj, cj, nullTime(run.CreatedAt), nullTime(run.CompletedAt))
		if err != nil {
			return false, err
		}
	}
	w.log().Info("merge queue intake settled", "intake_id", id, "status", status)
	return true, tx.Commit(ctx)
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
