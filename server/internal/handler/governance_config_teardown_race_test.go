package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type governanceConfigFenceTxStarter struct {
	inner          txStarter
	beginRequested chan<- struct{}
	continueBegin  <-chan struct{}
	backendPID     chan<- int32
	lockQuery      string
	lockAcquired   chan<- struct{}
	continueLock   <-chan struct{}
}

func (s governanceConfigFenceTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	if s.beginRequested != nil {
		signalGovernanceConfigRace(s.beginRequested)
		select {
		case <-s.continueBegin:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if s.backendPID != nil {
		var pid int32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
		s.backendPID <- pid
	}
	return governanceConfigFenceTx{Tx: tx, starter: s}, nil
}

type governanceConfigFenceTx struct {
	pgx.Tx
	starter governanceConfigFenceTxStarter
}

func (tx governanceConfigFenceTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, query, args...)
	if tx.starter.lockQuery == "" || !strings.Contains(query, tx.starter.lockQuery) {
		return row
	}
	return governanceConfigFenceRow{Row: row, starter: tx.starter}
}

type governanceConfigFenceRow struct {
	pgx.Row
	starter governanceConfigFenceTxStarter
}

func (row governanceConfigFenceRow) Scan(dest ...any) error {
	err := row.Row.Scan(dest...)
	if err == nil && row.starter.lockAcquired != nil {
		signalGovernanceConfigRace(row.starter.lockAcquired)
		if row.starter.continueLock != nil {
			<-row.starter.continueLock
		}
	}
	return err
}

func signalGovernanceConfigRace(ch chan<- struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func TestGovernanceConfigDeleteWinsAgainstPreauthorizedPatch(t *testing.T) {
	if testHandler == nil || testPool == nil || dbfx == nil {
		t.Skip("handler test database is unavailable")
	}
	requireGovernanceConfigTables(t)
	workspaceID := newGovernanceConfigTeardownWorkspace(t, "governance-config-delete-wins")
	beginRequested := make(chan struct{}, 1)
	continueBegin := make(chan struct{})
	var releaseOnce sync.Once
	releaseBegin := func() { releaseOnce.Do(func() { close(continueBegin) }) }
	defer releaseBegin()
	h := *testHandler
	h.TxStarter = governanceConfigFenceTxStarter{
		inner:          testHandler.TxStarter,
		beginRequested: beginRequested,
		continueBegin:  continueBegin,
	}
	body := governanceConfigPatchBody(t, true)
	patchResponse := httptest.NewRecorder()
	patchDone := make(chan struct{})
	go func() {
		defer close(patchDone)
		h.PatchGovernanceConfig(patchResponse, newGovernanceConfigPatchRequest(workspaceID, body))
	}()
	awaitGovernanceConfigRaceSignal(t, beginRequested, "pre-authorized patch transaction")

	deleteResponse := invokeGovernanceConfigWorkspaceDelete(testHandler, workspaceID)
	if deleteResponse.Code != http.StatusNoContent {
		releaseBegin()
		<-patchDone
		t.Fatalf("workspace DELETE = %d: %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	releaseBegin()
	awaitGovernanceConfigRaceDone(t, patchDone, "patch after teardown")
	if patchResponse.Code != http.StatusNotFound {
		t.Fatalf("pre-authorized PATCH after teardown = %d: %s", patchResponse.Code, patchResponse.Body.String())
	}
	assertSafeGovernanceProblem(t, patchResponse)
	assertNoGovernanceConfigTeardownRows(t, workspaceID)
}

func TestGovernanceConfigPatchWinsBeforeWorkspaceTeardown(t *testing.T) {
	if testHandler == nil || testPool == nil || dbfx == nil {
		t.Skip("handler test database is unavailable")
	}
	requireGovernanceConfigTables(t)
	workspaceID := newGovernanceConfigTeardownWorkspace(t, "governance-config-patch-wins")
	setWorkspaceDeleteLockTimeoutForTest(t, 15*time.Second)

	patchLockAcquired := make(chan struct{}, 1)
	continuePatch := make(chan struct{})
	patchPID := make(chan int32, 1)
	var releasePatchOnce sync.Once
	releasePatch := func() { releasePatchOnce.Do(func() { close(continuePatch) }) }
	patchResponse := httptest.NewRecorder()
	patchDone := make(chan struct{})
	patchBody := governanceConfigPatchBody(t, true)
	patchHandler := *testHandler
	patchHandler.TxStarter = governanceConfigFenceTxStarter{
		inner:        testHandler.TxStarter,
		backendPID:   patchPID,
		lockQuery:    "FOR KEY SHARE",
		lockAcquired: patchLockAcquired,
		continueLock: continuePatch,
	}
	go func() {
		defer close(patchDone)
		patchHandler.PatchGovernanceConfig(patchResponse, newGovernanceConfigPatchRequest(workspaceID, patchBody))
	}()
	defer func() {
		releasePatch()
		awaitGovernanceConfigRaceDone(t, patchDone, "patch completion")
	}()
	awaitGovernanceConfigRaceSignal(t, patchLockAcquired, "PATCH workspace lifetime lock")
	writerPID := <-patchPID

	deleteLockAcquired := make(chan struct{}, 1)
	continueDelete := make(chan struct{})
	deletePID := make(chan int32, 1)
	var releaseDeleteOnce sync.Once
	releaseDelete := func() { releaseDeleteOnce.Do(func() { close(continueDelete) }) }
	deleteResponse := httptest.NewRecorder()
	deleteDone := make(chan struct{})
	deleteHandler := *testHandler
	deleteHandler.TxStarter = governanceConfigFenceTxStarter{
		inner:        testHandler.TxStarter,
		backendPID:   deletePID,
		lockQuery:    "FOR UPDATE",
		lockAcquired: deleteLockAcquired,
		continueLock: continueDelete,
	}
	go func() {
		defer close(deleteDone)
		deleteHandler.DeleteWorkspace(deleteResponse, newGovernanceConfigDeleteRequest(workspaceID))
	}()
	defer func() {
		releasePatch()
		releaseDelete()
		awaitGovernanceConfigRaceDone(t, deleteDone, "workspace teardown completion")
	}()
	deleterPID := <-deletePID
	if writerPID == deleterPID {
		t.Fatalf("PATCH and DELETE shared backend pid %d; race requires independent connections", writerPID)
	}
	waitForGovernanceConfigBackendLockWait(t, deleterPID)

	releasePatch()
	awaitGovernanceConfigRaceSignal(t, deleteLockAcquired, "DELETE workspace lock after PATCH commit")
	awaitGovernanceConfigRaceDone(t, patchDone, "PATCH response before teardown sweep")
	if patchResponse.Code != http.StatusOK {
		t.Fatalf("PATCH before teardown = %d: %s", patchResponse.Code, patchResponse.Body.String())
	}
	var configVersion int64
	var auditCount int
	if err := testPool.QueryRow(t.Context(), `
SELECT config.config_version,
       (SELECT count(*) FROM governance_workspace_config_audit WHERE workspace_id = $1)
FROM governance_workspace_config AS config
WHERE config.workspace_id = $1
`, workspaceID).Scan(&configVersion, &auditCount); err != nil {
		t.Fatalf("read committed configuration before teardown sweep: %v", err)
	}
	if configVersion != 1 || auditCount != 1 {
		t.Fatalf("PATCH committed config/audit state = version %d, audit rows %d; want 1/1", configVersion, auditCount)
	}

	releaseDelete()
	awaitGovernanceConfigRaceDone(t, deleteDone, "workspace teardown")
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("workspace DELETE = %d: %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	assertNoGovernanceConfigTeardownRows(t, workspaceID)
}

func newGovernanceConfigTeardownWorkspace(t *testing.T, slugPrefix string) string {
	t.Helper()
	workspaceID := dbfx.Workspace(t, "Governance config teardown race", slugPrefix+"-"+uuid.NewString())
	dbfx.Member(t, workspaceID, testUserID, "owner")
	return workspaceID
}

func governanceConfigPatchBody(t *testing.T, enabled bool) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"expected_version":       0,
		"request_id":             uuid.NewString(),
		"jev_governance_enabled": enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func newGovernanceConfigPatchRequest(workspaceID string, body []byte) *http.Request {
	request := httptest.NewRequest(http.MethodPatch, "/api/workspaces/"+workspaceID+"/governance/config", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-User-ID", testUserID)
	request.Header.Set("X-Workspace-ID", workspaceID)
	return withURLParam(request, "id", workspaceID)
}

func newGovernanceConfigDeleteRequest(workspaceID string) *http.Request {
	request := httptest.NewRequest(http.MethodDelete, "/api/workspaces/"+workspaceID, nil)
	request.Header.Set("X-User-ID", testUserID)
	request.Header.Set("X-Workspace-ID", workspaceID)
	return withURLParam(request, "id", workspaceID)
}

func invokeGovernanceConfigWorkspaceDelete(h *Handler, workspaceID string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	h.DeleteWorkspace(response, newGovernanceConfigDeleteRequest(workspaceID))
	return response
}

func awaitGovernanceConfigRaceSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func awaitGovernanceConfigRaceDone(t *testing.T, done <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForGovernanceConfigBackendLockWait(t *testing.T, pid int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := testPool.QueryRow(t.Context(), `
SELECT EXISTS (
    SELECT 1 FROM pg_stat_activity
    WHERE pid = $1 AND state = 'active' AND wait_event_type = 'Lock'
)
`, pid).Scan(&waiting); err != nil {
			t.Fatalf("poll lock wait for backend %d: %v", pid, err)
		}
		if waiting {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend %d never blocked on the workspace teardown lock", pid)
}

func assertNoGovernanceConfigTeardownRows(t *testing.T, workspaceID string) {
	t.Helper()
	var workspaceExists, configExists, auditExists bool
	if err := testPool.QueryRow(t.Context(), `
SELECT EXISTS(SELECT 1 FROM workspace WHERE id = $1),
       EXISTS(SELECT 1 FROM governance_workspace_config WHERE workspace_id = $1),
       EXISTS(SELECT 1 FROM governance_workspace_config_audit WHERE workspace_id = $1)
`, workspaceID).Scan(&workspaceExists, &configExists, &auditExists); err != nil {
		t.Fatalf("check teardown state: %v", err)
	}
	if workspaceExists || configExists || auditExists {
		t.Fatalf("teardown state workspace/config/audit = %t/%t/%t; want false/false/false", workspaceExists, configExists, auditExists)
	}
}
