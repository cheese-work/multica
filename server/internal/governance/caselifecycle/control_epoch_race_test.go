package caselifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type commitBarrierDatabase struct {
	Database
	entered chan int32
	release chan struct{}
}

type commitBarrierTx struct {
	pgx.Tx
	entered chan<- int32
	release <-chan struct{}
	pid     int32
}

func (database *commitBarrierDatabase) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := database.Database.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	return &commitBarrierTx{Tx: tx, entered: database.entered, release: database.release, pid: pid}, nil
}

type observedBeginDatabase struct {
	Database
	started chan<- int32
}

func (database *observedBeginDatabase) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := database.Database.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	database.started <- pid
	return tx, nil
}

func (tx *commitBarrierTx) Commit(ctx context.Context) error {
	tx.entered <- tx.pid
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tx.release:
		return tx.Tx.Commit(ctx)
	}
}

func TestLifecycleMutationsRaceDisableOnTwoConnections(t *testing.T) {
	tests := []struct {
		name    string
		state   CaseState
		prepare func(*testing.T, *lifecycleFixture) func(*Service) error
		verify  func(*testing.T, *lifecycleFixture)
	}{
		{
			name:  "action claim",
			state: CaseCorrectionPending,
			prepare: func(t *testing.T, fixture *lifecycleFixture) func(*Service) error {
				token := lifecycleUUID(t)
				return func(service *Service) error {
					_, err := service.ClaimLease(context.Background(), LeaseCommand{
						WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
						ControlEpoch:  fixture.caseRow.ControlEpoch,
						ExpectedState: CaseCorrectionPending, ExpectedRevision: 0, Token: token, Duration: time.Minute,
					})
					return err
				}
			},
			verify: func(t *testing.T, fixture *lifecycleFixture) {
				current, err := fixture.currentCase(t)
				if err != nil || !current.LeaseToken.Valid || current.StateRevision != 0 {
					t.Fatalf("claimed case = lease:%v revision:%d err:%v", current.LeaseToken.Valid, current.StateRevision, err)
				}
			},
		},
		{
			name:  "lease release",
			state: CaseCorrectionPending,
			prepare: func(t *testing.T, fixture *lifecycleFixture) func(*Service) error {
				token := lifecycleUUID(t)
				if _, err := fixture.service().ClaimLease(context.Background(), LeaseCommand{
					WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
					ControlEpoch:  fixture.caseRow.ControlEpoch,
					ExpectedState: CaseCorrectionPending, ExpectedRevision: 0, Token: token, Duration: time.Minute,
				}); err != nil {
					t.Fatal(err)
				}
				return func(service *Service) error {
					_, err := service.ReleaseLease(context.Background(), fixture.workspaceID, fixture.caseRow.ID, token, fixture.caseRow.ControlEpoch)
					return err
				}
			},
			verify: func(t *testing.T, fixture *lifecycleFixture) {
				current, err := fixture.currentCase(t)
				if err != nil || current.LeaseToken.Valid {
					t.Fatalf("released case lease valid=%v err=%v", current.LeaseToken.Valid, err)
				}
			},
		},
		{
			name:  "queue mutation",
			state: CaseCaptured,
			prepare: func(_ *testing.T, fixture *lifecycleFixture) func(*Service) error {
				return func(service *Service) error {
					_, err := service.Transition(context.Background(), TransitionCommand{
						WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
						ControlEpoch:  fixture.caseRow.ControlEpoch,
						ExpectedState: CaseCaptured, ExpectedRevision: 0, NextState: CaseEvidenceReady,
						CauseEventKey: "race-queue-transition", Actor: ActorSystem, Reason: ReasonEvidenceCaptured,
					})
					return err
				}
			},
			verify: func(t *testing.T, fixture *lifecycleFixture) {
				current, err := fixture.currentCase(t)
				if err != nil || current.State != string(CaseEvidenceReady) || current.StateRevision != 1 || fixture.countTransitions(t, "race-queue-transition") != 1 {
					t.Fatalf("queue mutation state=%s revision=%d err=%v", current.State, current.StateRevision, err)
				}
			},
		},
		{
			name:  "correction mutation",
			state: CaseJevEvaluating,
			prepare: func(t *testing.T, fixture *lifecycleFixture) func(*Service) error {
				attempt := insertJevAttempt(t, fixture)
				token := lifecycleUUID(t)
				if _, err := fixture.service().ClaimLease(context.Background(), LeaseCommand{
					WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
					ControlEpoch:  fixture.caseRow.ControlEpoch,
					ExpectedState: CaseJevEvaluating, ExpectedRevision: 0, Token: token, Duration: time.Minute,
				}); err != nil {
					t.Fatal(err)
				}
				return func(service *Service) error {
					_, err := service.Transition(context.Background(), TransitionCommand{
						WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
						ControlEpoch:  fixture.caseRow.ControlEpoch,
						ExpectedState: CaseJevEvaluating, ExpectedRevision: 0, NextState: CaseCorrectionPending,
						CauseEventKey: "race-correction-transition", Actor: ActorSystem, Reason: ReasonQualifiedProposal,
						LeaseToken: token, AttemptID: attempt.ID, AttemptFence: attempt.AttemptFence,
					})
					return err
				}
			},
			verify: func(t *testing.T, fixture *lifecycleFixture) {
				current, err := fixture.currentCase(t)
				if err != nil || current.State != string(CaseCorrectionPending) || current.StateRevision != 1 || fixture.countTransitions(t, "race-correction-transition") != 1 {
					t.Fatalf("correction mutation state=%s revision=%d err=%v", current.State, current.StateRevision, err)
				}
			},
		},
		{
			name:  "successor insertion",
			state: CaseCaptured,
			prepare: func(_ *testing.T, fixture *lifecycleFixture) func(*Service) error {
				next := newLifecycleCaseParams(fixture, "race-successor", fixture.caseRow.ControlEpoch)
				next.SubjectRevision = fixture.caseRow.SubjectRevision + 1
				return func(service *Service) error {
					_, err := service.CreateSuccessor(context.Background(), SuccessorCommand{
						WorkspaceID: fixture.workspaceID, PredecessorID: fixture.caseRow.ID,
						ExpectedState: CaseCaptured, ExpectedRevision: 0, CauseEventKey: "race-successor-invalidation",
						Actor: ActorSystem, Successor: next,
					})
					return err
				}
			},
			verify: func(t *testing.T, fixture *lifecycleFixture) {
				var count int
				if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_case WHERE workspace_id = $1 AND material_fingerprint = 'race-successor' AND control_epoch = 1`, fixture.workspaceID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("successor rows=%d err=%v", count, err)
				}
				current, err := fixture.currentCase(t)
				if err != nil || current.State != string(CaseInvalidated) || current.StateRevision != 1 {
					t.Fatalf("predecessor state=%s revision=%d err=%v", current.State, current.StateRevision, err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLifecycleFixture(t, test.state)
			write := test.prepare(t, fixture)
			runLifecycleDisableRace(t, fixture, write, func() { test.verify(t, fixture) })
		})
		t.Run(test.name+" / disable wins", func(t *testing.T) {
			fixture := newLifecycleFixture(t, test.state)
			write := test.prepare(t, fixture)
			before := snapshotLifecycleWrite(t, fixture)
			runLifecycleDisableFirstRace(t, fixture, write)
			assertLifecycleWriteSnapshot(t, fixture, before)
		})
	}
}

func runLifecycleDisableRace(t *testing.T, fixture *lifecycleFixture, write func(*Service) error, verify func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	disableConn, err := pgx.Connect(ctx, databaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer disableConn.Close(context.Background())
	var disablePID int32
	if err := disableConn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&disablePID); err != nil {
		t.Fatal(err)
	}
	barrier := &commitBarrierDatabase{Database: fixture.pool, entered: make(chan int32, 1), release: make(chan struct{})}
	var releaseOnce sync.Once
	releaseWriter := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	t.Cleanup(releaseWriter)
	service, err := NewService(barrier, fixture.clock)
	if err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- write(service) }()
	var writerPID int32
	select {
	case writerPID = <-barrier.entered:
	case err := <-writeDone:
		t.Fatalf("mutation returned before commit barrier: %v", err)
	case <-ctx.Done():
		t.Fatalf("mutation did not reach commit barrier: %v", ctx.Err())
	}
	if writerPID == disablePID {
		t.Fatalf("writer and disable share backend pid %d", writerPID)
	}
	disableDone := startLifecycleDisable(t, ctx, disableConn, fixture.workspaceID)
	waitForBackendLockWait(t, ctx, fixture, disablePID)
	select {
	case err := <-disableDone:
		t.Fatalf("disable acknowledged while C05 mutation held control lock: %v", err)
	default:
	}
	releaseWriter()
	if err := <-writeDone; err != nil {
		t.Fatalf("C05 mutation before disable: %v", err)
	}
	if err := <-disableDone; err != nil {
		t.Fatalf("disable after C05 mutation: %v", err)
	}
	verify()
}

func runLifecycleDisableFirstRace(t *testing.T, fixture *lifecycleFixture, write func(*Service) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	disableConn, err := pgx.Connect(ctx, databaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer disableConn.Close(context.Background())
	disableTx, err := disableConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = disableTx.Rollback(context.Background())
		}
	})
	result, err := disableTx.Exec(ctx, `
		UPDATE governance_workspace_config
		SET config_version = config_version + 1,
		    control_epoch = control_epoch + 1,
		    settings = jsonb_set(settings, '{jev_governance_enabled}', 'false'::jsonb, true),
		    updated_at = now()
		WHERE workspace_id = $1
	`, fixture.workspaceID)
	if err != nil {
		t.Fatalf("hold disable control lock: %v", err)
	}
	if result.RowsAffected() != 1 {
		t.Fatalf("disable updated %d control rows, want 1", result.RowsAffected())
	}
	var disablePID int32
	if err := disableConn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&disablePID); err != nil {
		t.Fatal(err)
	}
	started := make(chan int32, 1)
	database := &observedBeginDatabase{Database: fixture.pool, started: started}
	service, err := NewService(database, fixture.clock)
	if err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- write(service) }()
	var writerPID int32
	select {
	case writerPID = <-started:
	case err := <-writeDone:
		t.Fatalf("mutation returned before waiting on the disable lock: %v", err)
	case <-ctx.Done():
		t.Fatalf("mutation did not begin before timeout: %v", ctx.Err())
	}
	if writerPID == disablePID {
		t.Fatalf("writer and disable share backend pid %d", writerPID)
	}
	waitForBackendLockWait(t, ctx, fixture, writerPID)
	if err := disableTx.Commit(ctx); err != nil {
		t.Fatalf("commit disable before stale mutation: %v", err)
	}
	committed = true
	if err := <-writeDone; !errors.Is(err, ErrStaleControlEpoch) {
		t.Fatalf("stale C05 mutation after disable = %v, want ErrStaleControlEpoch", err)
	}
	var enabled bool
	var epoch int64
	if err := fixture.pool.QueryRow(ctx, `SELECT settings->>'jev_governance_enabled' = 'true', control_epoch FROM governance_workspace_config WHERE workspace_id = $1`, fixture.workspaceID).Scan(&enabled, &epoch); err != nil {
		t.Fatal(err)
	}
	if enabled || epoch != 2 {
		t.Fatalf("control after disable = enabled:%v epoch:%d, want false/2", enabled, epoch)
	}
}

type lifecycleWriteSnapshot struct {
	state            string
	revision         int64
	leaseToken       pgtype.UUID
	leaseExpiresAt   pgtype.Timestamptz
	currentAttemptID pgtype.UUID
	cases            int
	transitions      int
	attempts         int
}

func snapshotLifecycleWrite(t *testing.T, fixture *lifecycleFixture) lifecycleWriteSnapshot {
	t.Helper()
	var snapshot lifecycleWriteSnapshot
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT state, state_revision, lease_token, lease_expires_at, current_attempt_id
		FROM governance_case WHERE workspace_id = $1 AND id = $2
	`, fixture.workspaceID, fixture.caseRow.ID).Scan(
		&snapshot.state, &snapshot.revision, &snapshot.leaseToken, &snapshot.leaseExpiresAt, &snapshot.currentAttemptID,
	); err != nil {
		t.Fatalf("snapshot lifecycle case: %v", err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_case WHERE workspace_id = $1`, fixture.workspaceID).Scan(&snapshot.cases); err != nil {
		t.Fatalf("count lifecycle cases: %v", err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_case_transition WHERE workspace_id = $1`, fixture.workspaceID).Scan(&snapshot.transitions); err != nil {
		t.Fatalf("count lifecycle transitions: %v", err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_attempt WHERE workspace_id = $1`, fixture.workspaceID).Scan(&snapshot.attempts); err != nil {
		t.Fatalf("count lifecycle attempts: %v", err)
	}
	return snapshot
}

func assertLifecycleWriteSnapshot(t *testing.T, fixture *lifecycleFixture, want lifecycleWriteSnapshot) {
	t.Helper()
	got := snapshotLifecycleWrite(t, fixture)
	if got.state != want.state || got.revision != want.revision || got.leaseToken != want.leaseToken ||
		got.leaseExpiresAt.Valid != want.leaseExpiresAt.Valid ||
		got.leaseExpiresAt.Valid && !got.leaseExpiresAt.Time.Equal(want.leaseExpiresAt.Time) ||
		got.currentAttemptID != want.currentAttemptID || got.cases != want.cases ||
		got.transitions != want.transitions || got.attempts != want.attempts {
		t.Fatalf("stale mutation changed persisted lifecycle state: got %#v, want %#v", got, want)
	}
}

func TestNewCaseUsesCapturedControlEpoch(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseCaptured)
	params := newLifecycleCaseParams(fixture, "fresh-current-epoch", 1)
	created, err := fixture.queries.CreateOrResolveGovernanceCase(context.Background(), params)
	if err != nil {
		t.Fatalf("create with captured epoch: %v", err)
	}
	if created.ControlEpoch != 1 {
		t.Fatalf("created control epoch = %d, want 1", created.ControlEpoch)
	}
	duplicate, err := fixture.queries.CreateOrResolveGovernanceCase(context.Background(), params)
	if err != nil {
		t.Fatalf("resolve duplicate with current epoch: %v", err)
	}
	if duplicate.ID != created.ID {
		t.Fatalf("duplicate case id = %s, want %s", duplicate.ID, created.ID)
	}

	setLifecycleControl(t, fixture, false, "shadow")
	setLifecycleControl(t, fixture, true, "shadow")
	staleAfterReenable := newLifecycleCaseParams(fixture, "stale-after-reenable", 1)
	if _, err := fixture.queries.CreateOrResolveGovernanceCase(context.Background(), staleAfterReenable); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("create with pre-disable epoch after re-enable = %v, want pgx.ErrNoRows", err)
	}

	currentEpoch := newLifecycleCaseParams(fixture, "fresh-after-reenable", 3)
	current, err := fixture.queries.CreateOrResolveGovernanceCase(context.Background(), currentEpoch)
	if err != nil {
		t.Fatalf("create with re-enabled epoch: %v", err)
	}
	setLifecycleControl(t, fixture, true, "correction")
	staleAfterConfigChange := newLifecycleCaseParams(fixture, "stale-after-config-change", 3)
	if _, err := fixture.queries.CreateOrResolveGovernanceCase(context.Background(), staleAfterConfigChange); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("create with pre-configuration epoch = %v, want pgx.ErrNoRows", err)
	}
	if _, err := fixture.queries.InsertNextGovernanceCase(context.Background(), staleAfterConfigChange); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("direct insert with pre-configuration epoch = %v, want pgx.ErrNoRows", err)
	}
	count := countGovernanceCases(t, fixture)
	if count != 3 {
		t.Fatalf("case rows = %d, want 3 (fixture, first fresh, re-enabled fresh)", count)
	}
	if current.ControlEpoch != 3 {
		t.Fatalf("re-enabled case epoch = %d, want 3", current.ControlEpoch)
	}
}

func TestNewCaseInsertRacesDisableOnTwoConnections(t *testing.T) {
	t.Run("insert holds control guard first", func(t *testing.T) {
		fixture := newLifecycleFixture(t, CaseCaptured)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		disableConn, err := pgx.Connect(ctx, databaseURL(t))
		if err != nil {
			t.Fatal(err)
		}
		defer disableConn.Close(context.Background())
		writerTx, err := fixture.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer writerTx.Rollback(context.Background())
		var writerPID, disablePID int32
		if err := writerTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
			t.Fatal(err)
		}
		if err := disableConn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&disablePID); err != nil {
			t.Fatal(err)
		}
		if writerPID == disablePID {
			t.Fatalf("writer and disable share backend pid %d", writerPID)
		}
		params := newLifecycleCaseParams(fixture, "writer-before-disable", 1)
		created, err := db.New(writerTx).InsertNextGovernanceCase(ctx, params)
		if err != nil {
			t.Fatalf("insert in writer transaction: %v", err)
		}
		disableDone := startLifecycleDisable(t, ctx, disableConn, fixture.workspaceID)
		waitForBackendLockWait(t, ctx, fixture, disablePID)
		if got := countGovernanceCases(t, fixture); got != 1 {
			t.Fatalf("visible case rows before writer commit = %d, want only fixture row", got)
		}
		if err := writerTx.Commit(ctx); err != nil {
			t.Fatalf("commit new-case insert: %v", err)
		}
		if err := <-disableDone; err != nil {
			t.Fatalf("disable after writer commit: %v", err)
		}
		stored, err := fixture.queries.FindGovernanceCaseByMaterialFingerprint(ctx, db.FindGovernanceCaseByMaterialFingerprintParams{
			WorkspaceID: fixture.workspaceID, SubjectType: params.SubjectType, SubjectID: params.SubjectID,
			SubjectRevision: params.SubjectRevision, RuleID: params.RuleID, MaterialFingerprint: params.MaterialFingerprint,
		})
		if err != nil {
			t.Fatalf("read inserted case after disable acknowledgement: %v", err)
		}
		if stored.ID != created.ID || stored.ControlEpoch != 1 {
			t.Fatalf("stored case id/epoch = %s/%d, want %s/1", stored.ID, stored.ControlEpoch, created.ID)
		}
	})

	t.Run("disable commits before stale insert", func(t *testing.T) {
		fixture := newLifecycleFixture(t, CaseCaptured)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		disableConn, err := pgx.Connect(ctx, databaseURL(t))
		if err != nil {
			t.Fatal(err)
		}
		defer disableConn.Close(context.Background())
		writerConn, err := pgx.Connect(ctx, databaseURL(t))
		if err != nil {
			t.Fatal(err)
		}
		defer writerConn.Close(context.Background())
		disableTx, err := disableConn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		committed := false
		t.Cleanup(func() {
			if !committed {
				_ = disableTx.Rollback(context.Background())
			}
		})
		if _, err := disableTx.Exec(ctx, `
			UPDATE governance_workspace_config
			SET config_version = config_version + 1,
			    control_epoch = control_epoch + 1,
			    settings = jsonb_set(settings, '{jev_governance_enabled}', 'false'::jsonb, true),
			    updated_at = now()
			WHERE workspace_id = $1
		`, fixture.workspaceID); err != nil {
			t.Fatal(err)
		}
		writerTx, err := writerConn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer writerTx.Rollback(context.Background())
		var writerPID, disablePID int32
		if err := writerTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
			t.Fatal(err)
		}
		if err := disableConn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&disablePID); err != nil {
			t.Fatal(err)
		}
		if writerPID == disablePID {
			t.Fatalf("writer and disable share backend pid %d", writerPID)
		}
		params := newLifecycleCaseParams(fixture, "stale-after-disable", 1)
		insertDone := make(chan error, 1)
		go func() {
			_, insertErr := db.New(writerTx).InsertNextGovernanceCase(ctx, params)
			insertDone <- insertErr
		}()
		waitForBackendLockWait(t, ctx, fixture, writerPID)
		if err := disableTx.Commit(ctx); err != nil {
			t.Fatalf("commit disable before stale insert: %v", err)
		}
		committed = true
		if err := <-insertDone; !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("stale insert after disable = %v, want pgx.ErrNoRows", err)
		}
		if got := countGovernanceCases(t, fixture); got != 1 {
			t.Fatalf("case rows after stale insert = %d, want fixture row only", got)
		}
	})
}

func newLifecycleCaseParams(fixture *lifecycleFixture, fingerprint string, controlEpoch int64) db.InsertNextGovernanceCaseParams {
	return db.InsertNextGovernanceCaseParams{
		WorkspaceID: fixture.workspaceID, SubjectType: fixture.caseRow.SubjectType,
		SubjectID: fixture.caseRow.SubjectID, SubjectRevision: fixture.caseRow.SubjectRevision + 1,
		RuleID: fixture.caseRow.RuleID, MaterialFingerprint: fingerprint, State: string(CaseCaptured),
		AuthorityLineage: []byte("[]"), TriggerAliases: []byte("[]"), EvidenceDigest: fingerprint,
		RuleRevision: "rule-next", ActivationRevision: "activation-next", ConfigRevision: "config-next",
		BudgetRootID: fixture.caseRow.BudgetRootID, FrozenStrategy: []byte("[]"), ControlEpoch: controlEpoch,
	}
}

func setLifecycleControl(t *testing.T, fixture *lifecycleFixture, enabled bool, ruleMode string) {
	t.Helper()
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE governance_workspace_config
		SET config_version = config_version + 1,
		    control_epoch = control_epoch + 1,
		    settings = jsonb_set(jsonb_set(settings, '{jev_governance_enabled}', to_jsonb($2::boolean), true), '{rule_mode}', to_jsonb($3::text), true)
		WHERE workspace_id = $1
	`, fixture.workspaceID, enabled, ruleMode); err != nil {
		t.Fatalf("update lifecycle control: %v", err)
	}
}

func countGovernanceCases(t *testing.T, fixture *lifecycleFixture) int {
	t.Helper()
	var count int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_case WHERE workspace_id = $1`, fixture.workspaceID).Scan(&count); err != nil {
		t.Fatalf("count governance cases: %v", err)
	}
	return count
}

func databaseURL(t *testing.T) string {
	t.Helper()
	value := os.Getenv("DATABASE_URL")
	if value == "" {
		t.Skip("DATABASE_URL is not set")
	}
	return value
}

func startLifecycleDisable(t *testing.T, ctx context.Context, conn *pgx.Conn, workspaceID pgtype.UUID) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		tx, err := conn.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		result, err := tx.Exec(ctx, `
			UPDATE governance_workspace_config
			SET config_version = config_version + 1,
			    control_epoch = control_epoch + 1,
			    settings = jsonb_set(settings, '{jev_governance_enabled}', 'false'::jsonb, true),
			    updated_at = now()
			WHERE workspace_id = $1
		`, workspaceID)
		if err != nil {
			_ = tx.Rollback(context.Background())
			done <- err
			return
		}
		if result.RowsAffected() != 1 {
			_ = tx.Rollback(context.Background())
			done <- fmt.Errorf("disable updated %d control rows, want 1", result.RowsAffected())
			return
		}
		done <- tx.Commit(ctx)
	}()
	return done
}

func waitForBackendLockWait(t *testing.T, ctx context.Context, fixture *lifecycleFixture, pid int32) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := fixture.pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1)) > 0`, pid).Scan(&blocked); err != nil {
			t.Fatalf("observe postgres lock wait for pid %d: %v", pid, err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("pid %d did not enter a PostgreSQL lock wait: %v", pid, ctx.Err())
		case <-ticker.C:
		}
	}
}
