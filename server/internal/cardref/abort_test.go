package cardref

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A2 (spec section 2): a missing row becomes a `void` tombstone and a `pending`
// row becomes `void`; open, pick_pending, decided and void stay exactly as they
// are and come back with notAborted.
func testAbortIfPending(t *testing.T, p *pgxpool.Pool) {
	want := map[Status]struct {
		status     Status
		notAborted bool
	}{
		Missing:     {Void, false},
		Pending:     {Void, false},
		Open:        {Open, true},
		PickPending: {PickPending, true},
		Decided:     {Decided, true},
		Void:        {Void, true},
	}
	for _, from := range allStates {
		t.Run(string(from), func(t *testing.T) {
			id := fmt.Sprintf("a2-%s", from)
			if from != Missing {
				seed(t, p, id, from)
			}
			status, notAborted, err := AbortIfPending(context.Background(), p, id)
			if err != nil {
				t.Fatal(err)
			}
			if w := want[from]; status != w.status || notAborted != w.notAborted {
				t.Fatalf("from %s: got (%s, notAborted=%v), want (%s, notAborted=%v)", from, status, notAborted, w.status, w.notAborted)
			}
			if got := statusOf(t, p, id); got != want[from].status {
				t.Fatalf("from %s: row is %s, want %s", from, got, want[from].status)
			}
		})
	}
}

// A1 (spec section 2): unconditional. Missing becomes a `void` tombstone;
// pending, open and pick_pending become `void`; void and decided stay. It
// answers the final status.
func testAbortRef(t *testing.T, p *pgxpool.Pool) {
	want := map[Status]Status{Missing: Void, Pending: Void, Open: Void, PickPending: Void, Decided: Decided, Void: Void}
	for _, from := range allStates {
		t.Run(string(from), func(t *testing.T) {
			id := fmt.Sprintf("a1-%s", from)
			if from != Missing {
				seed(t, p, id, from)
			}
			status, err := AbortRef(context.Background(), p, id, "force_release by cheese")
			if err != nil {
				t.Fatal(err)
			}
			if status != want[from] || statusOf(t, p, id) != want[from] {
				t.Fatalf("from %s: answered %s, row %s, want %s", from, status, statusOf(t, p, id), want[from])
			}
		})
	}
}

func testRefStatus(t *testing.T, p *pgxpool.Pool) {
	for _, from := range allStates {
		t.Run(string(from), func(t *testing.T) {
			id := fmt.Sprintf("rs-%s", from)
			if from != Missing {
				seed(t, p, id, from)
			}
			got, err := RefStatus(context.Background(), p, id)
			if err != nil {
				t.Fatal(err)
			}
			if got != from {
				t.Fatalf("got %s, want %s", got, from)
			}
		})
	}
}
