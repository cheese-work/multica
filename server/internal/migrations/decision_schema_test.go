package migrations

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var decisionSchemaMigrations = []string{
	"597_decision_parents", "598_decision_request_id_index", "599_decision_intent_id_index",
	"600_decision_dependents", "601_decision_answer_id_index", "602_decision_target_identity_index",
	"603_decision_live_answer_index", "604_decision_open_intent_index", "605_decision_answer_comment_index",
	"606_decision_request_comment_index", "607_decision_answer_messages_index", "608_decision_request_scope_index",
	"609_decision_worker_lease_name_index", "610_decision_class_change_request_index", "611_decision_answer_request_index",
	"612_decision_target_primary_key", "613_decision_comment_kind_validate",
	"615_decision_request_comment_v2_index", "616_decision_request_comment_index_retire",
	"617_decision_comment_request_check", "618_decision_comment_request_validate",
	"690_decision_contract", "730_decision_card_answer",
}

func TestDecisionSchemaV14(test *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		test.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "decision_schema_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		test.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			test.Error(err)
		}
	})
	fixture := testutil.New(pool, "", "")
	fixture.Exec(test, `
		CREATE TABLE comment (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), content text NOT NULL DEFAULT 'legacy');
		CREATE TABLE chat_message (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), message_kind text NOT NULL DEFAULT 'message');
		CREATE TABLE workspace (id uuid PRIMARY KEY DEFAULT gen_random_uuid());
	`)
	legacyComment := fixture.Insert(test, "comment", testutil.Cols{"content": "legacy"})
	legacyMessage := fixture.Insert(test, "chat_message", testutil.Cols{"message_kind": "message"})
	for _, migration := range decisionSchemaMigrations {
		fixture.Exec(test, readMigrationFile(test, migration+".up.sql"))
		if migration == "597_decision_parents" {
			test.Run("comment_constraint_not_valid", func(test *testing.T) {
				if count := fixture.Count(test, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'comment'::regclass AND conname = 'comment_decision_comment_kind_check' AND NOT convalidated`); count != 1 {
					test.Fatal("comment CHECK must be NOT VALID until the separate validation migration")
				}
				fixture.Insert(test, "comment", testutil.Cols{"decision_request_id": uuid.NewString(), "decision_comment_kind": "ask"})
				_, err := pool.Exec(ctx, `INSERT INTO comment (decision_comment_kind) VALUES ('unknown')`)
				var pgError *pgconn.PgError
				if !errors.As(err, &pgError) || pgError.Code != "23514" {
					test.Fatalf("NOT VALID must still enforce new writes: %v", err)
				}
			})
		}
		if migration == "617_decision_comment_request_check" {
			test.Run("comment_request_constraint_not_valid", func(test *testing.T) {
				if count := fixture.Count(test, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'comment'::regclass AND conname = 'comment_decision_request_check' AND NOT convalidated`); count != 1 {
					test.Fatal("comment request CHECK must be NOT VALID until the separate validation migration")
				}
				_, err := pool.Exec(ctx, `INSERT INTO comment (decision_comment_kind) VALUES ('ask')`)
				var pgError *pgconn.PgError
				if !errors.As(err, &pgError) || pgError.Code != "23514" || pgError.ConstraintName != "comment_decision_request_check" {
					test.Fatalf("NOT VALID request CHECK must still enforce new writes: %v", err)
				}
			})
		}
	}
	test.Run("comment_constraint_validated", func(test *testing.T) {
		if count := fixture.Count(test, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'comment'::regclass AND conname = 'comment_decision_comment_kind_check' AND convalidated`); count != 1 {
			test.Fatal("the separate migration must validate the comment CHECK")
		}
		if count := fixture.Count(test, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'comment'::regclass AND conname = 'comment_decision_request_check' AND convalidated`); count != 1 {
			test.Fatal("comment request CHECK was not validated")
		}
	})
	test.Run("query_round_trip_and_scope", func(test *testing.T) {
		newID := func() pgtype.UUID { return pgtype.UUID{Bytes: uuid.New(), Valid: true} }
		workspaceID := newID()
		fixture.Insert(test, "workspace", testutil.Cols{"id": workspaceID})
		tx, err := decisiontx.Begin(ctx, pool, workspaceID)
		if err != nil {
			test.Fatal(err)
		}
		defer tx.Rollback(ctx)
		queries := db.New(tx)
		issueID, agentID, humanID, sessionID := newID(), newID(), newID(), newID()
		intent, err := queries.CreateDecisionIntent(ctx, db.CreateDecisionIntentParams{WorkspaceID: workspaceID, IssueID: issueID, TaskID: newID(), AgentID: agentID, CommentID: newID()})
		if err != nil {
			test.Fatal(err)
		}
		if intent.ExpiresAt.Time.Sub(intent.OpenedAt.Time) != 3*time.Minute || intent.SealedAt.Valid || intent.GroupWokenAt.Valid {
			test.Fatalf("unexpected intent clock/state: %+v", intent)
		}
		if _, err := queries.LockDecisionPendingIntent(ctx, db.LockDecisionPendingIntentParams{WorkspaceID: workspaceID, TaskID: intent.TaskID}); err != nil {
			test.Fatal(err)
		}
		if _, err := queries.CreateDecisionIntentTarget(ctx, db.CreateDecisionIntentTargetParams{DecisionIntentID: intent.ID, HumanUserID: humanID}); err != nil {
			test.Fatal(err)
		}
		if _, err := queries.LockDecisionIntent(ctx, db.LockDecisionIntentParams{WorkspaceID: workspaceID, ID: intent.ID}); err != nil {
			test.Fatal(err)
		}
		if _, err := queries.LockDecisionIntentTarget(ctx, db.LockDecisionIntentTargetParams{WorkspaceID: workspaceID, DecisionIntentID: intent.ID, HumanUserID: humanID}); err != nil {
			test.Fatal(err)
		}
		request, err := queries.CreateDecisionRequest(ctx, db.CreateDecisionRequestParams{WorkspaceID: workspaceID, IssueID: issueID, AskingAgentID: agentID, HumanUserID: humanID, ChatSessionID: sessionID, PingMessageID: newID(), Ask: "Question", EscalationClass: "deploy", Origin: "asked", DecisionIntentID: intent.ID})
		if err != nil {
			test.Fatal(err)
		}
		if _, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: request.ID}); err != nil {
			test.Fatal(err)
		}
		answer, err := queries.CreateDecisionAnswer(ctx, db.CreateDecisionAnswerParams{DecisionRequestID: request.ID, AnsweredByTaskID: newID(), SourceChatSessionID: sessionID, ChatMessageIds: []pgtype.UUID{newID()}, ContentSnapshot: "Proceed", EffectiveAt: pgtype.Timestamptz{Time: time.Now().Add(10 * time.Minute), Valid: true}})
		if err != nil {
			test.Fatal(err)
		}
		if answer.WakeAt.Valid || answer.WokenAt.Valid || answer.DisclaimedAt.Valid || request.ChatSessionID != answer.SourceChatSessionID {
			test.Fatalf("unexpected request/answer initial state: %+v, %+v", request, answer)
		}
		if _, err := queries.LockDecisionAnswer(ctx, db.LockDecisionAnswerParams{WorkspaceID: workspaceID, ID: answer.ID}); err != nil {
			test.Fatal(err)
		}
		read, err := queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: request.ID})
		if err != nil || read.Ask != request.Ask {
			test.Fatalf("request read: %+v, %v", read, err)
		}
		listed, err := queries.ListDecisionRequestsByIssue(ctx, db.ListDecisionRequestsByIssueParams{WorkspaceID: workspaceID, IssueID: issueID})
		if err != nil || len(listed) != 1 || listed[0].ID != request.ID {
			test.Fatalf("request list: %+v, %v", listed, err)
		}
		otherWorkspace := newID()
		if _, err := queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: otherWorkspace, ID: request.ID}); !errors.Is(err, pgx.ErrNoRows) {
			test.Fatalf("cross-workspace get: %v", err)
		}
		if _, err := queries.LockDecisionAnswer(ctx, db.LockDecisionAnswerParams{WorkspaceID: otherWorkspace, ID: answer.ID}); !errors.Is(err, pgx.ErrNoRows) {
			test.Fatalf("cross-workspace answer lock: %v", err)
		}
		if _, err := queries.LockDecisionIntentTarget(ctx, db.LockDecisionIntentTargetParams{WorkspaceID: otherWorkspace, DecisionIntentID: intent.ID, HumanUserID: humanID}); !errors.Is(err, pgx.ErrNoRows) {
			test.Fatalf("cross-workspace target lock: %v", err)
		}
		listed, err = queries.ListDecisionRequestsByIssue(ctx, db.ListDecisionRequestsByIssueParams{WorkspaceID: otherWorkspace, IssueID: issueID})
		if err != nil || len(listed) != 0 {
			test.Fatalf("cross-workspace list: %+v, %v", listed, err)
		}
		if err := tx.Commit(ctx); err != nil {
			test.Fatal(err)
		}
	})
	test.Run("schema_and_foreign_keys", func(test *testing.T) {
		columns := map[string][]string{
			"decision_request":       {"workspace_id", "issue_id", "asking_agent_id", "human_user_id", "chat_session_id", "ping_message_id", "issue_comment_id", "ask", "escalation_class", "status", "origin", "decision_intent_id", "cancel_reason", "cancel_notice_posted_at", "ping_notified_at", "reask_delivered_at", "reask_outcome", "created_at"},
			"decision_answer":        {"decision_request_id", "answered_by_task_id", "source_chat_session_id", "chat_message_ids", "content_snapshot", "effective_at", "wake_at", "woken_at", "wake_outcome", "issue_comment_id", "disclaimed_at", "disclaim_comment_posted_at", "disclaimed_by_user_id", "disclaim_reason", "disclaimed_by_agent_id", "disclaimed_by_task_id", "veto_reason", "veto_note_posted_at", "retraction_delivered_at", "retraction_outcome", "withdrawal_comment_id", "created_at"},
			"decision_intent":        {"workspace_id", "issue_id", "task_id", "agent_id", "comment_id", "opened_at", "expires_at", "consumed_at", "consumed_by_request_id", "closed_at", "close_reason", "sealed_at", "group_woken_at"},
			"decision_intent_target": {"decision_intent_id", "human_user_id", "consumed_at", "consumed_by_request_id"},
			"decision_class_change":  {"decision_request_id", "from_class", "to_class", "changed_by_user_id", "created_at"},
			"decision_worker_lease":  {"name", "holder", "token", "expires_at"},
			"comment":                {"decision_request_id", "decision_answer_id", "decision_intent_id", "decision_comment_kind"},
		}
		for table, names := range columns {
			for _, name := range names {
				if count := fixture.Count(test, `SELECT count(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`, schema, table, name); count != 1 {
					test.Errorf("missing %s.%s", table, name)
				}
			}
		}
		rows, err := pool.Query(ctx, `SELECT child.relname, parent.relname, constraint_row.confdeltype::text
			FROM pg_constraint constraint_row JOIN pg_class child ON child.oid = constraint_row.conrelid
			JOIN pg_class parent ON parent.oid = constraint_row.confrelid
			JOIN pg_namespace namespace ON namespace.oid = child.relnamespace
			WHERE namespace.nspname = $1 AND constraint_row.contype = 'f' ORDER BY child.relname`, schema)
		if err != nil {
			test.Fatal(err)
		}
		defer rows.Close()
		var actual []string
		for rows.Next() {
			var child, parent, action string
			if err := rows.Scan(&child, &parent, &action); err != nil {
				test.Fatal(err)
			}
			actual = append(actual, child+"->"+parent+":"+action)
		}
		if err := rows.Err(); err != nil {
			test.Fatal(err)
		}
		want := "decision_answer->decision_request:c,decision_class_change->decision_request:c,decision_intent_target->decision_intent:c"
		if strings.Join(actual, ",") != want {
			test.Fatalf("foreign keys = %v, want only %s", actual, want)
		}
		if count := fixture.Count(test, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'decision_intent_target'::regclass AND contype = 'p'`); count != 1 {
			test.Error("intent targets need the contract's composite primary key")
		}
		if count := fixture.Count(test, `SELECT count(*) FROM comment WHERE id = $1 AND decision_intent_id IS NULL AND decision_request_id IS NULL AND decision_answer_id IS NULL AND decision_comment_kind IS NULL`, legacyComment); count != 1 {
			test.Fatal("legacy comments must remain unattested")
		}
	})
	requestColumns := func() testutil.Cols {
		return testutil.Cols{"workspace_id": uuid.NewString(), "issue_id": uuid.NewString(), "asking_agent_id": uuid.NewString(), "human_user_id": uuid.NewString(), "chat_session_id": uuid.NewString(), "ask": "Pick a lane", "escalation_class": "reversible", "origin": "asked"}
	}
	answerColumns := func(requestID string) testutil.Cols {
		return testutil.Cols{"decision_request_id": requestID, "answered_by_task_id": uuid.NewString(), "source_chat_session_id": uuid.NewString(), "chat_message_ids": []string{uuid.NewString()}, "content_snapshot": "Proceed", "effective_at": time.Now()}
	}
	expectCode := func(test *testing.T, code, statement string, arguments ...any) {
		test.Helper()
		_, err := pool.Exec(ctx, statement, arguments...)
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != code {
			test.Fatalf("want SQLSTATE %s, got %v", code, err)
		}
	}
	test.Run("comment_kind_requires_matching_stamp", func(test *testing.T) {
		for _, kind := range []string{"ask", "quote", "disclaim", "delivery", "cancel", "veto"} {
			test.Run(kind, func(test *testing.T) {
				expectCode(test, "23514", `INSERT INTO comment (decision_comment_kind) VALUES ($1)`, kind)
				if kind == "ask" || kind == "cancel" {
					fixture.Insert(test, "comment", testutil.Cols{"decision_request_id": uuid.NewString(), "decision_comment_kind": kind})
					expectCode(test, "23514", `INSERT INTO comment (decision_answer_id, decision_comment_kind) VALUES ($1, $2)`, uuid.NewString(), kind)
					expectCode(test, "23514", `INSERT INTO comment (decision_request_id, decision_answer_id, decision_comment_kind) VALUES ($1, $2, $3)`, uuid.NewString(), uuid.NewString(), kind)
				} else {
					fixture.Insert(test, "comment", testutil.Cols{"decision_answer_id": uuid.NewString(), "decision_comment_kind": kind})
					expectCode(test, "23514", `INSERT INTO comment (decision_request_id, decision_comment_kind) VALUES ($1, $2)`, uuid.NewString(), kind)
					fixture.Insert(test, "comment", testutil.Cols{"decision_request_id": uuid.NewString(), "decision_answer_id": uuid.NewString(), "decision_comment_kind": kind})
				}
			})
		}
		fixture.Insert(test, "comment", testutil.Cols{"decision_intent_id": uuid.NewString()})
		fixture.Insert(test, "comment", testutil.Cols{"decision_answer_id": uuid.NewString()})
		fixture.Insert(test, "comment", testutil.Cols{"decision_request_id": uuid.NewString()})
		commentID := fixture.Insert(test, "comment", testutil.Cols{"content": "ordinary"})
		expectCode(test, "23514", `UPDATE comment SET decision_comment_kind = 'ask' WHERE id = $1`, commentID)
	})
	test.Run("request_comment_index_predicate", func(test *testing.T) {
		var predicate string
		if err := pool.QueryRow(ctx, `SELECT pg_get_expr(index_row.indpred, index_row.indrelid)
			FROM pg_index index_row JOIN pg_class index_name ON index_name.oid = index_row.indexrelid
			WHERE index_row.indrelid = 'comment'::regclass AND index_name.relname = 'comment_decision_request_kind_v2_uidx' AND index_row.indisvalid`).Scan(&predicate); err != nil {
			test.Fatal(err)
		}
		if predicate != "((decision_answer_id IS NULL) AND (decision_request_id IS NOT NULL))" {
			test.Fatalf("request comment index predicate = %s", predicate)
		}
		if count := fixture.Count(test, `SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND indexname = 'comment_decision_request_kind_uidx'`, schema); count != 0 {
			test.Fatal("original request comment index was not retired")
		}
	})
	test.Run("live_answer_and_comment_idempotency", func(test *testing.T) {
		requestID := fixture.Insert(test, "decision_request", requestColumns())
		answerID := fixture.Insert(test, "decision_answer", answerColumns(requestID))
		expectCode(test, "23505", `INSERT INTO decision_answer (decision_request_id, answered_by_task_id, source_chat_session_id, chat_message_ids, content_snapshot, effective_at) SELECT decision_request_id, answered_by_task_id, source_chat_session_id, chat_message_ids, content_snapshot, effective_at FROM decision_answer WHERE id = $1`, answerID)
		fixture.Insert(test, "comment", testutil.Cols{"decision_request_id": requestID, "decision_comment_kind": "ask"})
		expectCode(test, "23505", `INSERT INTO comment (decision_request_id, decision_comment_kind) VALUES ($1, 'ask')`, requestID)
		fixture.Insert(test, "comment", testutil.Cols{"decision_request_id": requestID, "decision_answer_id": answerID, "decision_comment_kind": "quote"})
		expectCode(test, "23505", `INSERT INTO comment (decision_request_id, decision_answer_id, decision_comment_kind) VALUES ($1, $2, 'quote')`, requestID, answerID)
		fixture.Exec(test, `UPDATE decision_answer SET disclaimed_at = clock_timestamp(), disclaim_reason = 'human' WHERE id = $1`, answerID)
		replacement := fixture.Insert(test, "decision_answer", answerColumns(requestID))
		fixture.Insert(test, "comment", testutil.Cols{"decision_request_id": requestID, "decision_answer_id": replacement, "decision_comment_kind": "quote"})
		if count := fixture.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, requestID); count != 2 {
			test.Fatal("withdrawal must preserve answer history")
		}
		expectCode(test, "23514", `UPDATE decision_request SET escalation_class = 'unknown' WHERE id = $1`, requestID)
		expectCode(test, "23514", `UPDATE decision_answer SET veto_reason = repeat('界', 601) WHERE id = $1`, answerID)
	})
	test.Run("intent_and_target_identity", func(test *testing.T) {
		intentID := fixture.Insert(test, "decision_intent", testutil.Cols{"workspace_id": uuid.NewString(), "issue_id": uuid.NewString(), "task_id": uuid.NewString(), "agent_id": uuid.NewString(), "comment_id": uuid.NewString(), "expires_at": time.Now().Add(3 * time.Minute)})
		expectCode(test, "23505", `INSERT INTO decision_intent (workspace_id, issue_id, task_id, agent_id, comment_id, expires_at) SELECT workspace_id, issue_id, task_id, agent_id, comment_id, expires_at FROM decision_intent WHERE id = $1`, intentID)
		humanID := uuid.NewString()
		fixture.Exec(test, `INSERT INTO decision_intent_target (decision_intent_id, human_user_id) VALUES ($1, $2)`, intentID, humanID)
		expectCode(test, "23505", `INSERT INTO decision_intent_target (decision_intent_id, human_user_id) VALUES ($1, $2)`, intentID, humanID)
		fixture.Exec(test, `UPDATE decision_intent SET closed_at = clock_timestamp(), close_reason = 'superseded', sealed_at = clock_timestamp() WHERE id = $1`, intentID)
		fixture.Exec(test, `INSERT INTO decision_intent (workspace_id, issue_id, task_id, agent_id, comment_id, expires_at) SELECT workspace_id, issue_id, task_id, agent_id, comment_id, clock_timestamp() + interval '3 minutes' FROM decision_intent WHERE id = $1`, intentID)
		fixture.Exec(test, `DELETE FROM decision_intent WHERE id = $1`, intentID)
		if count := fixture.Count(test, `SELECT count(*) FROM decision_intent_target WHERE decision_intent_id = $1`, intentID); count != 0 {
			test.Fatal("intent cleanup must cascade only within decision tables")
		}
	})
	test.Run("no_implicit_workspace_lock", func(test *testing.T) {
		workspaceID := fixture.Insert(test, "workspace", testutil.Cols{"id": uuid.NewString()})
		holder, err := pool.Begin(ctx)
		if err != nil {
			test.Fatal(err)
		}
		defer holder.Rollback(ctx)
		if _, err := holder.Exec(ctx, `SELECT id FROM workspace WHERE id = $1 FOR UPDATE`, workspaceID); err != nil {
			test.Fatal(err)
		}
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if _, err := pool.Exec(bounded, `INSERT INTO decision_request (workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, ask, escalation_class, origin) VALUES ($1, $2, $3, $4, $5, 'Question', 'reversible', 'asked')`, workspaceID, uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()); err != nil {
			test.Fatalf("decision insert acquired an implicit baseline-owner lock: %v", err)
		}
	})
	for index := len(decisionSchemaMigrations) - 1; index >= 0; index-- {
		fixture.Exec(test, readMigrationFile(test, decisionSchemaMigrations[index]+".down.sql"))
		if decisionSchemaMigrations[index] == "615_decision_request_comment_v2_index" {
			test.Run("comment_hardening_rollback", func(test *testing.T) {
				var predicate string
				if err := pool.QueryRow(ctx, `SELECT pg_get_expr(indpred, indrelid) FROM pg_index WHERE indexrelid = 'comment_decision_request_kind_uidx'::regclass AND indisvalid`).Scan(&predicate); err != nil {
					test.Fatal(err)
				}
				if predicate != "(decision_answer_id IS NULL)" {
					test.Fatalf("rollback request comment index predicate = %s", predicate)
				}
				if count := fixture.Count(test, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'comment'::regclass AND conname = 'comment_decision_request_check'`); count != 0 {
					test.Fatal("rollback left comment request CHECK")
				}
				fixture.Insert(test, "comment", testutil.Cols{"decision_comment_kind": "ask"})
			})
		}
	}
	if count := fixture.Count(test, `SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name LIKE 'decision_%'`, schema); count != 0 {
		test.Fatal("rollback left decision tables")
	}
	if count := fixture.Count(test, `SELECT count(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'comment' AND column_name LIKE 'decision_%'`, schema); count != 0 {
		test.Fatal("rollback left comment provenance")
	}
	if count := fixture.Count(test, `SELECT count(*) FROM comment WHERE id = $1 AND content = 'legacy'`, legacyComment); count != 1 {
		test.Fatal("rollback changed a baseline comment")
	}
	if count := fixture.Count(test, `SELECT count(*) FROM chat_message WHERE id = $1 AND message_kind = 'message'`, legacyMessage); count != 1 {
		test.Fatal("rollback changed a baseline message")
	}
}

func TestDecisionIndexMigrationStructure(test *testing.T) {
	for _, migration := range decisionSchemaMigrations {
		if !strings.HasSuffix(migration, "_index") {
			continue
		}
		up := strings.TrimSpace(readMigrationFile(test, migration+".up.sql"))
		if strings.Count(up, ";") != 1 || !(strings.HasPrefix(up, "CREATE INDEX CONCURRENTLY") || strings.HasPrefix(up, "CREATE UNIQUE INDEX CONCURRENTLY")) {
			test.Errorf("%s must contain one concurrent index build", migration)
		}
		down := strings.TrimSpace(readMigrationFile(test, migration+".down.sql"))
		if strings.Count(down, ";") != 1 || !strings.HasPrefix(down, "DROP INDEX CONCURRENTLY") {
			test.Errorf("%s must contain one concurrent index removal", migration)
		}
	}
}

func TestDecisionCommentConstraintMigrationStructure(test *testing.T) {
	parents := strings.Join(strings.Fields(readMigrationFile(test, "597_decision_parents.up.sql")), " ")
	for _, statement := range strings.Split(parents, ";") {
		if strings.Contains(statement, "ADD COLUMN IF NOT EXISTS decision_comment_kind") && strings.Contains(statement, "CHECK") {
			test.Error("comment columns must not include an inline CHECK that scans under ACCESS EXCLUSIVE")
		}
	}
	if !strings.HasPrefix(parents, "SET LOCAL lock_timeout = '2s';") {
		test.Error("comment DDL must set a transaction-local lock timeout before acquiring locks")
	}
	if !strings.Contains(parents, "ADD CONSTRAINT comment_decision_comment_kind_check CHECK") || !strings.Contains(parents, ") NOT VALID;") {
		test.Error("comment CHECK must be added separately as NOT VALID")
	}
	if strings.Contains(parents, "VALIDATE CONSTRAINT") {
		test.Error("comment CHECK validation must not share the column-addition transaction")
	}
	validation := strings.Join(strings.Fields(readMigrationFile(test, "613_decision_comment_kind_validate.up.sql")), " ")
	if validation != "SET LOCAL lock_timeout = '2s'; ALTER TABLE comment VALIDATE CONSTRAINT comment_decision_comment_kind_check;" {
		test.Error("the separate validation migration must acquire only the validation lock with a local timeout")
	}
	requestCheck := strings.Join(strings.Fields(readMigrationFile(test, "617_decision_comment_request_check.up.sql")), " ")
	if requestCheck != "SET LOCAL lock_timeout = '2s'; DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'comment'::regclass AND conname = 'comment_decision_request_check') THEN ALTER TABLE comment ADD CONSTRAINT comment_decision_request_check CHECK ( decision_comment_kind IS NULL OR (decision_comment_kind IN ('ask', 'cancel') AND decision_request_id IS NOT NULL AND decision_answer_id IS NULL) OR (decision_comment_kind IN ('quote', 'disclaim', 'delivery', 'veto') AND decision_answer_id IS NOT NULL) ) NOT VALID; END IF; END; $$;" {
		test.Error("comment request CHECK must be added as NOT VALID with a local lock timeout")
	}
	requestValidation := strings.Join(strings.Fields(readMigrationFile(test, "618_decision_comment_request_validate.up.sql")), " ")
	if requestValidation != "SET LOCAL lock_timeout = '2s'; ALTER TABLE comment VALIDATE CONSTRAINT comment_decision_request_check;" {
		test.Error("comment request CHECK must be validated in a separate migration with a local lock timeout")
	}
}
