package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type decisionConsequences interface {
	CheckDecisionSource(context.Context, pgtype.UUID, pgtype.UUID) (decisiontx.Withdrawal, error)
	SealDecisionIntent(context.Context, pgtype.UUID, pgtype.UUID) (db.DecisionIntent, error)
}

func consequenceWorker(test *testing.T, handler *Handler) decisionConsequences {
	test.Helper()
	worker, ok := any(handler).(decisionConsequences)
	if !ok {
		test.Fatal("decision worker cannot withdraw removed sources or seal abandoned intent windows")
	}
	return worker
}

func TestDecisionSourceConsequence(test *testing.T) {
	for _, state := range []string{"present", "edited", "removed", "partial", "archived", "unprompted", "cancelled", "delivered", "deleted_anchor"} {
		test.Run(state, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			if state == "partial" {
				messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": fixture.sessionID, "role": "user", "content": "another source", "sent_via": "interactive_jwt"})
				dbfx.Exec(test, `UPDATE decision_answer SET chat_message_ids = array_append(chat_message_ids, $1::uuid) WHERE id = $2`, messageID, recorded.Answer.ID)
			}
			if state == "edited" {
				dbfx.Exec(test, `UPDATE chat_message SET content = 'edited later' WHERE id = $1`, fixture.messageID)
			} else if state != "present" {
				dbfx.Exec(test, `DELETE FROM chat_message WHERE id = $1`, fixture.messageID)
			}
			if state == "archived" {
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
			}
			if state == "unprompted" {
				dbfx.Exec(test, `UPDATE decision_request SET origin = 'unprompted' WHERE id = $1`, recorded.Request.ID)
			}
			if state == "cancelled" {
				dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'issue_closed' WHERE id = $1`, recorded.Request.ID)
			}
			if state == "deleted_anchor" {
				dbfx.Exec(test, `DELETE FROM comment WHERE id = $1`, fixture.parentID)
			}
			dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, recorded.Answer.ID)
			if state == "delivered" {
				dbfx.Exec(test, `UPDATE decision_answer SET woken_at = clock_timestamp(), wake_outcome = 'queued_agent' WHERE id = $1`, recorded.Answer.ID)
			}
			worker := consequenceWorker(test, testHandler)
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := worker.CheckDecisionSource(context.Background(), parseUUID(testWorkspaceID), recorded.Request.ID); err != nil {
					test.Fatal(err)
				}
			}
			answer, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{WorkspaceID: parseUUID(testWorkspaceID), ID: recorded.Answer.ID})
			if err != nil {
				test.Fatal(err)
			}
			missing := state != "present" && state != "edited"
			if answer.DisclaimedAt.Valid != missing || answer.ContentSnapshot != recorded.Answer.ContentSnapshot || answer.SourceChatSessionID != recorded.Answer.SourceChatSessionID {
				test.Fatalf("source check lost withdrawal or snapshot: %+v", answer)
			}
			if !missing {
				return
			}
			if answer.DisclaimReason.String != "source_removed" || !answer.DisclaimCommentPostedAt.Valid || !answer.WithdrawalCommentID.Valid || answer.DisclaimedByUserID.Valid || answer.DisclaimedByAgentID.Valid || answer.RetractionDeliveredAt.Valid || answer.WakeAt.Valid != (state == "delivered") {
				test.Fatalf("automatic withdrawal receipts: %+v", answer)
			}
			request, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: parseUUID(testWorkspaceID), ID: recorded.Request.ID})
			if err != nil {
				test.Fatal(err)
			}
			wantedStatus, wantedReason := "open", ""
			if state == "archived" {
				wantedStatus, wantedReason = "cancelled", "lane_archived"
			} else if state == "unprompted" {
				wantedStatus, wantedReason = "cancelled", "record_disclaimed"
			} else if state == "cancelled" {
				wantedStatus, wantedReason = "cancelled", "issue_closed"
			}
			if request.Status != wantedStatus || request.CancelReason.String != wantedReason {
				test.Fatalf("request status=%s reason=%s", request.Status, request.CancelReason.String)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'disclaim'`, answer.ID); count != 1 {
				test.Fatalf("withdrawal comments=%d, want exactly one after retry", count)
			}
			wantedCancel := 0
			if state == "archived" {
				wantedCancel = 1
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'cancel'`, request.ID); count != wantedCancel {
				test.Fatalf("cancel comments=%d, want %d", count, wantedCancel)
			}
			if dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID) != 1 {
				test.Fatal("source check synchronously enqueued delivery")
			}
		})
	}
}

func TestDecisionSealConsequence(test *testing.T) {
	for _, state := range []string{"live", "expired", "completed", "cancelled", "failed", "task_deleted", "tag_deleted", "tag_tombstoned", "expired_deleted"} {
		test.Run(state, func(test *testing.T) {
			fixture := newIntentFixture(test)
			openedID, tagID := openLaneIntent(test, fixture, fixture.mention())
			intentID := parseUUID(openedID)
			switch state {
			case "expired", "expired_deleted":
				dbfx.Exec(test, `UPDATE decision_intent SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, intentID)
			case "completed", "cancelled", "failed":
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = $1 WHERE id = $2`, state, fixture.taskID)
			case "task_deleted":
				dbfx.Exec(test, `DELETE FROM agent_task_queue WHERE id = $1`, fixture.taskID)
			case "tag_tombstoned":
				dbfx.Exec(test, `UPDATE comment SET deleted_at = clock_timestamp() WHERE id = $1`, tagID)
			}
			if state == "tag_deleted" || state == "expired_deleted" {
				dbfx.Exec(test, `DELETE FROM comment WHERE id = $1`, tagID)
			}
			worker := consequenceWorker(test, testHandler)
			first, err := worker.SealDecisionIntent(context.Background(), parseUUID(testWorkspaceID), intentID)
			if err != nil {
				test.Fatal(err)
			}
			second, err := worker.SealDecisionIntent(context.Background(), parseUUID(testWorkspaceID), intentID)
			if err != nil {
				test.Fatal(err)
			}
			deleted := state == "tag_deleted" || state == "tag_tombstoned" || state == "expired_deleted"
			if first.SealedAt.Valid != (state != "live") || first.ClosedAt.Valid != deleted || second.SealedAt != first.SealedAt || second.ClosedAt != first.ClosedAt || first.ConsumedAt.Valid {
				test.Fatalf("window seal or retry changed: first=%+v second=%+v", first, second)
			}
			if deleted && first.CloseReason.String != "comment_deleted" {
				test.Fatalf("tag deletion reason=%s", first.CloseReason.String)
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_intent_target WHERE decision_intent_id = $1 AND consumed_at IS NOT NULL`, intentID) != 0 {
				test.Fatal("sealing consumed a target")
			}
		})
	}
}

func TestDecisionConsequenceRollback(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `DELETE FROM chat_message WHERE id = $1`, fixture.messageID)
	failure := errors.New("withdrawal receipt failed")
	broken := *testHandler
	broken.TxStarter = &requestCoreStarter{base: testPool, query: "-- name: WithdrawDecisionAnswer", fail: failure}
	result, err := consequenceWorker(test, &broken).CheckDecisionSource(context.Background(), parseUUID(testWorkspaceID), recorded.Request.ID)
	if !errors.Is(err, failure) || len(result.Comments) != 0 || dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'disclaim'`, recorded.Answer.ID) != 0 {
		test.Fatalf("failed consequence escaped rollback: %+v error=%v", result, err)
	}
	bounded, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := consequenceWorker(test, testHandler).CheckDecisionSource(bounded, parseUUID(testWorkspaceID), recorded.Request.ID); err != nil {
		test.Fatalf("retry after rollback: %v", err)
	}
}

func TestDecisionSealRollback(test *testing.T) {
	fixture := newIntentFixture(test)
	openedID, _ := openLaneIntent(test, fixture, fixture.mention())
	intentID := parseUUID(openedID)
	dbfx.Exec(test, `UPDATE decision_intent SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, intentID)
	failure := errors.New("seal failed")
	broken := *testHandler
	broken.TxStarter = &requestCoreStarter{base: testPool, query: "-- name: SealDecisionIntent", fail: failure}
	result, err := consequenceWorker(test, &broken).SealDecisionIntent(context.Background(), parseUUID(testWorkspaceID), intentID)
	if !errors.Is(err, failure) || result.ID.Valid || dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NOT NULL`, intentID) != 0 {
		test.Fatalf("failed seal escaped rollback: %+v error=%v", result, err)
	}
}

type decisionConsequenceSelectors interface {
	ListDecisionSourceChecks(context.Context, int32) ([]db.DecisionRequest, error)
	ListDecisionSealIntents(context.Context, int32) ([]db.DecisionIntent, error)
}

func TestDecisionConsequenceSelectors(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	openedID, _ := openLaneIntent(test, fixture.intentFixture, fixture.mention())
	intentID := parseUUID(openedID)
	selectors, ok := any(testHandler.Queries).(decisionConsequenceSelectors)
	if !ok {
		test.Fatal("decision worker cannot select bounded source checks and abandoned intents")
	}
	ctx := context.Background()
	sources, err := selectors.ListDecisionSourceChecks(ctx, 10)
	if err != nil || len(sources) != 0 {
		test.Fatalf("live source selected: %+v error=%v", sources, err)
	}
	intents, err := selectors.ListDecisionSealIntents(ctx, 10)
	if err != nil || len(intents) != 0 {
		test.Fatalf("live intent selected: %+v error=%v", intents, err)
	}
	dbfx.Exec(test, `DELETE FROM chat_message WHERE id = $1`, fixture.messageID)
	dbfx.Exec(test, `UPDATE decision_intent SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, intentID)
	for _, limit := range []int32{-1, 0, 1} {
		sources, err = selectors.ListDecisionSourceChecks(ctx, limit)
		if err != nil || len(sources) != int(max(limit, 0)) {
			test.Fatalf("bounded source selector limit=%d: %+v error=%v", limit, sources, err)
		}
		intents, err = selectors.ListDecisionSealIntents(ctx, limit)
		if err != nil || len(intents) != int(max(limit, 0)) {
			test.Fatalf("bounded intent selector limit=%d: %+v error=%v", limit, intents, err)
		}
	}
	if sources[0].ID != recorded.Request.ID || intents[0].ID != intentID {
		test.Fatal("selector lost candidate scope")
	}
	worker := consequenceWorker(test, testHandler)
	if _, err := worker.CheckDecisionSource(ctx, parseUUID(testWorkspaceID), recorded.Request.ID); err != nil {
		test.Fatal(err)
	}
	if _, err := worker.SealDecisionIntent(ctx, parseUUID(testWorkspaceID), intentID); err != nil {
		test.Fatal(err)
	}
	sources, err = selectors.ListDecisionSourceChecks(ctx, 10)
	if err != nil || len(sources) != 0 {
		test.Fatalf("settled source selected again: %+v error=%v", sources, err)
	}
	intents, err = selectors.ListDecisionSealIntents(ctx, 10)
	if err != nil || len(intents) != 0 {
		test.Fatalf("settled intent selected again: %+v error=%v", intents, err)
	}
}
