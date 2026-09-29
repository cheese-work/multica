package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/governance/caselifecycle"
)

// Budget recovery (CHE-883 / B2) test support. Fault injection uses a
// test-owned AFTER INSERT trigger on governance_budget_journal that waits on a
// per-(workspace,event type) advisory lock. Holding that lock parks a
// transaction after all of its local writes but before commit, so the test can
// kill the backend or cancel the context at a deterministic point and then
// prove nothing partial survived. The trigger is installed and dropped per test.

var errRecoveryCrash = errors.New("simulated process crash")
var errRecoveryUncertain = errors.New("native admission outcome is uncertain")

type recoveryCrashMode string

const (
	recoveryBackendLoss  recoveryCrashMode = "backend-loss"
	recoveryContextAbort recoveryCrashMode = "context-abort"
)

func installRecoveryFault(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE OR REPLACE FUNCTION governance_recovery_fault() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(hashtextextended(NEW.workspace_id::text || ':' || NEW.event_type, 0));
			RETURN NULL;
		END $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS governance_recovery_fault_trg ON governance_budget_journal`,
		`CREATE TRIGGER governance_recovery_fault_trg AFTER INSERT ON governance_budget_journal
			FOR EACH ROW EXECUTE FUNCTION governance_recovery_fault()`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("install recovery fault trigger: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, statement := range []string{
			`DROP TRIGGER IF EXISTS governance_recovery_fault_trg ON governance_budget_journal`,
			`DROP FUNCTION IF EXISTS governance_recovery_fault()`,
		} {
			if _, err := pool.Exec(context.Background(), statement); err != nil {
				t.Errorf("remove recovery fault trigger: %v", err)
			}
		}
	})
}

type recoveryCrashReceipt struct {
	Boundary       string            `json:"boundary"`
	EventType      string            `json:"event_type"`
	Mode           recoveryCrashMode `json:"mode"`
	HolderBackend  int32             `json:"holder_backend_pid"`
	BlockedBackend int32             `json:"blocked_backend_pid"`
	OperationError string            `json:"operation_error"`
}

// crashAtJournalInsert runs op until its journal insert of eventType parks on
// the fault lock, kills it per mode, and returns op's error. The caller then
// asserts the rolled-back state.
func crashAtJournalInsert(t *testing.T, pool *pgxpool.Pool, workspaceID pgtype.UUID, eventType, boundary string, mode recoveryCrashMode, op func(context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lockKey := workspaceID.String() + ":" + eventType
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		holder.Release()
		t.Fatal(err)
	}
	unlocked := false
	unlock := func() {
		if unlocked {
			return
		}
		unlocked = true
		if _, err := holder.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lockKey); err != nil {
			t.Errorf("release recovery fault lock: %v", err)
		}
		holder.Release()
	}
	defer unlock()
	holderPID := int32(holder.Conn().PgConn().PID())
	opCtx, opCancel := context.WithCancel(ctx)
	defer opCancel()
	done := make(chan error, 1)
	go func() { done <- op(opCtx) }()
	blocked := recoveryWaitBlockedBy(t, pool, holderPID)
	switch mode {
	case recoveryBackendLoss:
		var terminated bool
		if err := pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, blocked).Scan(&terminated); err != nil || !terminated {
			t.Fatalf("terminate backend %d = %v, %v", blocked, terminated, err)
		}
	case recoveryContextAbort:
		opCancel()
	default:
		t.Fatalf("unknown crash mode %q", mode)
	}
	var opErr error
	select {
	case opErr = <-done:
	case <-ctx.Done():
		t.Fatalf("crashed operation did not return: %v", ctx.Err())
	}
	if opErr == nil {
		t.Fatalf("operation survived %s at %s", mode, boundary)
	}
	unlock()
	receipt, _ := json.Marshal(recoveryCrashReceipt{
		Boundary: boundary, EventType: eventType, Mode: mode,
		HolderBackend: holderPID, BlockedBackend: blocked, OperationError: opErr.Error(),
	})
	t.Logf("recovery-crash-receipt %s", receipt)
	return opErr
}

func recoveryWaitBlockedBy(t *testing.T, pool *pgxpool.Pool, holderPID int32) int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid int32
		err := pool.QueryRow(ctx, `
			SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
				AND $1::integer = ANY(pg_blocking_pids(pid))
			LIMIT 1
		`, holderPID).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("look for blocked backend: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no backend blocked behind %d: %v", holderPID, ctx.Err())
		case <-ticker.C:
		}
	}
}

// holdWorkspaceControl locks the workspace control row so the next composed
// operation parks in LockConcurrencyResources; the returned func releases it.
func holdWorkspaceControl(t *testing.T, pool *pgxpool.Pool, workspaceID pgtype.UUID) (int32, func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var epoch int64
	if err := tx.QueryRow(ctx, `SELECT control_epoch FROM governance_workspace_config WHERE workspace_id = $1 FOR UPDATE`, workspaceID).Scan(&epoch); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("release workspace control lock: %v", err)
		}
	}
	t.Cleanup(release)
	return int32(tx.Conn().PgConn().PID()), release
}

// recoveryBroker is the fake native broker: it keys admissions on the native
// obligation identity and wire sends on the debit event key, so replays are
// observable as counts instead of side effects.
type recoveryBroker struct {
	mu         sync.Mutex
	admissions map[string]int
	wires      map[string]int
	uncertain  bool
}

func newRecoveryBroker() *recoveryBroker {
	return &recoveryBroker{admissions: map[string]int{}, wires: map[string]int{}}
}

func (broker *recoveryBroker) Admit(key string) (bool, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	duplicate := broker.admissions[key] > 0
	if !duplicate {
		broker.admissions[key]++
	}
	if broker.uncertain {
		return duplicate, errRecoveryUncertain
	}
	return duplicate, nil
}

func (broker *recoveryBroker) Wire(key string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.wires[key]++
}

func (broker *recoveryBroker) admissionCount(key string) int {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return broker.admissions[key]
}

func (broker *recoveryBroker) wireCount(key string) int {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return broker.wires[key]
}

// recoveryDispatcher persists the worst-case debit before it lets a wire
// attempt reach the broker, the ordering the persisted contract requires.
type recoveryDispatcher struct {
	pool             *pgxpool.Pool
	budget           *BudgetService
	broker           *recoveryBroker
	workspaceID      pgtype.UUID
	reservationID    pgtype.UUID
	crashBeforeDebit bool
	crashAfterDebit  bool
}

func (dispatcher *recoveryDispatcher) Send(ctx context.Context, wireKey string, amount int64) error {
	var revision int64
	if err := dispatcher.pool.QueryRow(ctx, `
		SELECT revision FROM governance_budget_reservation WHERE workspace_id = $1 AND reservation_id = $2
	`, dispatcher.workspaceID, dispatcher.reservationID).Scan(&revision); err != nil {
		return err
	}
	if dispatcher.crashBeforeDebit {
		return errRecoveryCrash
	}
	if _, err := dispatcher.budget.Debit(ctx, BudgetDebitCommand{
		WorkspaceID: dispatcher.workspaceID, ReservationID: dispatcher.reservationID,
		ExpectedRevision: revision, EventKey: wireKey, AmountMicroUSD: amount,
	}); err != nil {
		return err
	}
	if dispatcher.crashAfterDebit {
		return errRecoveryCrash
	}
	dispatcher.broker.Wire(wireKey)
	return nil
}

// recoveryOutboxAdapter replays the persisted admit intent to the broker using
// the native obligation identity as the idempotency key. It only consumes
// existing outbox rows; there is no queue or lease logic here.
type recoveryOutboxAdapter struct {
	pool   *pgxpool.Pool
	broker *recoveryBroker
}

func (adapter *recoveryOutboxAdapter) Deliver(ctx context.Context, workspaceID, reservationID pgtype.UUID, crashAfterNative bool) error {
	var obligationID pgtype.UUID
	err := adapter.pool.QueryRow(ctx, `
		UPDATE governance_budget_outbox SET state = 'claimed'
		WHERE workspace_id = $1 AND reservation_id = $2 AND event_type = 'admit' AND state IN ('pending', 'claimed')
		RETURNING obligation_id
	`, workspaceID, reservationID).Scan(&obligationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := adapter.broker.Admit(obligationID.String()); err != nil {
		return err
	}
	if crashAfterNative {
		return errRecoveryCrash
	}
	_, err = adapter.pool.Exec(ctx, `
		UPDATE governance_budget_outbox SET state = 'delivered'
		WHERE workspace_id = $1 AND reservation_id = $2 AND event_type = 'admit'
	`, workspaceID, reservationID)
	return err
}

// Fresh-process worker. The parent re-executes this test binary; the child
// opens its own pool, so nothing in memory survives the "crash".

const recoveryHelperRequestEnv = "BUDGET_RECOVERY_HELPER_REQUEST"

type recoveryRequest struct {
	Action string               `json:"action"`
	Clock  time.Time            `json:"clock"`
	Admit  *AdmissionCommand    `json:"admit,omitempty"`
	Debit  *BudgetDebitCommand  `json:"debit,omitempty"`
	Settle *BudgetSettleCommand `json:"settle,omitempty"`
}

type recoveryResponse struct {
	ProcessPID    int    `json:"process_pid"`
	BackendPID    int32  `json:"backend_pid"`
	Duplicate     bool   `json:"duplicate"`
	Finalized     bool   `json:"finalized"`
	OutboxEventID string `json:"outbox_event_id"`
	Revision      int64  `json:"revision"`
	State         string `json:"state"`
	ErrorKind     string `json:"error_kind"`
	Error         string `json:"error"`
}

var recoveryErrors = map[string]error{
	"ErrBudgetLimit":          ErrBudgetLimit,
	"ErrBudgetWindow":         ErrBudgetWindow,
	"ErrBudgetConflict":       ErrBudgetConflict,
	"ErrBudgetRevision":       ErrBudgetRevision,
	"ErrConcurrencyLimit":     ErrConcurrencyLimit,
	"ErrStaleFence":           caselifecycle.ErrStaleFence,
	"ErrStaleCase":            caselifecycle.ErrStaleCase,
	"ErrStaleControlEpoch":    caselifecycle.ErrStaleControlEpoch,
	"ErrBudgetWindowConflict": ErrBudgetWindowConflict,
}

func recoveryErrorKind(err error) string {
	for name, target := range recoveryErrors {
		if errors.Is(err, target) {
			return name
		}
	}
	return "other"
}

func TestHelperBudgetRecoveryProcess(t *testing.T) {
	raw := os.Getenv(recoveryHelperRequestEnv)
	if raw == "" {
		return
	}
	var request recoveryRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	response := recoveryResponse{ProcessPID: os.Getpid()}
	if err := pool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&response.BackendPID); err != nil {
		t.Fatal(err)
	}
	clock := &budgetTestClock{current: request.Clock}
	switch request.Action {
	case "admit":
		service, serviceErr := NewAdmissionService(pool, clock)
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		result, opErr := service.Admit(ctx, *request.Admit)
		err = opErr
		response.Duplicate = result.Duplicate
		response.OutboxEventID = result.OutboxEventID.String()
		response.Revision, response.State = result.Reservation.Revision, result.Reservation.State
	case "debit":
		service, serviceErr := NewBudgetService(pool, clock)
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		result, opErr := service.Debit(ctx, *request.Debit)
		err = opErr
		response.Duplicate = result.Duplicate
		response.Revision, response.State = result.Reservation.Revision, result.Reservation.State
	case "settle":
		service, serviceErr := NewBudgetService(pool, clock)
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		result, opErr := service.Settle(ctx, *request.Settle)
		err = opErr
		response.Duplicate, response.Finalized = result.Duplicate, result.Finalized
		response.Revision, response.State = result.Reservation.Revision, result.Reservation.State
	default:
		t.Fatalf("unknown recovery action %q", request.Action)
	}
	if err != nil {
		response.Error, response.ErrorKind = err.Error(), recoveryErrorKind(err)
	}
	encoded, _ := json.Marshal(response)
	fmt.Printf("BUDGET_RECOVERY_RESULT %s\n", encoded)
}

// runRecoveryProcess runs one request in a fresh subprocess and returns its
// response. The child's PID must differ from the parent's.
func runRecoveryProcess(t *testing.T, request recoveryRequest) recoveryResponse {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestHelperBudgetRecoveryProcess$", "-test.count=1")
	command.Env = append(os.Environ(), recoveryHelperRequestEnv+"="+string(encoded))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("recovery subprocess failed: %v\n%s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		payload, found := strings.CutPrefix(line, "BUDGET_RECOVERY_RESULT ")
		if !found {
			continue
		}
		var response recoveryResponse
		if err := json.Unmarshal([]byte(payload), &response); err != nil {
			t.Fatal(err)
		}
		if response.ProcessPID == os.Getpid() {
			t.Fatal("recovery subprocess reused the parent process")
		}
		t.Logf("recovery-process-receipt action=%s pid=%d backend=%d duplicate=%t error=%q", request.Action, response.ProcessPID, response.BackendPID, response.Duplicate, response.ErrorKind)
		return response
	}
	t.Fatalf("recovery subprocess produced no result:\n%s", output)
	return recoveryResponse{}
}

func requireRecoveryError(t *testing.T, response recoveryResponse, want error) {
	t.Helper()
	if response.ErrorKind == "" || !errors.Is(recoveryErrors[response.ErrorKind], want) {
		t.Fatalf("subprocess error = %q (%s), want %v", response.Error, response.ErrorKind, want)
	}
}

// assertRecoveryConservation proves, for the whole workspace, that counters
// equal the persisted reservations, holds equal unresolved executions, journal
// revisions are gap-free and monotonic, and attempt/obligation ids are unique.
func assertRecoveryConservation(t *testing.T, pool *pgxpool.Pool, workspaceID pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	checks := []struct {
		name  string
		query string
	}{
		{"root counters equal reservations", `
			SELECT count(*) FROM governance_budget_root r
			WHERE r.workspace_id = $1 AND (
				r.reserved_micro_usd <> COALESCE((SELECT sum(remaining_micro_usd) FROM governance_budget_reservation x
					WHERE x.workspace_id = r.workspace_id AND x.budget_root_id = r.budget_root_id AND x.state = 'reserved'), 0)
				OR r.spent_micro_usd <> COALESCE((SELECT sum(debited_micro_usd) FROM governance_budget_reservation x
					WHERE x.workspace_id = r.workspace_id AND x.budget_root_id = r.budget_root_id), 0))`},
		{"window counters equal reservations", `
			SELECT count(*) FROM governance_budget_window w
			WHERE w.workspace_id = $1 AND (
				w.reserved_micro_usd <> COALESCE((SELECT sum(remaining_micro_usd) FROM governance_budget_reservation x
					WHERE x.workspace_id = w.workspace_id AND x.window_start = w.window_start AND x.state = 'reserved'), 0)
				OR w.spent_micro_usd <> COALESCE((SELECT sum(debited_micro_usd) FROM governance_budget_reservation x
					WHERE x.workspace_id = w.workspace_id AND x.window_start = w.window_start), 0))`},
		{"held holds equal unresolved reservations", `
			SELECT abs((SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND state = 'held')
				- (SELECT count(*) FROM governance_budget_reservation WHERE workspace_id = $1 AND state = 'reserved'))`},
		{"guard slots equal held holds", `
			SELECT count(*) FROM governance_concurrency_guard g
			WHERE g.workspace_id = $1 AND g.held_slots <> (SELECT count(*) FROM governance_concurrency_hold h
				WHERE h.workspace_id = g.workspace_id AND h.resource = g.resource AND h.state = 'held')`},
		{"reservation revision equals last journal revision", `
			SELECT count(*) FROM governance_budget_reservation r
			WHERE r.workspace_id = $1 AND r.revision <> COALESCE((SELECT max(resulting_revision) FROM governance_budget_journal j
				WHERE j.workspace_id = r.workspace_id AND j.reservation_id = r.reservation_id), -1)`},
		{"journal revision chain is gap free", `
			SELECT count(*) FROM (
				SELECT expected_revision, lag(resulting_revision, 1, 0) OVER (PARTITION BY reservation_id ORDER BY resulting_revision) AS previous
				FROM governance_budget_journal WHERE workspace_id = $1
			) chain WHERE expected_revision <> previous`},
		{"attempt and obligation ids are unique", `
			SELECT (SELECT count(*) FROM governance_budget_reservation WHERE workspace_id = $1)
				- (SELECT count(DISTINCT (attempt_id, obligation_id)) FROM governance_budget_reservation WHERE workspace_id = $1)`},
		{"retry allowance matches debits", `
			SELECT count(*) FROM governance_budget_reservation r
			WHERE r.workspace_id = $1 AND (r.attempts_started > r.retry_allowance + 1
				OR r.retry_allowance_remaining <> r.retry_allowance - (r.attempts_started - 1))`},
	}
	for _, check := range checks {
		var violations int64
		if err := pool.QueryRow(ctx, check.query, workspaceID).Scan(&violations); err != nil {
			t.Fatalf("conservation %q: %v", check.name, err)
		}
		if violations != 0 {
			t.Fatalf("conservation violated: %s (%d)", check.name, violations)
		}
	}
	t.Logf("recovery-conservation-receipt workspace=%s checks=%d ok", workspaceID, len(checks))
}

// recoveryReservation reads the persisted reservation counters directly.
type recoveryReservationRow struct {
	StateName                                                              string
	Revision, Debited, Settled, Remaining, AttemptsStarted, RetryRemaining int64
}

func readRecoveryReservation(t *testing.T, pool *pgxpool.Pool, workspaceID, reservationID pgtype.UUID) recoveryReservationRow {
	t.Helper()
	var row recoveryReservationRow
	if err := pool.QueryRow(context.Background(), `
		SELECT state, revision, debited_micro_usd, settled_micro_usd, remaining_micro_usd, attempts_started, retry_allowance_remaining
		FROM governance_budget_reservation WHERE workspace_id = $1 AND reservation_id = $2
	`, workspaceID, reservationID).Scan(&row.StateName, &row.Revision, &row.Debited, &row.Settled, &row.Remaining, &row.AttemptsStarted, &row.RetryRemaining); err != nil {
		t.Fatalf("read reservation %s: %v", reservationID, err)
	}
	return row
}

func newRecoveryFixture(t *testing.T) *admissionTestFixture {
	t.Helper()
	fixture := newAdmissionTestFixture(t)
	fixture.command.BudgetPolicy.RetryAllowance = 2
	return fixture
}

// nextRecoveryCommand seeds another case in the same workspace and budget root.
func nextRecoveryCommand(t *testing.T, fixture *admissionTestFixture, resource string) AdmissionCommand {
	t.Helper()
	command := seedAdmissionCase(t, fixture.budget, fixture.rootID, resource)
	command.BudgetPolicy = fixture.command.BudgetPolicy
	command.BudgetPolicy.Resource = resource
	return command
}
