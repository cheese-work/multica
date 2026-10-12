package decisiontx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestRetryableDecisionErrors(test *testing.T) {
	for _, code := range []string{"55P03", "40P01", "40001", "23505", "23503", "57014", "42501"} {
		err := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code})
		want := code == "55P03" || code == "40P01"
		if IsRetryable(err) != want {
			test.Errorf("SQLSTATE %s: retryable=%v, want %v", code, IsRetryable(err), want)
		}
	}
	for _, err := range []error{nil, pgx.ErrNoRows, context.DeadlineExceeded, errors.New("connection failed")} {
		if IsRetryable(err) {
			test.Errorf("must not retry unrelated error: %v", err)
		}
	}
}

func TestDecisionWorkspaceFence(test *testing.T) {
	pool, fixture := decisionPool(test)
	ctx := context.Background()
	workspace := fixture.Insert(test, "workspace", testutil.Cols{"id": uuid.NewString()})
	workspaceID := parseTestUUID(test, workspace)
	tx, err := Begin(ctx, pool, workspaceID)
	if err != nil {
		test.Fatal(err)
	}
	defer tx.Rollback(ctx)
	contender, err := pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer contender.Rollback(ctx)
	_, err = contender.Exec(ctx, `SELECT id FROM workspace WHERE id = $1 FOR UPDATE NOWAIT`, workspaceID)
	assertLockRefusal(test, err)
	if err := contender.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	missing, err := Begin(ctx, pool, parseTestUUID(test, uuid.NewString()))
	if missing != nil || !errors.Is(err, pgx.ErrNoRows) {
		test.Fatalf("missing workspace: tx=%v, err=%v", missing, err)
	}
	if count := pool.Stat().AcquiredConns(); count != 0 {
		test.Fatalf("failed fence leaked a transaction: %d acquired connections", count)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT id FROM workspace WHERE id = $1 FOR UPDATE`, workspaceID); err != nil {
		test.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	blocked, err := Begin(bounded, pool, workspaceID)
	if blocked != nil || !errors.Is(err, context.DeadlineExceeded) {
		test.Fatalf("workspace fence must precede returning the transaction: tx=%v, err=%v", blocked, err)
	}
}

func TestDecisionNowaitLocks(test *testing.T) {
	pool, fixture := decisionPool(test)
	ctx := context.Background()
	workspaceID := parseTestUUID(test, fixture.Insert(test, "workspace", testutil.Cols{"id": uuid.NewString()}))
	otherWorkspace := parseTestUUID(test, uuid.NewString())
	agentID := parseTestUUID(test, fixture.Insert(test, "agent", testutil.Cols{"workspace_id": workspaceID}))
	taskID := parseTestUUID(test, fixture.Insert(test, "agent_task_queue", testutil.Cols{"agent_id": agentID}))
	humanID := parseTestUUID(test, uuid.NewString())
	memberID := parseTestUUID(test, fixture.Insert(test, "member", testutil.Cols{"workspace_id": workspaceID, "user_id": humanID}))
	sessionID := parseTestUUID(test, fixture.Insert(test, "chat_session", testutil.Cols{"workspace_id": workspaceID}))
	messageID := parseTestUUID(test, fixture.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID}))
	cases := []struct {
		name, table string
		id          pgtype.UUID
		hold        string
		lock        func(context.Context, *db.Queries, pgtype.UUID) error
	}{
		{"calling_task", "agent_task_queue", taskID, "UPDATE agent_task_queue SET status = 'completed' WHERE id = $1", func(ctx context.Context, queries *db.Queries, scope pgtype.UUID) error {
			_, err := queries.LockDecisionCallingTask(ctx, db.LockDecisionCallingTaskParams{WorkspaceID: scope, ID: taskID})
			return err
		}},
		{"delivery_task", "agent_task_queue", taskID, "UPDATE agent_task_queue SET status = 'completed' WHERE id = $1", func(ctx context.Context, queries *db.Queries, scope pgtype.UUID) error {
			_, err := queries.LockDecisionDeliveryTask(ctx, db.LockDecisionDeliveryTaskParams{WorkspaceID: scope, ID: taskID})
			return err
		}},
		{"asking_agent", "agent", agentID, "UPDATE agent SET status = 'archived' WHERE id = $1", func(ctx context.Context, queries *db.Queries, scope pgtype.UUID) error {
			_, err := queries.LockDecisionAskingAgent(ctx, db.LockDecisionAskingAgentParams{WorkspaceID: scope, ID: agentID})
			return err
		}},
		{"member", "member", memberID, "DELETE FROM member WHERE id = $1", func(ctx context.Context, queries *db.Queries, scope pgtype.UUID) error {
			_, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: scope, UserID: humanID})
			return err
		}},
		{"source_message", "chat_message", messageID, "DELETE FROM chat_message WHERE id = $1", func(ctx context.Context, queries *db.Queries, scope pgtype.UUID) error {
			rows, err := queries.LockDecisionSourceMessages(ctx, db.LockDecisionSourceMessagesParams{WorkspaceID: scope, MessageIds: []pgtype.UUID{messageID}})
			if err == nil && len(rows) == 0 {
				return pgx.ErrNoRows
			}
			return err
		}},
	}
	for _, candidate := range cases {
		test.Run(candidate.name, func(test *testing.T) {
			if err := candidate.lock(ctx, db.New(pool), otherWorkspace); !errors.Is(err, pgx.ErrNoRows) {
				test.Fatalf("cross-workspace lock must find no row: %v", err)
			}
			if err := candidate.lock(ctx, db.New(pool), workspaceID); err != nil {
				test.Fatalf("uncontended lock: %v", err)
			}
			holder, err := pool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer holder.Rollback(ctx)
			if _, err := holder.Exec(ctx, candidate.hold, candidate.id); err != nil {
				test.Fatal(err)
			}
			bounded, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			tx, err := Begin(bounded, pool, workspaceID)
			if err != nil {
				test.Fatal(err)
			}
			defer tx.Rollback(ctx)
			assertLockRefusal(test, candidate.lock(bounded, db.New(tx), workspaceID))
		})
	}
}

func assertLockRefusal(test *testing.T, err error) {
	test.Helper()
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "55P03" || !IsRetryable(err) {
		test.Fatalf("want immediate retryable lock refusal, got %v", err)
	}
}

func TestDecisionScopeJoinsDoNotLockOwners(test *testing.T) {
	pool, fixture := decisionPool(test)
	ctx := context.Background()
	workspaceID := parseTestUUID(test, fixture.Insert(test, "workspace", testutil.Cols{"id": uuid.NewString()}))
	agentID := parseTestUUID(test, fixture.Insert(test, "agent", testutil.Cols{"workspace_id": workspaceID}))
	taskID := parseTestUUID(test, fixture.Insert(test, "agent_task_queue", testutil.Cols{"agent_id": agentID}))
	sessionID := parseTestUUID(test, fixture.Insert(test, "chat_session", testutil.Cols{"workspace_id": workspaceID}))
	firstMessage := parseTestUUID(test, fixture.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID}))
	secondMessage := parseTestUUID(test, fixture.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID}))
	holder, err := pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer holder.Rollback(ctx)
	for _, statement := range []struct {
		sql string
		id  pgtype.UUID
	}{
		{`SELECT id FROM agent WHERE id = $1 FOR UPDATE`, agentID},
		{`SELECT id FROM chat_session WHERE id = $1 FOR UPDATE`, sessionID},
		{`UPDATE chat_message SET content = 'edited' WHERE id = $1`, firstMessage},
	} {
		if _, err := holder.Exec(ctx, statement.sql, statement.id); err != nil {
			test.Fatal(err)
		}
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	tx, err := Begin(bounded, pool, workspaceID)
	if err != nil {
		test.Fatal(err)
	}
	defer tx.Rollback(ctx)
	queries := db.New(tx)
	if _, err := queries.LockDecisionCallingTask(bounded, db.LockDecisionCallingTaskParams{WorkspaceID: workspaceID, ID: taskID}); err != nil {
		test.Fatalf("scope join locked the owning agent: %v", err)
	}
	rows, err := queries.LockDecisionSourceMessages(bounded, db.LockDecisionSourceMessagesParams{WorkspaceID: workspaceID, MessageIds: []pgtype.UUID{secondMessage, firstMessage}})
	if err != nil || len(rows) != 2 {
		test.Fatalf("scope join locked the session, or KEY SHARE blocked a non-key update: %v, %v", rows, err)
	}
	if bytes.Compare(rows[0].Bytes[:], rows[1].Bytes[:]) >= 0 {
		test.Fatal("source rows were not locked in ascending ID order")
	}
}

func parseTestUUID(test *testing.T, value string) pgtype.UUID {
	test.Helper()
	var result pgtype.UUID
	if err := result.Scan(value); err != nil {
		test.Fatal(err)
	}
	return result
}

func decisionPool(test *testing.T) (*pgxpool.Pool, *testutil.Fixture) {
	test.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		test.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := "decision_tx_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		CREATE TABLE workspace (id uuid PRIMARY KEY);
		CREATE TABLE agent_task_queue (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), agent_id uuid NOT NULL, status text NOT NULL DEFAULT 'running');
		CREATE TABLE agent (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id uuid NOT NULL, status text NOT NULL DEFAULT 'active');
		CREATE TABLE member (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id uuid NOT NULL, user_id uuid NOT NULL);
		CREATE TABLE chat_session (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id uuid NOT NULL);
		CREATE TABLE chat_message (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), chat_session_id uuid NOT NULL, content text NOT NULL DEFAULT 'message');
	`)
	return pool, fixture
}
