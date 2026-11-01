package handler

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func requireWakePolicyRefusal(test *testing.T, err error) {
	test.Helper()
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "23514" || !strings.Contains(pgError.Message, "needs a card answer") {
		test.Fatalf("forbidden agent_record wake was not refused with check_violation: %v", err)
	}
}

func requireNoDeliveryEffects(test *testing.T, fixture askedRecordFixture, recorded decisionRecordResult) {
	test.Helper()
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NOT NULL`, recorded.Answer.ID); count != 0 {
		test.Fatal("refused acknowledgement left the answer woken")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND trigger_comment_id = $2`, fixture.issueID, recorded.Comment.ID); count != 0 {
		test.Fatal("refused acknowledgement left a queued run")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'delivery'`, recorded.Answer.ID); count != 0 {
		test.Fatal("refused acknowledgement left a delivery comment")
	}
}

// pauseBeforeWakeAck holds a delivery transaction just before its wake
// acknowledgement, after every earlier read and side effect.
type pauseBeforeWakeAck struct {
	txStarter
	paused  chan<- struct{}
	resume  <-chan struct{}
	pending bool
}

type pauseBeforeWakeAckTx struct {
	pgx.Tx
	starter *pauseBeforeWakeAck
}

func (starter *pauseBeforeWakeAck) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.txStarter.Begin(ctx)
	return pauseBeforeWakeAckTx{Tx: transaction, starter: starter}, err
}

func (transaction pauseBeforeWakeAckTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if starter := transaction.starter; starter.pending && strings.Contains(sql, "UPDATE decision_answer") && strings.Contains(sql, "SET woken_at") {
		starter.pending = false
		starter.paused <- struct{}{}
		select {
		case <-starter.resume:
		case <-ctx.Done():
			return pgconn.CommandTag{}, ctx.Err()
		}
	}
	return transaction.Tx.Exec(ctx, sql, arguments...)
}

// A worker that read a pre-cutover non-card answer of a non-reversible class
// acknowledges it after the trigger migration: the acknowledgement fails and
// every delivery side effect of that transaction rolls back.
func TestDecisionWakeTriggerCutoverRace(test *testing.T) {
	migration, err := os.ReadFile("../../migrations/870_decision_wake_policy.up.sql")
	if err != nil {
		test.Fatal(err)
	}
	fixture, recorded := readyDecisionDelivery(test)
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, fixture.decision.ID)
	// The database before the migration: no trigger.
	dbfx.Exec(test, `DROP TRIGGER decision_answer_wake_policy ON decision_answer`)
	installTrigger := func() {
		dbfx.Exec(test, `DROP TRIGGER IF EXISTS decision_answer_wake_policy ON decision_answer`)
		dbfx.Exec(test, string(migration))
	}
	test.Cleanup(func() {
		if dbfx.Count(test, `SELECT count(*) FROM pg_trigger WHERE tgname = 'decision_answer_wake_policy' AND NOT tgisinternal`) == 0 {
			installTrigger()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	paused, resume := make(chan struct{}, 1), make(chan struct{})
	handler, _ := newDecisionRunnerFixture(test)
	handler.TxStarter = &pauseBeforeWakeAck{txStarter: testPool, paused: paused, resume: resume, pending: true}
	done := make(chan error, 1)
	go func() { done <- handler.DeliverDecisionWake(ctx, fixture.issue.WorkspaceID, fixture.decision.ID) }()
	select {
	case <-paused:
	case err := <-done:
		test.Fatalf("delivery ended before its acknowledgement: %v", err)
	case <-time.After(10 * time.Second):
		test.Fatal("timed out waiting for the worker to reach its acknowledgement")
	}
	// The worker has read the answer and done its delivery work. The migration commits now.
	installTrigger()
	close(resume)
	select {
	case err := <-done:
		requireWakePolicyRefusal(test, err)
	case <-time.After(10 * time.Second):
		test.Fatal("timed out waiting for the refused delivery")
	}
	requireNoDeliveryEffects(test, fixture, recorded)
}

func TestDecisionWakeTriggerRefusesForbiddenAcknowledgement(test *testing.T) {
	for _, class := range []string{"spend", "deploy", "publish", "irreversible", "product_direction"} {
		test.Run(class, func(test *testing.T) {
			fixture, recorded := readyDecisionDelivery(test)
			dbfx.Exec(test, `UPDATE decision_request SET escalation_class = $2 WHERE id = $1`, fixture.decision.ID, class)
			handler, _ := newDecisionRunnerFixture(test)
			requireWakePolicyRefusal(test, deliverDecisionForTest(handler, fixture))
			requireNoDeliveryEffects(test, fixture, recorded)
		})
	}
}

func TestDecisionWakeTriggerAllowedAcknowledgements(test *testing.T) {
	cases := []struct {
		name    string
		class   string
		source  string
		outcome string
	}{
		{"reversible agent_record", "reversible", "agent_record", "run_enqueued"},
		{"lifecycle skip of a non-reversible agent_record", "spend", "agent_record", "skipped_ineligible"},
		{"non-reversible card answer", "spend", "card", "run_enqueued"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			fixture, recorded := readyDecisionDelivery(test)
			dbfx.Exec(test, `UPDATE decision_request SET escalation_class = $2 WHERE id = $1`, fixture.decision.ID, tc.class)
			if tc.source == "card" {
				dbfx.Exec(test, `UPDATE decision_answer SET answer_source = 'card', answered_by_user_id = $2, answered_by_task_id = NULL,
					source_chat_session_id = NULL, source_comment_id = NULL, chat_message_ids = '{}' WHERE id = $1`, recorded.Answer.ID, fixture.humanID)
			}
			rows, err := testHandler.Queries.AcknowledgeDecisionWake(context.Background(), db.AcknowledgeDecisionWakeParams{
				WorkspaceID: fixture.issue.WorkspaceID, ID: recorded.Answer.ID, Outcome: pgtype.Text{String: tc.outcome, Valid: true}})
			if err != nil || rows != 1 {
				test.Fatalf("acknowledgement rows=%d err=%v", rows, err)
			}
		})
	}
}

// The receipt lists non-reversible agent_record answers woken in the 24 h before
// the migration, and nothing a person has no reason to read.
func TestDecisionWakeReceiptQuery(test *testing.T) {
	query, err := os.ReadFile("../../../scripts/decision-wake-receipt.sql")
	if err != nil {
		test.Fatal(err)
	}
	fixture, recorded := readyDecisionDelivery(test)
	// Seed the pre-migration state: the trigger would refuse this wake today.
	dbfx.Exec(test, `ALTER TABLE decision_answer DISABLE TRIGGER decision_answer_wake_policy`)
	test.Cleanup(func() { dbfx.Exec(test, `ALTER TABLE decision_answer ENABLE TRIGGER decision_answer_wake_policy`) })
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, fixture.decision.ID)
	dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'run_enqueued' WHERE id = $1`, recorded.Answer.ID)
	count := func() int {
		return dbfx.Count(test, `SELECT count(*) FROM (`+string(query)+`) receipt WHERE receipt.answer_id = $1`, recorded.Answer.ID)
	}
	if count() != 1 {
		test.Fatal("receipt misses a non-reversible agent_record answer woken just now")
	}
	for name, statement := range map[string]string{
		"older than 24 h": `UPDATE decision_answer SET woken_at = clock_timestamp() - interval '25 hours' WHERE id = $1`,
		"lifecycle skip":  `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'skipped_ineligible' WHERE id = $1`,
		"reversible":      `UPDATE decision_request SET escalation_class = 'reversible' WHERE id = $1`,
	} {
		if name == "reversible" {
			dbfx.Exec(test, statement, fixture.decision.ID)
		} else {
			dbfx.Exec(test, statement, recorded.Answer.ID)
		}
		if count() != 0 {
			test.Fatalf("receipt lists an answer that is %s", name)
		}
		dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, fixture.decision.ID)
		dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'run_enqueued' WHERE id = $1`, recorded.Answer.ID)
	}
}
