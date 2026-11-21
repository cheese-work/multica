package mergequeue

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func inTx(t *testing.T, p *pgxpool.Pool, fn func(tx pgx.Tx)) {
	t.Helper()
	tx, err := p.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) // runs on t.Fatal too, so the pool can close
	fn(tx)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, p *pgxpool.Pool, table string, ws uuid.UUID) int {
	t.Helper()
	var n int
	if err := p.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE workspace_id = $1", ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func input(ws uuid.UUID, delivery, routing string) IntakeInput {
	return IntakeInput{WS: ws, Owner: "Cheese-Work", Repo: "Multica", PR: 7, DeliveryID: delivery, Routing: routing}
}

func TestInsertIntakeStoresAPendingRowWithTheRoutingOfItsCommit(t *testing.T) {
	p, ws := pool(t)
	var id uuid.UUID
	var created bool
	inTx(t, p, func(tx pgx.Tx) {
		var err error
		id, created, err = InsertIntake(context.Background(), tx, input(ws, "d1", "warn"))
		if err != nil {
			t.Fatal(err)
		}
	})
	if !created || id == uuid.Nil {
		t.Fatalf("created %v, id %v; want a new row", created, id)
	}
	var status, delivery, routing, owner string
	if err := p.QueryRow(context.Background(), `SELECT status, delivery, routing, repo_owner FROM merge_queue_intake WHERE id = $1`, id).Scan(&status, &delivery, &routing, &owner); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || delivery != "pending" || routing != "warn" || owner != "cheese-work" {
		t.Errorf("row = %s/%s/%s/%s; want pending/pending/warn/cheese-work", status, delivery, routing, owner)
	}
}

func TestInsertIntakeDuplicateDeliveryAddsNothing(t *testing.T) {
	p, ws := pool(t)
	for i := 0; i < 2; i++ {
		inTx(t, p, func(tx pgx.Tx) {
			_, created, err := InsertIntake(context.Background(), tx, input(ws, "d1", "warn"))
			if err != nil || created != (i == 0) {
				t.Fatalf("call %d: created %v, err %v", i, created, err)
			}
		})
	}
	if n := count(t, p, "merge_queue_intake", ws); n != 1 {
		t.Errorf("rows = %d; want 1", n)
	}
}

func TestInsertIntakeWithRoutingOffStoresNothing(t *testing.T) {
	p, ws := pool(t)
	inTx(t, p, func(tx pgx.Tx) {
		id, created, err := InsertIntake(context.Background(), tx, input(ws, "d1", "off"))
		if err != nil || created || id != uuid.Nil {
			t.Fatalf("id %v, created %v, err %v; want nothing", id, created, err)
		}
	})
	if n := count(t, p, "merge_queue_intake", ws); n != 0 {
		t.Errorf("rows = %d; want 0", n)
	}
}

func TestEnsureIntakeForRemovalIsUniqueOnTheNode(t *testing.T) {
	p, ws := pool(t)
	for i := 0; i < 2; i++ {
		inTx(t, p, func(tx pgx.Tx) {
			_, created, err := EnsureIntakeForRemoval(context.Background(), tx, input(ws, "", "warn"), "RFMQE_1", time.Now())
			if err != nil || created != (i == 0) {
				t.Fatalf("call %d: created %v, err %v", i, created, err)
			}
		})
	}
	if n := count(t, p, "merge_queue_intake", ws); n != 1 {
		t.Errorf("rows = %d; want 1", n)
	}
}

func TestSetQueueStateKeepsOneRowPerPRAndIgnoresALateChange(t *testing.T) {
	p, ws := pool(t)
	t.Cleanup(func() { p.Exec(context.Background(), `DELETE FROM merge_queue_state WHERE workspace_id = $1`, ws) })
	t0 := time.Now().Truncate(time.Second)
	steps := []struct {
		state string
		at    time.Time
		want  string
	}{
		{QueueEnqueued, t0, QueueEnqueued},
		{QueueDequeued, t0.Add(time.Minute), QueueDequeued},
		{QueueEnqueued, t0.Add(30 * time.Second), QueueDequeued}, // late: ignored
		{QueueClosed, t0.Add(2 * time.Minute), QueueClosed},
	}
	for _, s := range steps {
		inTx(t, p, func(tx pgx.Tx) {
			if err := SetQueueState(context.Background(), tx, ws, "Cheese-Work", "Multica", 7, s.state, s.at); err != nil {
				t.Fatal(err)
			}
		})
		var got string
		if err := p.QueryRow(context.Background(), `SELECT state FROM merge_queue_state WHERE workspace_id = $1 AND pr_number = 7`, ws).Scan(&got); err != nil || got != s.want {
			t.Fatalf("after %s: state %q, err %v; want %s", s.state, got, err, s.want)
		}
	}
	if n := count(t, p, "merge_queue_state", ws); n != 1 {
		t.Errorf("rows = %d; want 1", n)
	}
}

func TestRoutingFromSettingsDefaultsToWarn(t *testing.T) {
	for in, want := range map[string]string{
		``:                                      "warn",
		`{}`:                                    "warn",
		`not json`:                              "warn",
		`{"merge_queue_dequeue_routing":"off"}`: "off",
		`{"merge_queue_dequeue_routing":"block"}`: "block",
		`{"merge_queue_dequeue_routing":"x"}`:     "warn",
	} {
		if got := RoutingFromSettings([]byte(in)); got != want {
			t.Errorf("%q -> %q; want %q", in, got, want)
		}
	}
}
