package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-3590: a wakeup whose watched inputs are the same as when the last run on
// the issue started adds nothing, so it starts no run. Fail open: any doubt
// (no fingerprint, a first run, a run that did not complete, a stale run)
// starts the run.
const (
	wakeupFingerprintKey   = "wakeup_input_fingerprint"
	wakeupOutcomeUnchanged = "unchanged_since_last_run"
	// wakeupSkipWindow bounds how long a run vouches for unchanged inputs, so
	// state the platform does not hold is still looked at a few times a day.
	wakeupSkipWindow = 6 * time.Hour
)

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// prStateFingerprint hashes what a PR watchdog reads: head, mergeable state and
// required-check rollup of every PR linked to the issue. "" when none is linked.
func prStateFingerprint(ctx context.Context, db rowQuerier, issueID pgtype.UUID) (string, error) {
	var fp string
	err := db.QueryRow(ctx, `SELECT COALESCE(md5(string_agg(jsonb_build_array(p.id, p.state, p.head_sha, p.mergeable_state, p.api_mergeable, p.api_merge_state_status, p.checks_rollup_state)::text, ';' ORDER BY p.id)), '')
		FROM issue_pull_request ip JOIN github_pull_request p ON p.id = ip.pull_request_id WHERE ip.issue_id = $1`, issueID).Scan(&fp)
	return fp, err
}

// heartbeatFingerprint adds what else a heartbeat can react to: the issue's
// status and assignee, and comments by anyone but the woken agent. "" when no PR
// is linked, since the rest of the world is then unobservable here.
func heartbeatFingerprint(ctx context.Context, db rowQuerier, issueID, agentID pgtype.UUID) (string, error) {
	pr, err := prStateFingerprint(ctx, db, issueID)
	if err != nil || pr == "" {
		return "", err
	}
	var rest string
	err = db.QueryRow(ctx, `SELECT md5(jsonb_build_array(i.status, i.assignee_id,
			(SELECT count(*) FROM comment c WHERE c.issue_id = i.id AND NOT (c.author_type = 'agent' AND c.author_id = $2)),
			(SELECT max(c.created_at) FROM comment c WHERE c.issue_id = i.id AND NOT (c.author_type = 'agent' AND c.author_id = $2)))::text)
		FROM issue i WHERE i.id = $1`, issueID, agentID).Scan(&rest)
	if err != nil {
		return "", err
	}
	return pr + rest, nil
}

// unchangedSinceLastRun reports whether the newest run on the issue that
// matches (context key = value) completed recently with this same fingerprint.
func unchangedSinceLastRun(ctx context.Context, db rowQuerier, issueID, agentID pgtype.UUID, ctxKey, ctxValue, fingerprint string) (pgtype.UUID, bool, error) {
	if fingerprint == "" {
		return pgtype.UUID{}, false, nil
	}
	var (
		id     pgtype.UUID
		status string
		prev   *string
		fresh  bool
	)
	err := db.QueryRow(ctx, `SELECT id, status, context->>'`+wakeupFingerprintKey+`', created_at > now() - make_interval(secs => $5)
		FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND context->>$3 = $4 ORDER BY created_at DESC LIMIT 1`,
		issueID, agentID, ctxKey, ctxValue, wakeupSkipWindow.Seconds()).Scan(&id, &status, &prev, &fresh)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, false, nil
	}
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	return id, status == "completed" && fresh && prev != nil && *prev == fingerprint, nil
}

func skippedUnchangedDetails(rule string, lastTask pgtype.UUID) map[string]any {
	return map[string]any{"rule": rule, "reason": wakeupOutcomeUnchanged, "last_task_id": util.UUIDToString(lastTask)}
}
