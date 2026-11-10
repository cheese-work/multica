package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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
func prStateFingerprint(ctx context.Context, db rowQuerier, issueID pgtype.UUID) (string, json.RawMessage, string, error) {
	var fp, commentFingerprint string
	var prStates json.RawMessage
	err := db.QueryRow(ctx, `WITH linked_prs AS (
			SELECT string_agg(jsonb_build_array(p.id, p.state, p.head_sha, p.mergeable_state, p.api_mergeable, p.api_merge_state_status, p.checks_rollup_state, p.review_state, p.review_head_sha, p.review_id, p.review_at)::text, ';' ORDER BY p.id) AS value,
				jsonb_agg(jsonb_build_object('repo_owner', p.repo_owner, 'repo_name', p.repo_name, 'pr_number', p.pr_number, 'head_sha', p.head_sha, 'checks_rollup_state', p.checks_rollup_state, 'review_state', p.review_state, 'review_head_sha', p.review_head_sha) ORDER BY p.id) AS snapshot
			FROM issue_pull_request ip JOIN github_pull_request p ON p.id = ip.pull_request_id WHERE ip.issue_id = $1
		), issue_comments AS (
			SELECT md5(string_agg(jsonb_build_array(c.id, c.revision, md5(c.content), c.deleted_at, c.resolved_at)::text, ';' ORDER BY c.id)) AS value
			FROM comment c WHERE c.issue_id = $1
		)
		SELECT CASE WHEN linked_prs.value IS NULL THEN '' ELSE md5(linked_prs.value || '|' || COALESCE(issue_comments.value, '')) END,
			COALESCE(linked_prs.snapshot, '[]'::jsonb), COALESCE(issue_comments.value, '')
		FROM linked_prs CROSS JOIN issue_comments`, issueID).Scan(&fp, &prStates, &commentFingerprint)
	return fp, prStates, commentFingerprint, err
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

// holdsDraftReturn reports whether any pending fact is a returned-to-draft event.
func holdsDraftReturn(receipts []db.IssueWakeupReceipt) bool {
	for _, r := range receipts {
		var p struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(r.Payload, &p) == nil && p.Reason == PRAttentionDraft {
			return true
		}
	}
	return false
}

func skippedUnchangedDetails(rule string, lastTask pgtype.UUID) map[string]any {
	return map[string]any{"rule": rule, "reason": wakeupOutcomeUnchanged, "last_task_id": util.UUIDToString(lastTask)}
}
