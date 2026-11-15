package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The repository variables that pause agent merges (CHE-5121).
var mergePauseVariables = []string{"AGENT_MERGE_PAUSED", "MAIN_RED_PAUSED"}

// defaultPauseRefreshInterval bounds GitHub reads: one list call per repository
// per interval, shared by every merge_eligibility rule that cares about it.
const defaultPauseRefreshInterval = time.Minute

// defaultGateVeto is the veto window after the last PASS on a gate-path PR.
const defaultGateVeto = 24 * time.Hour

// PauseVariableReader lists the names of a repository's Actions variables.
type PauseVariableReader interface {
	RepoVariableNames(ctx context.Context, installationID int64, owner, repo string) ([]string, error)
}

var errMergePauseUnknown = errors.New("merge pause state is unknown")

// pauseUnknown wraps the stored read error so the rule's last_error says why.
func pauseUnknown(detail string) error {
	if detail == "" {
		return errMergePauseUnknown
	}
	return fmt.Errorf("%w: %s", errMergePauseUnknown, detail)
}

// RefreshMergePauses is the named owner of the pause state: the scheduler job
// merge_pause_refresh calls it. Per repository some enabled merge_eligibility
// rule watches, it lists the repository variables at most once per interval
// and stores whether each pause variable is set. A failed read stores NULL
// (unknown) and the error, never "not set", so the evaluator refuses.
func (s *IssueWakeupService) RefreshMergePauses(ctx context.Context) error {
	if s.PauseVars == nil || s.Tasks == nil || s.Tasks.TxStarter == nil {
		return nil
	}
	interval := s.PauseRefreshInterval
	if interval <= 0 {
		interval = defaultPauseRefreshInterval
	}
	now := s.now()
	type repo struct {
		installation int64
		owner, name  string
	}
	// Read the stale repositories, then release the transaction before any
	// network call.
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT pr.installation_id, pr.repo_owner, pr.repo_name
		FROM issue_wakeup w
		JOIN issue_pull_request ipr ON ipr.issue_id = w.issue_id
		JOIN github_pull_request pr ON pr.id = ipr.pull_request_id
		LEFT JOIN merge_pause_state ps ON ps.repo_owner = pr.repo_owner AND ps.repo_name = pr.repo_name
			AND ps.variable_name = ANY($1)
		WHERE w.enabled AND w.condition->>'type' = 'merge_eligibility'
		GROUP BY pr.installation_id, pr.repo_owner, pr.repo_name
		HAVING count(ps.variable_name) < $2 OR min(ps.fetched_at) <= $3`,
		mergePauseVariables, len(mergePauseVariables), now.Add(-interval))
	if err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	var repos []repo
	for rows.Next() {
		var r repo
		if err := rows.Scan(&r.installation, &r.owner, &r.name); err != nil {
			rows.Close()
			_ = tx.Rollback(ctx)
			return err
		}
		repos = append(repos, r)
	}
	rows.Close()
	err = rows.Err()
	if rbErr := tx.Rollback(ctx); err == nil {
		err = rbErr
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range repos {
		names, readErr := s.PauseVars.RepoVariableNames(ctx, r.installation, r.owner, r.name)
		var detail *string
		if readErr != nil {
			msg := readErr.Error()
			detail = &msg
			errs = append(errs, fmt.Errorf("%s/%s: %w", r.owner, r.name, readErr))
		}
		if err := s.storePauseState(ctx, r.owner, r.name, names, detail, now); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

// storePauseState writes one row per pause variable: is_set is NULL when the
// read failed (detail non-nil).
func (s *IssueWakeupService) storePauseState(ctx context.Context, owner, repo string, names []string, detail *string, now time.Time) error {
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	for _, v := range mergePauseVariables {
		var isSet *bool
		if detail == nil {
			b := slices.Contains(names, v)
			isSet = &b
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO merge_pause_state(repo_owner,repo_name,variable_name,is_set,fetched_at,error)
			VALUES($1,$2,$3,$4,$5,$6)
			ON CONFLICT (repo_owner,repo_name,variable_name)
			DO UPDATE SET is_set=EXCLUDED.is_set, fetched_at=EXCLUDED.fetched_at, error=EXCLUDED.error`,
			owner, repo, v, isSet, now, detail); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
	}
	return tx.Commit(ctx)
}

// mergePaused reports whether any pause variable is set for the repository.
// Unknown (never read, failed read, or older than maxAge) is an error, not false.
func mergePaused(ctx context.Context, tx pgx.Tx, owner, repo string, now time.Time, maxAge time.Duration) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT variable_name, is_set, fetched_at, COALESCE(error,'')
		FROM merge_pause_state WHERE repo_owner=$1 AND repo_name=$2 AND variable_name = ANY($3)`,
		owner, repo, mergePauseVariables)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	seen, paused := 0, false
	var unknown error
	for rows.Next() {
		var name, detail string
		var isSet *bool
		var at pgtype.Timestamptz
		if err := rows.Scan(&name, &isSet, &at, &detail); err != nil {
			return false, err
		}
		seen++
		if age := now.Sub(at.Time); isSet == nil || !at.Valid || age < 0 || age > maxAge {
			unknown = pauseUnknown(detail)
		} else if *isSet {
			paused = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if seen < len(mergePauseVariables) && unknown == nil {
		unknown = errMergePauseUnknown
	}
	// A set variable holds regardless of what else is unknown.
	if paused {
		return true, nil
	}
	return false, unknown
}

// mergeVetoElapsed persists last PASS + window per PR and reports whether the
// window has run out. A newer PASS moves the deadline; an older one never does.
func mergeVetoElapsed(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, owner, repo string, number int32, lastPass, now time.Time, window time.Duration) (bool, error) {
	var due time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO merge_veto_deadline(workspace_id,repo_owner,repo_name,pr_number,last_pass_at,due_at)
		VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT (workspace_id,repo_owner,repo_name,pr_number) DO UPDATE
		SET last_pass_at = GREATEST(merge_veto_deadline.last_pass_at, EXCLUDED.last_pass_at),
		    due_at = GREATEST(merge_veto_deadline.last_pass_at, EXCLUDED.last_pass_at) + $7 * interval '1 second'
		RETURNING due_at`,
		workspaceID, owner, repo, number, lastPass, lastPass.Add(window), window.Seconds()).Scan(&due)
	return err == nil && !now.Before(due), err
}
