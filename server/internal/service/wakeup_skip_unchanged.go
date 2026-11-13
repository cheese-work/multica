package service

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
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

// prStateFingerprint hashes linked PR state and issue comments, ignoring
// unchanged comments from the agent's completed runs for this wakeup.
// It returns "" when no PR is linked.
func prStateFingerprint(ctx context.Context, db rowQuerier, issueID, agentID pgtype.UUID, ctxKey, ctxValue string) (string, json.RawMessage, string, error) {
	var fp, commentFingerprint string
	var prStates json.RawMessage
	err := db.QueryRow(ctx, `WITH linked_prs AS (
			SELECT string_agg(jsonb_build_array(p.id, p.state, p.head_sha, p.mergeable_state, p.api_mergeable, p.api_merge_state_status, p.checks_rollup_state, p.review_state, p.review_head_sha, p.review_id, p.review_at)::text, ';' ORDER BY p.id) AS value,
				jsonb_agg(jsonb_build_object('repo_owner', p.repo_owner, 'repo_name', p.repo_name, 'pr_number', p.pr_number, 'head_sha', p.head_sha, 'checks_rollup_state', p.checks_rollup_state, 'review_state', p.review_state, 'review_head_sha', p.review_head_sha) ORDER BY p.id) AS snapshot
			FROM issue_pull_request ip JOIN github_pull_request p ON p.id = ip.pull_request_id WHERE ip.issue_id = $1
		), own_completed_tasks AS (
			SELECT id, completed_at FROM agent_task_queue
			WHERE issue_id = $1 AND agent_id = $2 AND context->>$3 = $4 AND status = 'completed'
		), issue_comments AS (
			SELECT md5(string_agg(jsonb_build_array(c.id, c.revision, md5(c.content), c.deleted_at, c.resolved_at)::text, ';' ORDER BY c.id)) AS value
			FROM comment c LEFT JOIN own_completed_tasks t ON c.source_task_id = t.id
			WHERE c.issue_id = $1 AND (t.id IS NULL OR c.updated_at > t.completed_at)
		)
		SELECT CASE WHEN linked_prs.value IS NULL THEN '' ELSE md5(linked_prs.value || '|' || COALESCE(issue_comments.value, '')) END,
			COALESCE(linked_prs.snapshot, '[]'::jsonb), COALESCE(issue_comments.value, '')
		FROM linked_prs CROSS JOIN issue_comments`, issueID, agentID, ctxKey, ctxValue).Scan(&fp, &prStates, &commentFingerprint)
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

var (
	heartbeatReadsMergeability = regexp.MustCompile(`mergeable|mergeStateStatus`)
	heartbeatActsOnConflict    = regexp.MustCompile(`CONFLICTING|DIRTY`)
)

// isConflictHeartbeat reports the CHE-2795 PR conflict heartbeat: a repeating
// time rule an agent armed whose text only reads PR mergeability and acts on
// CONFLICTING or DIRTY (the same test as the in-review gate). Such a rule
// watches what prStateFingerprint hashes; any other free-text time rule can
// watch things the platform does not hold, so it always runs.
func isConflictHeartbeat(w db.IssueWakeup) bool {
	return (w.Kind == "every" || w.Kind == "cron") && w.Mode == "continuous" && !w.SystemRule.Valid &&
		!w.DefaultRuleKey.Valid && w.SourceTaskID.Valid &&
		heartbeatReadsMergeability.MatchString(w.Instruction) && heartbeatActsOnConflict.MatchString(w.Instruction)
}

// declaredInputsFingerprint hashes the inputs a wakeup declares it watches.
// Fail open: "" when the list is empty, holds a value it does not know, or an
// input cannot be read completely, so unchangedSinceLastRun runs the wakeup.
func declaredInputsFingerprint(ctx context.Context, db rowQuerier, issueID pgtype.UUID, inputs []string, revision int32, instruction string) (string, error) {
	if len(inputs) == 0 {
		return "", nil
	}
	h := md5.New()
	for _, input := range inputs {
		value, err := declaredInputValue(ctx, db, issueID, input)
		if err != nil || value == nil {
			return "", err
		}
		fmt.Fprintf(h, "%s=%s;", input, *value)
	}
	// An edited rule is not what the last run answered (same suffix as CHE-4984).
	return fmt.Sprintf("%x|%d|%s", h.Sum(nil), revision, instruction), nil
}

// declaredInputValue reads one declared input as text. It returns nil when the
// input is unknown or not readable completely.
func declaredInputValue(ctx context.Context, db rowQuerier, issueID pgtype.UUID, input string) (*string, error) {
	var query string
	args := []any{issueID}
	switch {
	case input == "issue":
		query = `SELECT jsonb_build_array(status, revision, assignee_type, assignee_id)::text FROM issue WHERE id = $1`
	case input == "pr":
		query = `SELECT string_agg(jsonb_build_array(p.id, p.state, p.head_sha, p.mergeable_state, p.api_mergeable, p.api_merge_state_status, p.checks_rollup_state, p.review_state, p.review_head_sha, p.review_id, p.review_at)::text, ';' ORDER BY p.id)
			FROM issue_pull_request ip JOIN github_pull_request p ON p.id = ip.pull_request_id WHERE ip.issue_id = $1`
	case input == "children":
		query = `SELECT COALESCE(string_agg(jsonb_build_array(id, status, revision)::text, ';' ORDER BY id), '') FROM issue WHERE parent_issue_id = $1`
	case strings.HasPrefix(input, "thread:"):
		root, err := util.ParseUUID(strings.TrimPrefix(input, "thread:"))
		if err != nil {
			return nil, nil
		}
		args = append(args, root)
		query = `WITH RECURSIVE thread AS (
				SELECT c.id, c.revision, c.content, c.deleted_at, c.resolved_at FROM comment c WHERE c.id = $2 AND c.issue_id = $1
				UNION ALL
				SELECT c.id, c.revision, c.content, c.deleted_at, c.resolved_at FROM comment c JOIN thread t ON c.parent_id = t.id WHERE c.issue_id = $1
			) SELECT string_agg(jsonb_build_array(id, revision, md5(content), deleted_at, resolved_at)::text, ';' ORDER BY id) FROM thread`
	default:
		return nil, nil
	}
	var value *string
	if err := db.QueryRow(ctx, query, args...).Scan(&value); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return value, nil
}
