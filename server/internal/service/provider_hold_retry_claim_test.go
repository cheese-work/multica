package service

// Regression coverage for CHE-607: the provider-hold gate CHE-588 added to
// AgentReadiness only covers TRIGGER admission — task.go's retry (FailTask's
// in-tx retry, MaybeRetryFailedTask, RetrySourceContextQuickCreate) and claim
// (claimTask) paths bypassed it entirely, so a task admitted before a hold was
// configured re-queued under the hold on every failure that looked like
// transport flakiness (agent_error.provider_network, runtime_offline —
// exactly the shape a held provider produces). These tests pin the fix the
// same way agent_ready_provider_hold_test.go pins the trigger-side gate:
// held agent refused with dispatch_blocked.provider_hold, unheld (Claude)
// agent unaffected. TestRerunIssue* below extend the same coverage to
// RerunIssue (CHE-675), the last manual admission path the CHE-607 review
// left ungated.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// setWorkspaceProviderHold configures (or clears, with an empty heldProvider)
// the workspace's provider_holds settings and sets the fixture agent's model,
// so ResolveModelProvider/findProviderHold has something real to match on.
// seedAttributionFixture's agent carries no model by default.
func setWorkspaceProviderHold(t *testing.T, pool *pgxpool.Pool, workspaceID, agentID, model, heldProvider string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE agent SET model = $1 WHERE id = $2`, model, agentID); err != nil {
		t.Fatalf("set agent model: %v", err)
	}
	settings := `{}`
	if heldProvider != "" {
		settings = `{"provider_holds":[{"provider":"` + heldProvider + `","text":"CHE-607 test hold"}]}`
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, settings, workspaceID); err != nil {
		t.Fatalf("set workspace provider_holds: %v", err)
	}
}

// TestMaybeRetryFailedTaskRefusesHeldProvider is AC1's regression: a retry
// successor for a held-provider agent must not be enqueued, and must
// terminate with dispatch_blocked.provider_hold rather than silently
// vanishing (which would look identical to "budget exhausted" in the logs)
// or, worse, being created 'queued' and left for a daemon to claim into the
// held provider.
func TestMaybeRetryFailedTaskRefusesHeldProvider(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, _, agentID, issueID := seedAttributionFixture(t, pool)
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "gpt-5.6-sol", "openai")

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("read agent runtime: %v", err)
	}

	var parentID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts, failure_reason)
		VALUES ($1, $2, $3, 'failed', 0, 1, 3, 'agent_error.provider_network')
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&parentID); err != nil {
		t.Fatalf("insert failed parent task: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE parent_task_id = $1 OR id = $1`, parentID)
	})

	parent, err := q.GetAgentTask(ctx, parentID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	child, err := svc.MaybeRetryFailedTask(ctx, parent)
	if err != nil {
		t.Fatalf("MaybeRetryFailedTask: %v", err)
	}
	if child != nil {
		t.Fatalf("expected no retry child under an active provider hold, got %+v", child)
	}

	// The child must exist as a terminal, distinguishable record — not merely
	// "no row at all" the way budget-exhausted / triage skips are — so a
	// person auditing retries can tell "refused by policy" from "gave up".
	var status, failureReason string
	if err := pool.QueryRow(ctx, `
		SELECT status, failure_reason FROM agent_task_queue WHERE parent_task_id = $1 ORDER BY created_at DESC LIMIT 1
	`, parentID).Scan(&status, &failureReason); err != nil {
		t.Fatalf("load retry child row: %v", err)
	}
	if status != "cancelled" {
		t.Errorf("retry child status = %q, want cancelled (never claimable)", status)
	}
	// The prior assert already pins the exact wanted value; a value equal to
	// it cannot also equal agent_error.provider_server_error (the two reasons
	// are distinct string constants), so a separate check for that would be
	// unreachable. TestProviderHoldReasonIsNotAnAgentError is where the two
	// codes are pinned apart at the constant level.
	if failureReason != taskfailure.ReasonDispatchBlockedProviderHold.String() {
		t.Errorf("retry child failure_reason = %q, want %q", failureReason, taskfailure.ReasonDispatchBlockedProviderHold.String())
	}
}

// TestMaybeRetryFailedTaskClaudeUnaffectedByHold is AC4: a workspace hold on
// openai must never touch a Claude-backed agent's retry.
func TestMaybeRetryFailedTaskClaudeUnaffectedByHold(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, _, agentID, issueID := seedAttributionFixture(t, pool)
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "claude-sonnet-5[1m]", "openai")

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("read agent runtime: %v", err)
	}

	var parentID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts, failure_reason)
		VALUES ($1, $2, $3, 'failed', 0, 1, 3, 'agent_error.provider_network')
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&parentID); err != nil {
		t.Fatalf("insert failed parent task: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE parent_task_id = $1 OR id = $1`, parentID)
	})

	parent, err := q.GetAgentTask(ctx, parentID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	child, err := svc.MaybeRetryFailedTask(ctx, parent)
	if err != nil {
		t.Fatalf("MaybeRetryFailedTask: %v", err)
	}
	if child == nil {
		t.Fatal("expected a retry child for a Claude-backed agent even with an openai hold active")
	}
	if child.Status != "queued" {
		t.Errorf("child status = %q, want queued", child.Status)
	}
}

// TestFailTaskInTxRetryRefusesHeldProvider covers the SAME gap on FailTask's
// own in-transaction retry (the primary path — MaybeRetryFailedTask is the
// orphan-sweeper's independent call to the same shared retryEligible), so a
// live failure landing directly on a held agent is refused exactly like the
// sweeper path, not just when picked up later.
func TestFailTaskInTxRetryRefusesHeldProvider(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, _, agentID, issueID := seedAttributionFixture(t, pool)
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "gpt-5.6-sol", "openai")

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("read agent runtime: %v", err)
	}

	var taskID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts)
		VALUES ($1, $2, $3, 'running', 0, 1, 3)
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("insert running task: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE parent_task_id = $1 OR id = $1`, taskID)
	})

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	failed, err := svc.FailTask(ctx, taskID, "stream disconnected", "", "", "", "agent_error.provider_network", false, "", "")
	if err != nil {
		t.Fatalf("FailTask: %v", err)
	}
	if failed.Status != "failed" {
		t.Fatalf("parent status = %q, want failed", failed.Status)
	}
	// The ORIGINAL failure keeps its real proximate cause — only the retry
	// attempt is what the hold refuses.
	if failed.FailureReason.String != "agent_error.provider_network" {
		t.Errorf("parent failure_reason = %q, want unchanged agent_error.provider_network", failed.FailureReason.String)
	}

	var status, failureReason string
	if err := pool.QueryRow(ctx, `
		SELECT status, failure_reason FROM agent_task_queue WHERE parent_task_id = $1 ORDER BY created_at DESC LIMIT 1
	`, taskID).Scan(&status, &failureReason); err != nil {
		t.Fatalf("load retry child row: %v", err)
	}
	if status != "cancelled" {
		t.Errorf("retry child status = %q, want cancelled", status)
	}
	if failureReason != taskfailure.ReasonDispatchBlockedProviderHold.String() {
		t.Errorf("retry child failure_reason = %q, want %q", failureReason, taskfailure.ReasonDispatchBlockedProviderHold.String())
	}
}

// TestFailTaskInTxRetryClaudeUnaffectedByHold is AC4 for FailTask's own
// in-transaction retry — the review that found the claim-path defect also
// flagged this as the one AC4 case the original suite never pinned
// (MaybeRetryFailedTask and claimTask both had one; FailTask's primary retry
// path did not).
func TestFailTaskInTxRetryClaudeUnaffectedByHold(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, _, agentID, issueID := seedAttributionFixture(t, pool)
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "claude-sonnet-5[1m]", "openai")

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("read agent runtime: %v", err)
	}

	var taskID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts)
		VALUES ($1, $2, $3, 'running', 0, 1, 3)
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("insert running task: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE parent_task_id = $1 OR id = $1`, taskID)
	})

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	failed, err := svc.FailTask(ctx, taskID, "stream disconnected", "", "", "", "agent_error.provider_network", false, "", "")
	if err != nil {
		t.Fatalf("FailTask: %v", err)
	}
	if failed.Status != "failed" {
		t.Fatalf("parent status = %q, want failed", failed.Status)
	}

	var childCount int
	var childStatus string
	if err := pool.QueryRow(ctx, `
		SELECT count(*), coalesce(max(status), '') FROM agent_task_queue WHERE parent_task_id = $1
	`, taskID).Scan(&childCount, &childStatus); err != nil {
		t.Fatalf("count retry children: %v", err)
	}
	if childCount != 1 || childStatus != "queued" {
		t.Fatalf("retry child count/status = %d/%q, want 1/queued — a Claude-backed retry must proceed despite the openai hold", childCount, childStatus)
	}
}

// TestFailTaskInTxRetryRefusalPostsIssueNotice covers review finding (a): a
// provider-hold refusal on the retry path must be visible to the human who
// owns the issue, not just recorded in a DB column nobody reads. Without
// this, ProviderHoldNotice's carefully-built agent/provider/policy text is
// computed and immediately discarded, and the user sees only the ORIGINAL
// failure's error text with no explanation of why no retry followed it.
func TestFailTaskInTxRetryRefusalPostsIssueNotice(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, _, agentID, issueID := seedAttributionFixture(t, pool)
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "gpt-5.6-sol", "openai")

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("read agent runtime: %v", err)
	}
	var taskID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts)
		VALUES ($1, $2, $3, 'running', 0, 1, 3)
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("insert running task: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, issueID)
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE parent_task_id = $1 OR id = $1`, taskID)
	})

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	if _, err := svc.FailTask(ctx, taskID, "stream disconnected", "", "", "", "agent_error.provider_network", false, "", ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT content FROM comment WHERE issue_id = $1 ORDER BY created_at`, issueID)
	if err != nil {
		t.Fatalf("list issue comments: %v", err)
	}
	defer rows.Close()
	var contents []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan comment: %v", err)
		}
		contents = append(contents, c)
	}
	var foundNotice bool
	for _, c := range contents {
		if strings.Contains(c, "policy hold") || strings.Contains(c, "openai") {
			foundNotice = true
		}
	}
	if !foundNotice {
		t.Fatalf("no issue comment mentions the provider hold; comments = %#v", contents)
	}
}

// TestClaimTaskRefusesHeldProvider is AC2's regression, revised after review:
// a task already queued when a hold is configured must not reach the
// provider on claim — but staying queued, not being cancelled, is the fix.
// Cancelling destroys real admitted work with no way back once the hold
// lifts (nothing re-queues a cancelled task, and its trigger/coalesced
// comments die undelivered), and AC2's letter only requires the task not
// reach the provider — leaving it queued satisfies that and keeps the work.
// Simulates the exact ordering CHE-607 describes: enqueue first (provider
// healthy), hold second, claim third — then proves the task is still fully
// alive by clearing the hold and claiming it for real.
func TestClaimTaskRefusesHeldProvider(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, _, agentID, issueID := seedAttributionFixture(t, pool)

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("read agent runtime: %v", err)
	}

	// Enqueue BEFORE the hold exists — models the exact scenario CHE-607
	// describes: admission happened while the provider was healthy.
	var taskID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts)
		VALUES ($1, $2, $3, 'queued', 0, 1, 3)
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("insert queued task: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})

	// Hold configured AFTER admission, before claim.
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "gpt-5.6-sol", "openai")

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	claimed, err := svc.ClaimTask(ctx, util.MustParseUUID(agentID))
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if claimed != nil {
		t.Fatalf("expected no task claimed under an active provider hold, got %+v", claimed)
	}

	var status string
	var failureReason pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT status, failure_reason FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status, &failureReason); err != nil {
		t.Fatalf("load task row: %v", err)
	}
	if status != "queued" {
		t.Fatalf("task status = %q, want queued (untouched, not cancelled — the row must survive the hold)", status)
	}
	if failureReason.Valid {
		t.Errorf("task failure_reason = %q, want NULL — a skipped claim attempt is not a terminal outcome", failureReason.String)
	}

	// Prove the task is genuinely still claimable: lift the hold and claim it
	// for real. A cancel-based fix would fail this — there would be nothing
	// left to claim.
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "gpt-5.6-sol", "")
	claimedAfterLift, err := svc.ClaimTask(ctx, util.MustParseUUID(agentID))
	if err != nil {
		t.Fatalf("ClaimTask after hold lifted: %v", err)
	}
	if claimedAfterLift == nil {
		t.Fatal("expected the same task to be claimable once the hold lifted")
	}
	if claimedAfterLift.ID != taskID {
		t.Errorf("claimed task id = %s, want %s", util.UUIDToString(claimedAfterLift.ID), util.UUIDToString(taskID))
	}
}

// TestClaimTaskClaudeUnaffectedByHold is AC4 for the claim path: an openai
// hold must not block a Claude-backed agent's claim.
func TestClaimTaskClaudeUnaffectedByHold(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, _, agentID, issueID := seedAttributionFixture(t, pool)

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("read agent runtime: %v", err)
	}
	var taskID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts)
		VALUES ($1, $2, $3, 'queued', 0, 1, 3)
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("insert queued task: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "claude-sonnet-5[1m]", "openai")

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	claimed, err := svc.ClaimTask(ctx, util.MustParseUUID(agentID))
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected the Claude-backed task to be claimed despite the openai hold")
	}
	if claimed.ID != taskID {
		t.Errorf("claimed task id = %s, want %s", util.UUIDToString(claimed.ID), util.UUIDToString(taskID))
	}
}

// TestRetrySourceContextQuickCreateRefusesHeldProvider covers the manual
// quick-create retry entry point named in CHE-607 (task.go's
// CreateManualQuickCreateRetryTask call site, RetrySourceContextQuickCreate):
// a user clicking retry on a held-provider agent must be refused up front,
// not left to fail against the provider again. Fixture mirrors
// TestManualSourceContextRetryReusesPendingContextExactlyOnce.
func TestRetrySourceContextQuickCreateRefusesHeldProvider(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, userID, agentID, sourceIssueID := seedAttributionFixture(t, pool)
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "gpt-5.6-sol", "openai")

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("load fixture runtime: %v", err)
	}

	contextID := dbid.NewV7()
	payload, err := json.Marshal(QuickCreateContext{
		Type: QuickCreateContextType, Prompt: "retry this", RequesterID: userID,
		WorkspaceID: workspaceID, SourceContextID: util.UUIDToString(contextID),
	})
	if err != nil {
		t.Fatal(err)
	}
	var parentID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, status, priority, context,
			originator_user_id, accountable_user_id
		) VALUES ($1, $2, 'failed', 0, $3, $4, $4)
		RETURNING id
	`, agentID, runtimeID, payload, userID).Scan(&parentID); err != nil {
		t.Fatalf("insert failed source-context task: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO issue_source_context (
			id, workspace_id, origin_task_id, source_issue_id, anchor_comment_id,
			captured_by_user_id, snapshot_version, snapshot, capture_digest, state
		) VALUES ($1, $2, $3, $4, gen_random_uuid(), $5, 1, '{}'::jsonb, 'digest', 'pending')
	`, contextID, workspaceID, parentID, sourceIssueID, userID); err != nil {
		t.Fatalf("insert pending context: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issue_source_context WHERE id = $1`, contextID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE rerun_of_task_id = $1 OR id = $1`, parentID)
	})

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	child, err := svc.RetrySourceContextQuickCreate(
		ctx, util.MustParseUUID(workspaceID), util.MustParseUUID(userID), parentID,
		func(db.Agent) bool { return true },
	)
	if !errors.Is(err, ErrSourceContextRetryProviderHeld) {
		t.Fatalf("RetrySourceContextQuickCreate error = %v, want ErrSourceContextRetryProviderHeld", err)
	}
	if child != nil {
		t.Fatalf("expected no retry task under an active provider hold, got %+v", child)
	}
	// The pending context must stay put — a refused retry has not consumed it,
	// so the exact same click works once the hold lifts.
	stored, err := q.GetIssueSourceContextByID(ctx, db.GetIssueSourceContextByIDParams{
		WorkspaceID: util.MustParseUUID(workspaceID), ID: contextID,
	})
	if err != nil || stored.OriginTaskID != parentID || stored.State != "pending" {
		t.Fatalf("context after refused retry = %+v, err=%v, want unchanged pending/origin=%s", stored, err, util.UUIDToString(parentID))
	}
}

// TestRerunIssueRefusesHeldProvider covers CHE-675: the manual "rerun" button
// (TaskService.RerunIssue / enqueueRerunTask) is another manual admission path
// AgentReadiness never covers — the same gap CHE-607 closed for the
// quick-create retry button above. A rerun of a held-provider agent must be
// refused up front, with nothing cancelled and nothing created (RerunIssue's
// own canInvoke gate documents the same fail-closed contract this mirrors).
func TestRerunIssueRefusesHeldProvider(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, actorID, agentID, issueID := seedAttributionFixture(t, pool)
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "gpt-5.6-sol", "openai")

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	task, err := svc.RerunIssue(ctx, util.MustParseUUID(issueID), pgtype.UUID{}, pgtype.UUID{}, util.MustParseUUID(actorID), nil)
	if !errors.Is(err, ErrRerunProviderHeld) {
		t.Fatalf("RerunIssue error = %v, want ErrRerunProviderHeld", err)
	}
	if task != nil {
		t.Fatalf("expected no task under an active provider hold, got %+v", task)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&count); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if count != 0 {
		t.Fatalf("a refused rerun must not create or cancel any task, found %d rows", count)
	}
}

// TestRerunIssueClaudeUnaffectedByHold is AC4 for the manual rerun path: an
// openai hold must never touch a Claude-backed agent's rerun.
func TestRerunIssueClaudeUnaffectedByHold(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, actorID, agentID, issueID := seedAttributionFixture(t, pool)
	setWorkspaceProviderHold(t, pool, workspaceID, agentID, "claude-sonnet-5[1m]", "openai")

	svc := &TaskService{Queries: q, TxStarter: pool, Bus: events.New()}
	task, err := svc.RerunIssue(ctx, util.MustParseUUID(issueID), pgtype.UUID{}, pgtype.UUID{}, util.MustParseUUID(actorID), nil)
	if err != nil {
		t.Fatalf("RerunIssue: %v", err)
	}
	if task == nil {
		t.Fatal("expected a rerun task for a Claude-backed agent despite the openai hold")
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, task.ID) })
	if task.Status != "queued" {
		t.Errorf("task status = %q, want queued", task.Status)
	}
}
