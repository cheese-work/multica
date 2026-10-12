package governance

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type concurrencyFixture struct {
	pool        *pgxpool.Pool
	workspaceID pgtype.UUID
}

func newConcurrencyFixture(t *testing.T) concurrencyFixture {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	workspaceID := concurrencyUUID()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO governance_workspace_config (workspace_id, control_epoch, settings)
		VALUES ($1, 1, '{"jev_governance_enabled":true,"rule_mode":"shadow"}')
	`, workspaceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"governance_concurrency_hold", "governance_concurrency_guard", "governance_workspace_config"} {
			if _, err := pool.Exec(context.Background(), "DELETE FROM "+table+" WHERE workspace_id = $1", workspaceID); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
	})
	return concurrencyFixture{pool: pool, workspaceID: workspaceID}
}

func concurrencyUUID() pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

func (fixture concurrencyFixture) lock(t *testing.T, resources ...string) (pgx.Tx, *ConcurrencyGuards) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	guards, err := LockConcurrencyResources(ctx, tx, fixture.workspaceID, resources...)
	if err != nil {
		t.Fatal(err)
	}
	return tx, guards
}

func (fixture concurrencyFixture) counts(t *testing.T, resource string, wantHeld, wantRows int64) {
	t.Helper()
	var held, rows, active int64
	err := fixture.pool.QueryRow(context.Background(), `
		SELECT held_slots,
		    (SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND resource = $2),
		    (SELECT count(*) FROM governance_concurrency_hold WHERE workspace_id = $1 AND resource = $2 AND state = 'held')
		FROM governance_concurrency_guard WHERE workspace_id = $1 AND resource = $2
	`, fixture.workspaceID, resource).Scan(&held, &rows, &active)
	if err != nil || held != wantHeld || active != wantHeld || rows != wantRows {
		t.Fatalf("held/counter/rows = %d/%d/%d, want %d/%d/%d: %v", active, held, rows, wantHeld, wantHeld, wantRows, err)
	}
}

func TestConcurrencyIndependentWorkersRaceInitialGuard(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	workers := sync.WaitGroup{}
	workers.Add(2)
	connections := make(map[uint32]bool)
	for range 2 {
		tx, err := fixture.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		connections[tx.Conn().PgConn().PID()] = true
		go func() {
			defer workers.Done()
			defer tx.Rollback(context.Background())
			<-start
			guards, err := LockConcurrencyResources(ctx, tx, fixture.workspaceID, "jev")
			if err == nil {
				_, err = guards.Reserve(ctx, "jev", concurrencyUUID(), 1, 1)
			}
			if err == nil {
				err = tx.Commit(ctx)
			}
			results <- err
		}()
	}
	if len(connections) != 2 {
		t.Fatal("race did not use independent PostgreSQL connections")
	}
	close(start)
	workers.Wait()
	close(results)
	admitted, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrConcurrencyLimit):
			refused++
		default:
			t.Fatalf("unexpected worker result: %v", err)
		}
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("admitted/refused = %d/%d, want 1/1", admitted, refused)
	}
	fixture.counts(t, "jev", 1, 1)
}

func TestConcurrencyOldUnknownHoldSurvivesWindowsAndLowerCap(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	ctx := context.Background()
	reservations := []pgtype.UUID{concurrencyUUID(), concurrencyUUID()}
	tx, guards := fixture.lock(t, "agent")
	for _, reservation := range reservations {
		if duplicate, err := guards.Reserve(ctx, "agent", reservation, 1, 2); err != nil || duplicate {
			t.Fatalf("initial reserve = %v/%v", duplicate, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE governance_concurrency_hold SET held_at = now() - interval '60 days' WHERE workspace_id = $1`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	tx, guards = fixture.lock(t, "agent")
	if duplicate, err := guards.Reserve(ctx, "agent", reservations[0], 1, 1); err != nil || !duplicate {
		t.Fatalf("duplicate above reduced cap = %v/%v", duplicate, err)
	}
	if _, err := guards.Reserve(ctx, "agent", concurrencyUUID(), 1, 1); !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("old unknown holds lost at new-window/lower-cap admission: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	fixture.counts(t, "agent", 2, 2)
	tx, guards = fixture.lock(t, "agent")
	if released, err := guards.Release(ctx, "agent", reservations[0]); err != nil || !released {
		t.Fatalf("release = %v/%v", released, err)
	}
	if released, err := guards.Release(ctx, "agent", reservations[0]); err != nil || released {
		t.Fatalf("duplicate release = %v/%v", released, err)
	}
	if _, err := guards.Reserve(ctx, "agent", reservations[0], 1, 2); !errors.Is(err, ErrConcurrencyReleased) {
		t.Fatalf("released reservation reacquired: %v", err)
	}
	if _, err := guards.Reserve(ctx, "agent", concurrencyUUID(), 1, 1); !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("remaining hold ignored at reduced cap: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	fixture.counts(t, "agent", 1, 2)
}

func TestConcurrencyCallerRollbackAndCrashLeaveNoPartialEffects(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	ctx := context.Background()
	reservationID := concurrencyUUID()
	tx, guards := fixture.lock(t, "jev")
	if _, err := guards.Reserve(ctx, "jev", reservationID, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE governance_workspace_config SET control_epoch = 2 WHERE workspace_id = $1`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Conn().Close(ctx); err != nil {
		t.Fatal(err)
	}
	tx, guards = fixture.lock(t, "jev")
	if _, err := guards.Reserve(ctx, "jev", reservationID, 1, 1); err != nil {
		t.Fatalf("crashed transaction retained guard/hold/config effects: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	fixture.counts(t, "jev", 1, 1)
	tx, guards = fixture.lock(t, "jev")
	if _, err := guards.Release(ctx, "jev", reservationID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	fixture.counts(t, "jev", 1, 1)
}

func TestConcurrencyControlFenceAndDisabledSettlement(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	ctx := context.Background()
	reservationID := concurrencyUUID()
	tx, guards := fixture.lock(t, "jev")
	if _, err := guards.Reserve(ctx, "jev", reservationID, 0, 1); !errors.Is(err, ErrConcurrencyControl) {
		t.Fatalf("stale control epoch admitted: %v", err)
	}
	if _, err := guards.Reserve(ctx, "jev", reservationID, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE governance_workspace_config SET control_epoch = 2, settings = '{"rule_mode":"off"}' WHERE workspace_id = $1`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	tx, guards = fixture.lock(t, "jev")
	if _, err := guards.Reserve(ctx, "jev", concurrencyUUID(), 2, 2); !errors.Is(err, ErrConcurrencyControl) {
		t.Fatalf("disabled workspace admitted: %v", err)
	}
	if released, err := guards.Release(ctx, "jev", reservationID); err != nil || !released {
		t.Fatalf("kill switch prevented settlement: %v/%v", released, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	fixture.counts(t, "jev", 0, 1)
}

func TestConcurrencyLocksSortedResourcesAndIsolatesWorkspaces(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	other := newConcurrencyFixture(t)
	ctx := context.Background()
	reservationID := concurrencyUUID()
	for _, workspace := range []concurrencyFixture{fixture, other} {
		tx, guards := workspace.lock(t, "jev", "agent", "jev")
		if !slices.Equal(guards.resources, []string{"agent", "jev"}) {
			t.Fatalf("resource lock order = %v, want [agent jev]", guards.resources)
		}
		for _, resource := range []string{"agent", "jev"} {
			if _, err := guards.Reserve(ctx, resource, reservationID, 1, 1); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := guards.Reserve(ctx, "unlocked", reservationID, 1, 1); !errors.Is(err, ErrConcurrencyInput) {
			t.Fatalf("unlocked resource admitted: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		workspace.counts(t, "agent", 1, 1)
		workspace.counts(t, "jev", 1, 1)
	}
}

func TestConcurrencyIndependentReleaseWorkersDecrementOnce(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reservationID := concurrencyUUID()
	tx, guards := fixture.lock(t, "jev")
	if _, err := guards.Reserve(ctx, "jev", reservationID, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type releaseResult struct {
		released bool
		err      error
	}
	results := make(chan releaseResult, 2)
	workers := sync.WaitGroup{}
	workers.Add(2)
	for range 2 {
		tx, err := fixture.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			defer workers.Done()
			defer tx.Rollback(context.Background())
			<-start
			guards, err := LockConcurrencyResources(ctx, tx, fixture.workspaceID, "jev")
			released := false
			if err == nil {
				released, err = guards.Release(ctx, "jev", reservationID)
			}
			if err == nil {
				err = tx.Commit(ctx)
			}
			results <- releaseResult{released: released, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	releases := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.released {
			releases++
		}
	}
	if releases != 1 {
		t.Fatalf("release transitions = %d, want 1", releases)
	}
	fixture.counts(t, "jev", 0, 1)
	tx, guards = fixture.lock(t, "jev")
	if _, err := guards.Reserve(ctx, "jev", reservationID, 1, 1); !errors.Is(err, ErrConcurrencyReleased) {
		t.Fatalf("restart reacquired released reservation: %v", err)
	}
	if released, err := guards.Release(ctx, "jev", concurrencyUUID()); err != nil || released {
		t.Fatalf("unknown release = %v/%v", released, err)
	}
	if _, err := guards.Reserve(ctx, "jev", concurrencyUUID(), 1, 1); err != nil {
		t.Fatalf("release did not make the slot available: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	fixture.counts(t, "jev", 1, 2)
}

func TestConcurrencyMissingConfigDefaultsOffAndInvalidInputs(t *testing.T) {
	fixture := newConcurrencyFixture(t)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `DELETE FROM governance_workspace_config WHERE workspace_id = $1`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	tx, guards := fixture.lock(t, "jev")
	if _, err := guards.Reserve(ctx, "jev", concurrencyUUID(), 0, 1); !errors.Is(err, ErrConcurrencyControl) {
		t.Fatalf("missing config admitted: %v", err)
	}
	for _, input := range []struct {
		reservation pgtype.UUID
		limit       int64
	}{{pgtype.UUID{}, 1}, {concurrencyUUID(), -1}, {pgtype.UUID{Valid: true}, 1}} {
		if _, err := guards.Reserve(ctx, "jev", input.reservation, 0, input.limit); !errors.Is(err, ErrConcurrencyInput) {
			t.Fatalf("invalid input accepted: %v", err)
		}
	}
	if _, err := LockConcurrencyResources(ctx, tx, fixture.workspaceID, " "); !errors.Is(err, ErrConcurrencyInput) {
		t.Fatalf("blank resource accepted: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}
