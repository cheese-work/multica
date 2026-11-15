package handler

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Unprovable now can be provable later: the check runs this long after the
// waiver took effect. A crash-fix check repeats on its own until its result is
// recorded, because a closed issue accepts no new wakeup.
const (
	agentDecisionFollowupDelay = 7 * 24 * time.Hour
	agentDecisionFollowupRetry = 72 * time.Hour
)

const agentDecisionFollowupCheckMax = 2000

var crashSignaturePattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func agentDecisionFollowupInstruction(instance string, p agentDecisionProposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Follow-up check for the test_first_red waiver (decision %s) on this issue. The red test was waived: the status is WAIVED, never PASS.\n", instance)
	fmt.Fprintf(&b, "Why red was impossible: %s\nWeaker evidence then (%s).\n\n", strings.TrimSpace(p.RedImpossibleReason), p.WeakerEvidenceKind)
	if p.CrashSignature != "" {
		fmt.Fprintf(&b, "This change fixes crash signature %s. After the release that carries the fix, read the Crashlytics crash rate for that signature (firebase-crashlytics MCP) and compare it with the rate before the fix. ", p.CrashSignature)
		fmt.Fprintf(&b, "This check repeats every 72 hours until a comment that starts with `Follow-up result (%s):` exists on this issue. ", instance)
		b.WriteString("If the fix is not released yet, stop without a comment and wait for the next run. If the change will never ship (reverted or abandoned), record that as the result.\n")
		fmt.Fprintf(&b, "Also run the check the waiver names: %s\n\n", strings.TrimSpace(p.FollowupCheck))
	} else {
		fmt.Fprintf(&b, "Check: %s\n\n", strings.TrimSpace(p.FollowupCheck))
	}
	fmt.Fprintf(&b, "Record the result as one comment on this issue that starts with `Follow-up result (%s):`, with the numbers or output and where you read them. ", instance)
	b.WriteString("If the crash rate is worse or the check fails, reopen the work: set this issue to `in_progress` before any change and fix it on a new PR.")
	return b.String()
}

// createWaiverFollowup leaves the check a waiver owes behind as a wakeup on the
// waived issue, in the settlement transaction. The settlement row is inserted
// once, so this runs once per record; the unique index on followup_instance_id
// backs that up. The author runs the check, for the human who started the
// author's task, else the workspace owner.
func createWaiverFollowup(ctx context.Context, tx pgx.Tx, workspaceID, issueID pgtype.UUID, instance string, scope agentDecisionScope) error {
	var member pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT originator_user_id FROM agent_task_queue WHERE id=$1`, scope.Author.TaskID).Scan(&member); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !member.Valid {
		if err := tx.QueryRow(ctx, `SELECT user_id FROM member WHERE workspace_id=$1 AND role='owner' ORDER BY created_at, id LIMIT 1`, workspaceID).Scan(&member); err != nil {
			return fmt.Errorf("no member to own the follow-up check: %w", err)
		}
	}
	// A crash-fix check may find the fix unreleased: it repeats by itself.
	kind, mode := "at", "once"
	var interval any
	if scope.Proposal.CrashSignature != "" {
		kind, mode = "every", "continuous"
		interval = int64(agentDecisionFollowupRetry / time.Second)
	}
	_, err := tx.Exec(ctx, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,created_by,instruction,kind,mode,event_types,timezone,next_fire_at,followup_instance_id,interval_seconds)
 VALUES($1,$2,$3,$4,$5,$6,$9,$10,'{}','UTC',clock_timestamp()+$7::interval,$8,$11)
 ON CONFLICT (followup_instance_id) WHERE followup_instance_id IS NOT NULL DO NOTHING`,
		dbid.NewV7(), workspaceID, issueID, scope.Author.AgentID, member, agentDecisionFollowupInstruction(instance, scope.Proposal), agentDecisionFollowupDelay.String(), instance, kind, mode, interval)
	return err
}
