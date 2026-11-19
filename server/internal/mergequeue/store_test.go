package mergequeue

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pool opens the migrated test database; each test uses its own workspace id
// and removes its rows.
func pool(t *testing.T) (*pgxpool.Pool, uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	ws := uuid.New()
	t.Cleanup(func() {
		for _, table := range []string{"merge_queue_epoch", "merge_queue_intake"} {
			if _, err := p.Exec(context.Background(), "DELETE FROM "+table+" WHERE workspace_id = $1", ws); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
		p.Close()
	})
	return p, ws
}

func newEpoch(t *testing.T, p *pgxpool.Pool, ws uuid.UUID, state State) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := p.QueryRow(context.Background(), `INSERT INTO merge_queue_epoch (workspace_id, repo_owner, repo_name, cause_key, kind, state) VALUES ($1, 'o', 'r', $2, 'shared', $3) RETURNING id`, ws, uuid.NewString(), string(state)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func epochState(t *testing.T, p *pgxpool.Pool, id uuid.UUID) State {
	t.Helper()
	var s State
	if err := p.QueryRow(context.Background(), `SELECT state FROM merge_queue_epoch WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTransitionEpochWritesEveryLegalEdgeAndNoOther(t *testing.T) {
	p, ws := pool(t)
	ctx := context.Background()
	for _, from := range allStates {
		for _, to := range allStates {
			id := newEpoch(t, p, ws, from)
			err := TransitionEpoch(ctx, p, id, from, to)
			if legalEdges[[2]State{from, to}] {
				if err != nil || epochState(t, p, id) != to {
					t.Errorf("%s -> %s: err %v, stored %s", from, to, err, epochState(t, p, id))
				}
			} else if !errors.Is(err, ErrIllegalTransition) || epochState(t, p, id) != from {
				t.Errorf("%s -> %s: err %v, stored %s; want refusal and no change", from, to, err, epochState(t, p, id))
			}
		}
	}
}

func TestTransitionEpochRefusesAStaleReader(t *testing.T) {
	p, ws := pool(t)
	id := newEpoch(t, p, ws, Owned)
	// The row moved on to verifying after the caller read owned.
	if _, err := p.Exec(context.Background(), `UPDATE merge_queue_epoch SET state = 'verifying' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := TransitionEpoch(context.Background(), p, id, Owned, Unowned); !errors.Is(err, ErrStaleState) {
		t.Errorf("err = %v, want ErrStaleState", err)
	}
	if got := epochState(t, p, id); got != Verifying {
		t.Errorf("state = %s, want verifying", got)
	}
}

func TestTransitionToResolvedStampsResolvedAt(t *testing.T) {
	p, ws := pool(t)
	id := newEpoch(t, p, ws, Verifying)
	if err := TransitionEpoch(context.Background(), p, id, Verifying, Resolved); err != nil {
		t.Fatal(err)
	}
	var stamped bool
	if err := p.QueryRow(context.Background(), `SELECT resolved_at IS NOT NULL FROM merge_queue_epoch WHERE id = $1`, id).Scan(&stamped); err != nil || !stamped {
		t.Errorf("resolved_at stamped = %v (err %v), want true", stamped, err)
	}
}

func TestSetDeliverySetsOnceFromPending(t *testing.T) {
	p, ws := pool(t)
	ctx := context.Background()
	var id uuid.UUID
	if err := p.QueryRow(ctx, `INSERT INTO merge_queue_intake (workspace_id, repo_owner, repo_name, pr_number) VALUES ($1, 'o', 'r', 1) RETURNING id`, ws).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if ok, err := SetDelivery(ctx, p, id, DeliveryHeld, ""); err != nil || !ok {
		t.Fatalf("first set = %v, %v; want true", ok, err)
	}
	// A second path (the failed-wait fallback) must not overwrite it.
	if ok, err := SetDelivery(ctx, p, id, DeliveryRunSent, "wait_create_failed"); err != nil || ok {
		t.Errorf("second set = %v, %v; want false", ok, err)
	}
	var delivery string
	var reason *string
	if err := p.QueryRow(ctx, `SELECT delivery, delivery_reason FROM merge_queue_intake WHERE id = $1`, id).Scan(&delivery, &reason); err != nil || delivery != DeliveryHeld || reason != nil {
		t.Errorf("stored = %q/%v (err %v), want held/nil", delivery, reason, err)
	}
	// Only held or run_sent can be set.
	var id2 uuid.UUID
	if err := p.QueryRow(ctx, `INSERT INTO merge_queue_intake (workspace_id, repo_owner, repo_name, pr_number) VALUES ($1, 'o', 'r', 2) RETURNING id`, ws).Scan(&id2); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{DeliveryPending, "sent", ""} {
		if ok, err := SetDelivery(ctx, p, id2, bad, ""); err == nil || ok {
			t.Errorf("SetDelivery(%q) = %v, %v; want an error", bad, ok, err)
		}
	}
}
