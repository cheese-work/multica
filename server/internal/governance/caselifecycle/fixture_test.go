package caselifecycle

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type lifecycleFixture struct {
	pool        *pgxpool.Pool
	queries     *db.Queries
	workspaceID pgtype.UUID
	caseRow     db.GovernanceCase
	clock       *fakeLifecycleClock
}

func newLifecycleFixture(t *testing.T, state CaseState) *lifecycleFixture {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping test database: %v", err)
	}
	t.Cleanup(pool.Close)
	workspaceID := lifecycleUUID(t)
	clock := &fakeLifecycleClock{current: time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)}
	queries := db.New(pool)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, '{"jev_governance_enabled":true,"rule_mode":"shadow"}'::jsonb)
	`, workspaceID); err != nil {
		t.Fatalf("insert lifecycle fixture control: %v", err)
	}
	caseRow, err := queries.InsertNextGovernanceCase(context.Background(), db.InsertNextGovernanceCaseParams{
		WorkspaceID:         workspaceID,
		SubjectType:         "issue",
		SubjectID:           lifecycleUUID(t),
		SubjectRevision:     1,
		RuleID:              lifecycleUUID(t),
		MaterialFingerprint: "fingerprint-a",
		State:               string(state),
		AuthorityLineage:    []byte("[]"),
		TriggerAliases:      []byte("[]"),
		EvidenceDigest:      "evidence-a",
		RuleRevision:        "rule-1",
		ActivationRevision:  "activation-1",
		ConfigRevision:      "config-1",
		BudgetRootID:        lifecycleUUID(t),
		FrozenStrategy:      []byte(`[{"candidate":"fixed"}]`),
		AbsoluteDeadline:    pgtype.Timestamptz{Time: clock.Now().Add(time.Hour), Valid: true},
		EvidenceEpoch:       3,
		RefreshCount:        2,
		ControlEpoch:        1,
	})
	if err != nil {
		t.Fatalf("insert lifecycle fixture case: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM governance_attempt WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM governance_case_transition WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM governance_case WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM governance_workspace_config_audit WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM governance_workspace_config WHERE workspace_id = $1`, workspaceID)
	})
	return &lifecycleFixture{pool: pool, queries: queries, workspaceID: workspaceID, caseRow: caseRow, clock: clock}
}

func (fixture *lifecycleFixture) service() *Service {
	service, err := NewService(fixture.pool, fixture.clock)
	if err != nil {
		panic(err)
	}
	return service
}

func (fixture *lifecycleFixture) insertAgentAttempt(t *testing.T) db.GovernanceAttempt {
	t.Helper()
	attempt, err := fixture.queries.InsertGovernanceAttempt(context.Background(), db.InsertGovernanceAttemptParams{
		WorkspaceID:  fixture.workspaceID,
		CaseID:       fixture.caseRow.ID,
		Ordinal:      0,
		Kind:         "agent",
		ObligationID: lifecycleUUID(t),
		InputDigest:  "input-a",
		AttemptFence: lifecycleUUID(t),
		DeadlineAt:   pgtype.Timestamptz{Time: fixture.clock.Now().Add(5 * time.Minute), Valid: true},
		Confidence:   []byte("{}"),
		Result:       []byte("{}"),
		Usage:        []byte("{}"),
	})
	if err != nil {
		t.Fatalf("insert lifecycle fixture attempt: %v", err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `UPDATE governance_case SET current_attempt_id = $1 WHERE workspace_id = $2 AND id = $3`, attempt.ID, fixture.workspaceID, fixture.caseRow.ID); err != nil {
		t.Fatalf("bind lifecycle fixture attempt: %v", err)
	}
	return attempt
}

func (fixture *lifecycleFixture) currentCase(t *testing.T) (db.GovernanceCase, error) {
	t.Helper()
	return fixture.queries.FindGovernanceCaseByMaterialFingerprint(context.Background(), db.FindGovernanceCaseByMaterialFingerprintParams{
		WorkspaceID:         fixture.workspaceID,
		SubjectType:         fixture.caseRow.SubjectType,
		SubjectID:           fixture.caseRow.SubjectID,
		SubjectRevision:     fixture.caseRow.SubjectRevision,
		RuleID:              fixture.caseRow.RuleID,
		MaterialFingerprint: fixture.caseRow.MaterialFingerprint,
	})
}

func (fixture *lifecycleFixture) countTransitions(t *testing.T, causeKey string) int {
	t.Helper()
	var count int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_case_transition WHERE workspace_id = $1 AND cause_event_key = $2`, fixture.workspaceID, causeKey).Scan(&count); err != nil {
		t.Fatalf("count lifecycle transitions: %v", err)
	}
	return count
}

type fakeLifecycleClock struct {
	mu      sync.RWMutex
	current time.Time
}

func (clock *fakeLifecycleClock) Now() time.Time {
	clock.mu.RLock()
	defer clock.mu.RUnlock()
	return clock.current
}

func (clock *fakeLifecycleClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.current = clock.current.Add(duration)
}

func lifecycleUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	id := uuid.New()
	return pgtype.UUID{Bytes: [16]byte(id), Valid: true}
}

func bytesEqual(left, right []byte) bool {
	return string(left) == string(right)
}
