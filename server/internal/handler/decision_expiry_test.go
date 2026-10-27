package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func TestDecisionStaleAskExpiry(test *testing.T) {
	resetDecisionLease(test)
	fixture := newAskedRecordFixture(test)
	dbfx.Exec(test, `UPDATE decision_request SET created_at = clock_timestamp() - interval '168 hours' WHERE id = $1`, fixture.decision.ID)
	beforeMessages := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fixture.sessionID)
	beforeTasks := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1`, fixture.agentID)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	stored, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.decision.ID})
	if err != nil || stored.Status != "cancelled" || stored.CancelReason.String != "expired" {
		test.Fatalf("seven-day unanswered ask did not expire: status=%s reason=%s error=%v", stored.Status, stored.CancelReason.String, err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID); count != 0 {
		test.Fatal("expiry created an answer")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE agent_id = $1`, fixture.agentID); count != beforeTasks {
		test.Fatal("expiry dispatched an execution task")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fixture.sessionID); count != beforeMessages {
		test.Fatal("expiry added an individual DM instead of one inbox digest")
	}
	for attempt := 0; attempt < 2; attempt++ {
		dbfx.Exec(test, `DELETE FROM decision_worker_lease`)
		_, restarted := newDecisionRunnerFixture(test)
		decisionPass(test, restarted)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'cancel'`, fixture.decision.ID); count != 1 {
		test.Fatalf("expiry comments=%d, want 1 across worker restarts", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1 AND recipient_id = $2 AND details->>'kind' = 'decision_expiry_digest'`, testWorkspaceID, fixture.humanID); count != 1 {
		test.Fatalf("expiry digests=%d, want 1 across worker restarts", count)
	}
	cards := testHandler.decisionRequestsForMessages(context.Background(), testWorkspaceID, []db.ChatMessage{{ID: fixture.decision.PingMessageID}})
	card := cards[uuidToString(fixture.decision.PingMessageID)]
	if len(card) != 1 || card[0].State != "withdrawn" || !strings.Contains(card[0].Reason, "not an answer") || !strings.Contains(card[0].Reason, "permission") {
		test.Fatalf("expiry card lost its no-permission explanation: %+v", card)
	}
}

func TestDecisionExpiryClockBoundaries(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	for _, boundary := range []struct {
		name, offset, reason string
	}{
		{"one_microsecond_before", "1 microsecond", ""},
		{"exactly_168_hours", "0 microseconds", "expired"},
		{"one_microsecond_after", "-1 microsecond", "expired"},
	} {
		test.Run(boundary.name, func(test *testing.T) {
			ctx := context.Background()
			transaction, err := testPool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer transaction.Rollback(ctx)
			if _, err := transaction.Exec(ctx, `SET LOCAL TIME ZONE 'America/New_York'`); err != nil {
				test.Fatal(err)
			}
			if _, err := transaction.Exec(ctx, `UPDATE decision_request SET created_at = CURRENT_TIMESTAMP - interval '168 hours' + $2::interval WHERE id = $1`, fixture.decision.ID, boundary.offset); err != nil {
				test.Fatal(err)
			}
			queries := testHandler.Queries.WithTx(transaction)
			reason, err := queries.DecisionLifecycleReason(ctx, db.DecisionLifecycleReasonParams{WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.decision.ID})
			if err != nil || reason != boundary.reason {
				test.Fatalf("reason=%q, want %q; error=%v", reason, boundary.reason, err)
			}
			candidates, err := queries.ListDecisionLifecycleCancellations(ctx, decisionBatchSize)
			if err != nil {
				test.Fatal(err)
			}
			selected := false
			for _, candidate := range candidates {
				selected = selected || candidate.ID == fixture.decision.ID
			}
			if selected != (boundary.reason == "expired") {
				test.Fatalf("expiry selection=%v at %s", selected, boundary.name)
			}
		})
	}
	ctx := context.Background()
	transaction, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	queries := testHandler.Queries.WithTx(transaction)
	for index, instant := range []string{"2026-10-26T16:59:59.999998Z", "2026-10-26T16:59:59.999999Z", "2026-10-26T17:00:00Z"} {
		asOf, err := time.Parse(time.RFC3339Nano, instant)
		if err != nil {
			test.Fatal(err)
		}
		digest, err := queries.ClaimDecisionExpiryDigest(ctx, db.ClaimDecisionExpiryDigestParams{ID: dbid.NewV7(), WorkspaceID: fixture.issue.WorkspaceID, HumanUserID: parseUUID(fixture.humanID), AsOf: pgtype.Timestamptz{Time: asOf, Valid: true}})
		if index == 1 {
			if !errors.Is(err, pgx.ErrNoRows) {
				test.Fatalf("second digest before midnight ICT was not deduplicated: %v", err)
			}
			continue
		}
		wantDay := "2026-10-26"
		if index == 2 {
			wantDay = "2026-10-27"
		}
		if err != nil || digest.DigestDay.Time.Format("2006-01-02") != wantDay {
			test.Fatalf("ICT day=%v, want %s; error=%v", digest.DigestDay, wantDay, err)
		}
	}
}

func expireDecisionFixture(test *testing.T, fixture askedRecordFixture) {
	test.Helper()
	dbfx.Exec(test, `UPDATE decision_request SET created_at = clock_timestamp() - interval '169 hours' WHERE id = $1`, fixture.decision.ID)
	if _, err := testHandler.CancelDecisionLifecycle(context.Background(), fixture.issue.WorkspaceID, fixture.decision.ID); err != nil {
		test.Fatal(err)
	}
}

func TestDecisionExpiryDigestGroupingAndNextDay(test *testing.T) {
	test.Setenv("MULTICA_APP_URL", "https://multica.test")
	newFixture := func() askedRecordFixture {
		fixture := newAskedRecordFixture(test)
		dbfx.Exec(test, `UPDATE agent SET name = name || '-' || id::text WHERE id = $1`, fixture.agentID)
		return fixture
	}
	first, second, other := newFixture(), newFixture(), newFixture()
	dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2 WHERE id = $1`, second.decision.ID, first.humanID)
	dbfx.Exec(test, `UPDATE chat_session SET creator_id = $2 WHERE id = $1`, second.sessionID, first.humanID)
	for _, fixture := range []askedRecordFixture{first, second, other} {
		expireDecisionFixture(test, fixture)
	}
	ctx := context.Background()
	if err := testHandler.deliverDecisionExpiryDigest(ctx, first.issue.WorkspaceID, parseUUID(first.humanID)); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_expiry_digest_request item JOIN decision_expiry_digest digest ON digest.id = item.digest_id WHERE digest.workspace_id = $1 AND digest.human_user_id = $2`, testWorkspaceID, first.humanID); count != 2 {
		test.Fatalf("one recipient's digest contains %d asks, want 2", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_expiry_digest_request WHERE request_id = $1`, other.decision.ID); count != 0 {
		test.Fatal("digest included another recipient's private ask")
	}
	var body string
	if err := testPool.QueryRow(ctx, `SELECT body FROM inbox_item WHERE recipient_id = $1 AND details->>'kind' = 'decision_expiry_digest'`, first.humanID).Scan(&body); err != nil || !strings.Contains(body, uuidToString(first.decision.IssueCommentID)) || !strings.Contains(body, uuidToString(second.decision.IssueCommentID)) || !strings.Contains(body, "not an answer or permission") {
		test.Fatalf("digest lost source links or safety statement: body=%q, error=%v", body, err)
	}
	late := newFixture()
	dbfx.Exec(test, `UPDATE decision_request SET human_user_id = $2 WHERE id = $1`, late.decision.ID, first.humanID)
	dbfx.Exec(test, `UPDATE chat_session SET creator_id = $2 WHERE id = $1`, late.sessionID, first.humanID)
	expireDecisionFixture(test, late)
	if err := testHandler.deliverDecisionExpiryDigest(ctx, first.issue.WorkspaceID, parseUUID(first.humanID)); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_expiry_digest_request WHERE request_id = $1`, late.decision.ID); count != 0 {
		test.Fatal("late ask caused a second digest on the same ICT day")
	}
	dbfx.Exec(test, `UPDATE decision_expiry_digest SET digest_day = digest_day - 1 WHERE human_user_id = $1`, first.humanID)
	if err := testHandler.deliverDecisionExpiryDigest(ctx, first.issue.WorkspaceID, parseUUID(first.humanID)); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_expiry_digest_request WHERE request_id = $1`, late.decision.ID); count != 1 {
		test.Fatal("late ask was lost instead of deferred to the next ICT day")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND details->>'kind' = 'decision_expiry_digest'`, first.humanID); count != 2 {
		test.Fatalf("digests across two ICT days=%d, want 2", count)
	}
	dbfx.Exec(test, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, other.humanID)
	if err := testHandler.deliverDecisionExpiryDigest(ctx, other.issue.WorkspaceID, parseUUID(other.humanID)); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_expiry_digest WHERE human_user_id = $1`, other.humanID); count != 0 {
		test.Fatal("removed member received a digest")
	}
}

func TestDecisionExpiryDigestRollbackAndConcurrentRetry(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	expireDecisionFixture(test, fixture)
	broken := *testHandler
	broken.TxStarter = decisionFailCommit{testPool}
	ctx := context.Background()
	if err := broken.deliverDecisionExpiryDigest(ctx, fixture.issue.WorkspaceID, parseUUID(fixture.humanID)); err == nil {
		test.Fatal("injected commit failure succeeded")
	}
	for _, query := range []string{
		`SELECT count(*) FROM decision_expiry_digest WHERE human_user_id = $1`,
		`SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND details->>'kind' = 'decision_expiry_digest'`,
	} {
		if count := dbfx.Count(test, query, fixture.humanID); count != 0 {
			test.Fatal("failed digest leaked a delivery or deduplication receipt")
		}
	}
	finished := make(chan error, 2)
	for attempt := 0; attempt < 2; attempt++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			finished <- testHandler.deliverDecisionExpiryDigest(ctx, fixture.issue.WorkspaceID, parseUUID(fixture.humanID))
		}()
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := <-finished; err != nil {
			test.Fatal(err)
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND details->>'kind' = 'decision_expiry_digest'`, fixture.humanID); count != 1 {
		test.Fatalf("concurrent retry produced %d inbox digests, want 1", count)
	}
	dbfx.Exec(test, `DELETE FROM inbox_item WHERE recipient_id = $1 AND details->>'kind' = 'decision_expiry_digest'`, fixture.humanID)
	if err := testHandler.deliverDecisionExpiryDigest(ctx, fixture.issue.WorkspaceID, parseUUID(fixture.humanID)); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_expiry_digest WHERE human_user_id = $1`, fixture.humanID); count != 1 {
		test.Fatalf("concurrent retry or inbox deletion duplicated the digest: %d", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND details->>'kind' = 'decision_expiry_digest'`, fixture.humanID); count != 0 {
		test.Fatal("inbox deletion replayed an already delivered digest")
	}
}

func TestDecisionExpiryAnswerWithdrawRaces(test *testing.T) {
	for _, competitor := range []string{"answer", "withdraw"} {
		for _, first := range []string{"expiry", "competitor"} {
			test.Run(competitor+"_"+first+"_first", func(test *testing.T) {
				fixture := newAskedRecordFixture(test)
				dbfx.Exec(test, `UPDATE decision_request SET created_at = clock_timestamp() - interval '169 hours' WHERE id = $1`, fixture.decision.ID)
				original := *testHandler
				query := "-- name: CancelDecisionLifecycleRequest"
				if first == "competitor" {
					query = "-- name: CreateDecisionAnswer"
					if competitor == "withdraw" {
						query = "-- name: WithdrawOpenDecisionRequest"
					}
				}
				paused, unblock := pauseDecisionTransaction(test, query)
				start := func(handler *Handler, operation string) <-chan error {
					finished := make(chan error, 1)
					go func() {
						ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
						defer cancel()
						var err error
						switch operation {
						case "expiry":
							_, err = handler.CancelDecisionLifecycle(ctx, fixture.issue.WorkspaceID, fixture.decision.ID)
						case "answer":
							_, err = handler.createAskedDecisionRecord(fixture.recordRequest().WithContext(ctx), fixture.issue, fixture.recordInput())
						case "withdraw":
							_, err = handler.withdrawDecisionRequestTx(fixture.request().WithContext(ctx), fixture.decision, "Superseded")
						}
						finished <- err
					}()
					return finished
				}
				firstOperation, secondOperation := "expiry", competitor
				if first == "competitor" {
					firstOperation, secondOperation = competitor, "expiry"
				}
				firstDone := start(testHandler, firstOperation)
				holderPID := awaitDecisionPause(test, paused)
				secondDone := start(&original, secondOperation)
				if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
					unblock()
					<-firstDone
					<-secondDone
					test.Fatal("expiry and competitor did not contend on the lifecycle fence")
				}
				unblock()
				firstErr, secondErr := <-firstDone, <-secondDone
				if firstErr != nil {
					test.Fatal(firstErr)
				}
				if first == "expiry" {
					requireRequestRefusal(test, secondErr)
				} else if secondErr != nil {
					test.Fatal(secondErr)
				}
				wantReason := "expired"
				if first == "competitor" {
					wantReason = "agent_withdrawn"
					if competitor == "answer" {
						wantReason = ""
					}
				}
				stored, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.decision.ID})
				if err != nil || stored.CancelReason.String != wantReason {
					test.Fatalf("race lost winner: reason=%q, want %q; error=%v", stored.CancelReason.String, wantReason, err)
				}
				wantAnswers := 0
				if first == "competitor" && competitor == "answer" {
					wantAnswers = 1
					if stored.Status != "answered" {
						test.Fatalf("answer-first request status=%s", stored.Status)
					}
				}
				if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID); count != wantAnswers {
					test.Fatalf("answers=%d, want %d", count, wantAnswers)
				}
			})
		}
	}
}

func TestDecisionStaleAskExpiryEligibility(test *testing.T) {
	for _, scenario := range []string{"before", "after", "answered", "withdrawn", "unprompted"} {
		test.Run(scenario, func(test *testing.T) {
			resetDecisionLease(test)
			fixture := newAskedRecordFixture(test)
			wantExpired := scenario == "after"
			if scenario == "answered" {
				if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput()); err != nil {
					test.Fatal(err)
				}
			}
			if scenario == "withdrawn" {
				if _, err := testHandler.withdrawDecisionRequestTx(fixture.request(), fixture.decision, "Superseded"); err != nil {
					test.Fatal(err)
				}
			}
			if scenario == "unprompted" {
				dbfx.Exec(test, `UPDATE decision_request SET origin = 'unprompted' WHERE id = $1`, fixture.decision.ID)
			}
			age := "169 hours"
			if scenario == "before" {
				age = "167 hours 59 minutes"
			}
			dbfx.Exec(test, `UPDATE decision_request SET created_at = clock_timestamp() - $2::interval WHERE id = $1`, fixture.decision.ID, age)
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND cancel_reason = 'expired'`, fixture.decision.ID); (count == 1) != wantExpired {
				test.Fatalf("expired=%v, want %v", count == 1, wantExpired)
			}
		})
	}
}
