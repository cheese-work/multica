package migrations

import (
	"context"
	"encoding/json"
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
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// decision_action is the approve-to-execute payload store: one row per
// decision request, a hash that is always 64 lowercase hex, and a state set
// without a visible "executing". Runs on a migrated database and rolls back.
func TestDecisionActionSchema(test *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		test.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		test.Fatal(err)
	}
	// Each begin() holds its connection until the subtest ends, and "state set" opens seven.
	poolConfig.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)

	const insert = `INSERT INTO decision_action (workspace_id, decision_request_id, action_type, action_version, params, payload_sha256, expires_at, requested_by_agent_id%s)
		VALUES ($1, $2, 'agent.copy', 1, $3::jsonb, $4, now() + interval '24 hours', $5%s)`
	hash := strings.Repeat("a", 64)
	begin := func(test *testing.T) pgx.Tx {
		tx, err := pool.Begin(ctx)
		if err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() { _ = tx.Rollback(ctx) })
		return tx
	}
	exec := func(tx pgx.Tx, request uuid.UUID, params, sha string, extraColumns, extraValues string) error {
		_, err := tx.Exec(ctx, strings.Replace(strings.Replace(insert, "%s", extraColumns, 1), "%s", extraValues, 1), uuid.New(), request, params, sha, uuid.New())
		return err
	}
	expectCode := func(test *testing.T, err error, code string) {
		test.Helper()
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != code {
			test.Fatalf("want SQLSTATE %s, got %v", code, err)
		}
	}

	test.Run("defaults and round trip", func(test *testing.T) {
		tx := begin(test)
		request := uuid.New()
		if err := exec(tx, request, `{"a":1}`, hash, "", ""); err != nil {
			test.Fatal(err)
		}
		var state string
		var slots string
		var doneNull bool
		if err := tx.QueryRow(ctx, `SELECT state, secret_slots::text, post_commit_done_at IS NULL FROM decision_action WHERE decision_request_id = $1`, request).Scan(&state, &slots, &doneNull); err != nil {
			test.Fatal(err)
		}
		if state != "pending" || slots != "[]" || !doneNull {
			test.Fatalf("defaults: state=%q slots=%q done_null=%v", state, slots, doneNull)
		}
	})
	test.Run("one action per request", func(test *testing.T) {
		tx := begin(test)
		request := uuid.New()
		if err := exec(tx, request, `{}`, hash, "", ""); err != nil {
			test.Fatal(err)
		}
		expectCode(test, exec(tx, request, `{}`, hash, "", ""), "23505")
	})
	test.Run("payload hash must be 64 lowercase hex", func(test *testing.T) {
		for name, bad := range map[string]string{"short": "abc", "upper": strings.Repeat("A", 64), "non hex": strings.Repeat("g", 64)} {
			test.Run(name, func(test *testing.T) {
				expectCode(test, exec(begin(test), uuid.New(), `{}`, bad, "", ""), "23514")
			})
		}
	})
	test.Run("state set", func(test *testing.T) {
		for _, state := range []string{"pending", "rejected", "expired", "cancelled"} {
			if err := exec(begin(test), uuid.New(), `{}`, hash, ", state", ", '"+state+"'"); err != nil {
				test.Fatalf("%s: %v", state, err)
			}
		}
		for _, state := range []string{"executing", "approved", ""} {
			expectCode(test, exec(begin(test), uuid.New(), `{}`, hash, ", state", ", '"+state+"'"), "23514")
		}
	})
	test.Run("executed and failed carry executed_at, others do not", func(test *testing.T) {
		for _, state := range []string{"executed", "failed"} {
			expectCode(test, exec(begin(test), uuid.New(), `{}`, hash, ", state", ", '"+state+"'"), "23514")
			if err := exec(begin(test), uuid.New(), `{}`, hash, ", state, executed_at", ", '"+state+"', now()"); err != nil {
				test.Fatalf("%s: %v", state, err)
			}
		}
		expectCode(test, exec(begin(test), uuid.New(), `{}`, hash, ", executed_at", ", now()"), "23514")
	})
	test.Run("params is an object, secret_slots is an array", func(test *testing.T) {
		expectCode(test, exec(begin(test), uuid.New(), `[]`, hash, "", ""), "23514")
		expectCode(test, exec(begin(test), uuid.New(), `{}`, hash, ", secret_slots", ", '{}'::jsonb"), "23514")
	})
	// Added after the queries, so no separate red run: the hash a request is
	// stored with must survive the database round trip (microsecond expiry,
	// jsonb key order and spacing).
	test.Run("stored payload re-hashes to the stored hash", func(test *testing.T) {
		tx := begin(test)
		queries := db.New(tx)
		workspace, request, agent := uuid.New(), uuid.New(), uuid.New()
		normalized, err := decisionaction.Normalize(decisionaction.Request{Type: "agent.env.patch", Version: 1,
			Params:      []byte(`{"agent_id":"` + agent.String() + `","set":{"B":"2","A":"1"}}`),
			SecretSlots: []decisionaction.SecretSlot{{Slot: "K", Target: decisionaction.SlotTarget{AgentID: agent.String(), EnvKey: "K"}}}})
		if err != nil {
			test.Fatal(err)
		}
		slots := []byte(`[{"slot":"K","target":{"agent_id":"` + agent.String() + `","env_key":"K"}}]`)
		payload := decisionaction.Payload{ActionType: "agent.env.patch", ActionVersion: 1, WorkspaceID: workspace.String(), DecisionRequestID: request.String(),
			Params: normalized.Params, Preconditions: []byte(`{"custom_env_version":4}`), SecretSlots: normalized.SecretSlots,
			ExpiresAt: time.Now().Add(24 * time.Hour)}
		sum, err := payload.Hash()
		if err != nil {
			test.Fatal(err)
		}
		created, err := queries.CreateDecisionAction(ctx, db.CreateDecisionActionParams{
			WorkspaceID: pgtype.UUID{Bytes: workspace, Valid: true}, DecisionRequestID: pgtype.UUID{Bytes: request, Valid: true},
			ActionType: payload.ActionType, ActionVersion: 1, Params: payload.Params, Preconditions: payload.Preconditions, SecretSlots: slots,
			PayloadSha256: sum, ExpiresAt: pgtype.Timestamptz{Time: payload.ExpiresAt, Valid: true}, RequestedByAgentID: pgtype.UUID{Bytes: agent, Valid: true}})
		if err != nil {
			test.Fatal(err)
		}
		row, err := queries.GetDecisionActionByRequest(ctx, db.GetDecisionActionByRequestParams{DecisionRequestID: created.DecisionRequestID, WorkspaceID: created.WorkspaceID})
		if err != nil {
			test.Fatal(err)
		}
		var storedSlots []decisionaction.SecretSlot
		if err := json.Unmarshal(row.SecretSlots, &storedSlots); err != nil {
			test.Fatal(err)
		}
		again, err := decisionaction.Payload{ActionType: row.ActionType, ActionVersion: int(row.ActionVersion), WorkspaceID: workspace.String(), DecisionRequestID: request.String(),
			Params: row.Params, Preconditions: row.Preconditions, SecretSlots: storedSlots, ExpiresAt: row.ExpiresAt.Time}.Hash()
		if err != nil {
			test.Fatal(err)
		}
		if again != sum || row.PayloadSha256 != sum || row.State != "pending" {
			test.Fatalf("stored hash %s, rehash %s, payload hash %s, state %s", row.PayloadSha256, again, sum, row.State)
		}
	})
}
