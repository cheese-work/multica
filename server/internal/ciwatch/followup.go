package ciwatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EscalationReopenAfter: a signature closed inside this window is attached to,
// not reopened, by an escalation.
const EscalationReopenAfter = 24 * time.Hour

func escalationKind(protocol string) string {
	if protocol == "P4" {
		return ActionInboxNotify
	}
	return ActionIssueCreate
}

// escalate schedules the one P2/P4 action of an original run, in tx when given.
// P2 and P4 share the run, so any existing escalation (either kind, from any
// source) means there is nothing to add: inserted=false. A P2 claims the
// signature owner in the same tx; an owner already held also means inserted=false.
func (l *Ledger) escalate(ctx context.Context, tx pgx.Tx, n NewAction) (Action, bool, error) {
	own := tx == nil
	if own {
		var err error
		if tx, err = l.pool.Begin(ctx); err != nil {
			return Action{}, false, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
	}
	// Two writers of different kinds must not both pass the NOT EXISTS below.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("ci-escalate/%s/%s/%s/%d", n.WorkspaceID, n.RepoOwner+"/"+n.RepoName, "run", n.OriginalRunID)); err != nil {
		return Action{}, false, err
	}
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ci_watch_action WHERE workspace_id=$1::uuid AND repo_owner=$2
			AND repo_name=$3 AND original_run_id=$4 AND action_kind IN ('inbox_notify','issue_create'))`,
		n.WorkspaceID, n.RepoOwner, n.RepoName, n.OriginalRunID).Scan(&found); err != nil || found {
		return Action{}, false, err
	}
	// RunOne cancels an issue_create that does not hold the signature owner, so
	// claim it here, in the same tx as the insert. An owner already held means
	// an issue exists to attach to.
	if n.Kind == ActionIssueCreate && n.SignatureHash != "" {
		owned, err := l.claimOwner(ctx, tx, n.WorkspaceID, n.RepoOwner, n.RepoName, n.SignatureHash, EscalationReopenAfter)
		if err != nil || !owned {
			return Action{}, false, err
		}
	}
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO ci_watch_action (workspace_id, repo_owner, repo_name, original_run_id,
			action_kind, protocol, signature_hash, due_at, run_completed_at, created_at, updated_at)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		RETURNING id::text`,
		n.WorkspaceID, n.RepoOwner, n.RepoName, n.OriginalRunID, n.Kind, n.Protocol, n.SignatureHash,
		n.DueAt, n.RunCompletedAt, l.now()).Scan(&id)
	if err != nil {
		return Action{}, false, err
	}
	a, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionCols+` FROM ci_watch_action WHERE id=$1::uuid`, id))
	if err == nil && own {
		err = tx.Commit(ctx)
	}
	return a, err == nil, err
}

// RerunFollowup routes one rerun row whose POST did not succeed, once. It
// never POSTs. The side effects (Shadow switch, P2/P4 action) commit in one
// transaction with the followup_at marker, so a crash leaves either all or
// none, and the next call finishes the job. An existing P2/P4 action for the
// run does not stop the Shadow switch, and neither does a reconcile that ended
// in done: the permission problem stays.
//
//   - permission: refused for lack of permission; route now, in any state.
//   - rate_limit: refused; route now (ambiguous or failed).
//   - server, rejected, no_answer: wait until read-only reconcile has finished
//     and found no rerun (failed/reconcile_not_found). A rerun that landed is
//     done and escalates nothing.
//   - "" (answer never stored): once reconcile ended the row (no rerun found,
//     or done by reconcile because the attempt advanced) it is treated as a
//     possible permission denial: Shadow switch and P4. A 2xx done row is
//     never reconciled, so it is never routed.
//
// Returns true when it handled a row.
func (l *Ledger) RerunFollowup(ctx context.Context) (bool, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionCols+` FROM ci_watch_action a
		WHERE action_kind='rerun' AND post_started_at IS NOT NULL AND followup_at IS NULL AND (
			(post_outcome='permission' AND state IN ('ambiguous','failed','done'))
			OR (post_outcome='rate_limit' AND state IN ('ambiguous','failed'))
			OR (state='failed' AND reason=$1 AND post_outcome IN ('','server','rejected','no_answer'))
			OR (state='done' AND reason=$2 AND post_outcome=''))
		ORDER BY updated_at LIMIT 1 FOR UPDATE OF a SKIP LOCKED`, ReasonReconcileNotFound, ReasonReconciled))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := l.now()
	protocol := "P2"
	switch a.PostOutcome {
	case OutcomePermission, "":
		protocol = "P4"
		// p1_mode of the repo goes back to shadow, with an audit row.
		if _, err = tx.Exec(ctx, `WITH old AS (
				SELECT id, p1_mode FROM ci_watch_config
				WHERE workspace_id=$1::uuid AND repo_owner=lower($2) AND repo_name=lower($3) AND p1_mode<>'shadow' FOR UPDATE),
			upd AS (UPDATE ci_watch_config c SET p1_mode='shadow', updated_at=$4 FROM old WHERE c.id=old.id RETURNING c.id)
			INSERT INTO ci_watch_config_audit (workspace_id, config_id, actor_type, actor_id, field, old_value, new_value, changed_at)
			SELECT $1::uuid, old.id, 'system', '00000000-0000-0000-0000-000000000000', 'p1_mode', old.p1_mode, 'shadow', $4 FROM old`,
			a.WorkspaceID, a.RepoOwner, a.RepoName, now); err != nil {
			return true, err
		}
	case OutcomeRateLimit:
		protocol = "P4"
	}
	n := NewAction{WorkspaceID: a.WorkspaceID, RepoOwner: a.RepoOwner, RepoName: a.RepoName,
		OriginalRunID: a.OriginalRunID, Kind: escalationKind(protocol), Protocol: protocol,
		SignatureHash: a.SignatureHash, DueAt: now, RunCompletedAt: a.RunCompletedAt}
	if _, _, err = l.escalate(ctx, tx, n); err != nil {
		return true, err
	}
	if _, err = tx.Exec(ctx, `UPDATE ci_watch_action SET followup_at=$2 WHERE id=$1::uuid`, a.ID, now); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}
