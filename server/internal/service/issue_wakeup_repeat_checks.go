package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrWakeupHeadUnchanged refuses a repeat checks_finished rule (CHE-5136).
var ErrWakeupHeadUnchanged = errors.New("head unchanged since the last checks_finished fire")

// headUnchangedError carries the PR head sha that has not changed.
type headUnchangedError struct{ head string }

func (e *headUnchangedError) Error() string {
	return fmt.Sprintf("head %s unchanged since the last checks_finished fire; wait for a new head or use --until-status", e.head)
}

func (e *headUnchangedError) Is(target error) bool { return target == ErrWakeupHeadUnchanged }

// refuseRepeatChecksFinished refuses a new pull_request/checks_finished rule an
// agent registers (source is its run) when that agent's previous rule of the
// kind on the issue already fired and every linked PR head is one that fire
// reported. Each such rule would fire on the first poll and wake the agent
// about the same result again. Fail open: no source run, no fired rule, no
// linked head or any head the fire did not report accepts the rule.
func refuseRepeatChecksFinished(ctx context.Context, tx pgx.Tx, issueID, source pgtype.UUID, raw []byte) error {
	var c WakeupCondition
	if !source.Valid || json.Unmarshal(raw, &c) != nil || c.Type != "pull_request" || c.Event != "checks_finished" {
		return nil
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT w.condition_state FROM issue_wakeup w
		JOIN agent_task_queue prev ON prev.id=w.source_task_id
		JOIN agent_task_queue cur ON cur.id=$2 AND cur.agent_id=prev.agent_id
		WHERE w.issue_id=$1 AND w.last_task_id IS NOT NULL AND w.system_rule IS NULL
		  AND w.condition->>'type'='pull_request' AND w.condition->>'event'='checks_finished'
		ORDER BY w.created_at DESC LIMIT 1`, issueID, source).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	fired := firedHeads(state)
	rows, err := tx.Query(ctx, `SELECT pr.head_sha FROM github_pull_request pr JOIN issue_pull_request ipr ON ipr.pull_request_id=pr.id WHERE ipr.issue_id=$1
		UNION ALL SELECT pr.head_sha FROM vcs_pull_request pr JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id=pr.id WHERE ipr.issue_id=$1
		ORDER BY 1`, issueID)
	if err != nil {
		return err
	}
	defer rows.Close()
	first := ""
	for rows.Next() {
		var head string
		if err := rows.Scan(&head); err != nil {
			return err
		}
		if head == "" {
			continue
		}
		if !fired[head] {
			return nil
		}
		if first == "" {
			first = head
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if first == "" {
		return nil
	}
	return &headUnchangedError{head: first}
}

// firedHeads returns the PR head shas in a checks_finished fingerprint, whose
// keys read "<pr>@<head>:<CHECKS>".
func firedHeads(fingerprint string) map[string]bool {
	heads := map[string]bool{}
	for key := range conditionKeys(fingerprint) {
		at := strings.LastIndex(key, "@")
		if at < 0 {
			continue
		}
		head, _, _ := strings.Cut(key[at+1:], ":")
		heads[head] = true
	}
	return heads
}
