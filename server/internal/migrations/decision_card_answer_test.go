package migrations

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A card answer is a human click on a decision card: it names the user, never
// a task, and carries no chat or comment source. These tests run against a
// fully migrated database and roll everything back.
func TestDecisionCardAnswerSchema(test *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		test.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	newID := func() pgtype.UUID { return pgtype.UUID{Bytes: uuid.New(), Valid: true} }
	humanID, sessionID := newID(), newID()
	soon := pgtype.Timestamptz{Time: time.Now(), Valid: true}

	newRequest := func(test *testing.T, tx pgx.Tx) (*db.Queries, db.DecisionRequest) {
		queries := db.New(tx)
		workspaceID, issueID, agentID := newID(), newID(), newID()
		intent, err := queries.CreateDecisionIntent(ctx, db.CreateDecisionIntentParams{WorkspaceID: workspaceID, IssueID: issueID, TaskID: newID(), AgentID: agentID, CommentID: newID()})
		if err != nil {
			test.Fatal(err)
		}
		request, err := queries.CreateDecisionRequest(ctx, db.CreateDecisionRequestParams{WorkspaceID: workspaceID, IssueID: issueID, AskingAgentID: agentID, HumanUserID: humanID, ChatSessionID: sessionID, PingMessageID: newID(), Ask: "Question", EscalationClass: "deploy", Origin: "asked", DecisionIntentID: intent.ID})
		if err != nil {
			test.Fatal(err)
		}
		return queries, request
	}
	begin := func(test *testing.T) pgx.Tx {
		tx, err := pool.Begin(ctx)
		if err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() { _ = tx.Rollback(ctx) })
		return tx
	}
	expectCheck := func(test *testing.T, err error) {
		test.Helper()
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "23514" {
			test.Fatalf("want check violation, got %v", err)
		}
	}

	test.Run("card_answer_round_trip", func(test *testing.T) {
		tx := begin(test)
		queries, request := newRequest(test, tx)
		answer, err := queries.CreateDecisionCardAnswer(ctx, db.CreateDecisionCardAnswerParams{DecisionRequestID: request.ID, AnsweredByUserID: humanID, ContentSnapshot: "Approved", EffectiveAt: soon})
		if err != nil {
			test.Fatal(err)
		}
		if answer.AnswerSource != "card" || answer.AnsweredByTaskID.Valid || answer.AnsweredByUserID != humanID || answer.SourceChatSessionID.Valid || answer.SourceCommentID.Valid || len(answer.ChatMessageIds) != 0 {
			test.Fatalf("unexpected card answer: %+v", answer)
		}
		// A card answer is eligible to wake the asking agent at once.
		if !answer.WakeAt.Valid {
			test.Fatalf("card answer has no wake_at: %+v", answer)
		}
	})
	test.Run("agent_record_unchanged", func(test *testing.T) {
		tx := begin(test)
		queries, request := newRequest(test, tx)
		answer, err := queries.CreateDecisionAnswer(ctx, db.CreateDecisionAnswerParams{DecisionRequestID: request.ID, AnsweredByTaskID: newID(), SourceChatSessionID: sessionID, ChatMessageIds: []pgtype.UUID{newID()}, ContentSnapshot: "Proceed", EffectiveAt: soon})
		if err != nil {
			test.Fatal(err)
		}
		if answer.AnswerSource != "agent_record" || answer.AnsweredByUserID.Valid || answer.WakeAt.Valid {
			test.Fatalf("unexpected agent_record answer: %+v", answer)
		}
	})
	test.Run("agent_record_task_less_chat_answer", func(test *testing.T) {
		tx := begin(test)
		_, request := newRequest(test, tx)
		_, err := tx.Exec(ctx, `INSERT INTO decision_answer (decision_request_id, source_chat_session_id, chat_message_ids, content_snapshot, effective_at) VALUES ($1, $2, ARRAY[gen_random_uuid()], 'x', now())`, request.ID, sessionID)
		if err != nil {
			test.Fatalf("DM card answer without a task must stay valid: %v", err)
		}
	})
	test.Run("card_answer_rejects_task_or_sources", func(test *testing.T) {
		for name, statement := range map[string]string{
			"task":       `INSERT INTO decision_answer (decision_request_id, answered_by_task_id, answered_by_user_id, answer_source, chat_message_ids, content_snapshot, effective_at) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'card', '{}', 'x', now())`,
			"chat":       `INSERT INTO decision_answer (decision_request_id, answered_by_user_id, answer_source, source_chat_session_id, chat_message_ids, content_snapshot, effective_at) VALUES ($1, gen_random_uuid(), 'card', gen_random_uuid(), ARRAY[gen_random_uuid()], 'x', now())`,
			"comment":    `INSERT INTO decision_answer (decision_request_id, answered_by_user_id, answer_source, source_comment_id, chat_message_ids, content_snapshot, effective_at) VALUES ($1, gen_random_uuid(), 'card', gen_random_uuid(), '{}', 'x', now())`,
			"no_user":    `INSERT INTO decision_answer (decision_request_id, answer_source, chat_message_ids, content_snapshot, effective_at) VALUES ($1, 'card', '{}', 'x', now())`,
			"agent_user": `INSERT INTO decision_answer (decision_request_id, answered_by_task_id, answered_by_user_id, answer_source, source_chat_session_id, chat_message_ids, content_snapshot, effective_at) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'agent_record', gen_random_uuid(), ARRAY[gen_random_uuid()], 'x', now())`,
			"bad_source": `INSERT INTO decision_answer (decision_request_id, answered_by_user_id, answer_source, chat_message_ids, content_snapshot, effective_at) VALUES ($1, gen_random_uuid(), 'other', '{}', 'x', now())`,
		} {
			test.Run(name, func(test *testing.T) {
				tx := begin(test)
				_, request := newRequest(test, tx)
				_, err := tx.Exec(ctx, statement, request.ID)
				expectCheck(test, err)
			})
		}
	})
}
