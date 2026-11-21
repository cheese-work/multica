package cardref

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// emptyDB creates an empty database on the explicit disposable server and
// applies this package's migration only (the full corpus takes minutes).
// It also runs the down file and the up file again, so both are proven.
func emptyDB(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "cardref_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	p, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Close()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		admin.Close(ctx)
	})
	for _, file := range []string{"99c_card_refs.up.sql", "99c_card_refs.down.sql", "99c_card_refs.up.sql"} {
		sql, err := os.ReadFile("../../migrations/" + file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	return p
}

// TestWithDB runs the database tests on one empty migrated database (the tests share it and each uses ids of its own).
func TestWithDB(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	p := emptyDB(t, dsn)
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, *pgxpool.Pool)
	}{
		{"A2 abort_if_pending", testAbortIfPending},
		{"A1 abort_ref", testAbortRef},
		{"ref_status", testRefStatus},
		{"A2 against P7 (race)", testAbortIfPendingRace},
		{"P3 P5 P7 K1 K4 C1", testLifecycle},
		{"receipt checks", testReceiptChecks},
		{"pick_driver", testPickDriver},
		{"service routes", testHandler},
	} {
		run(t, tc.name, func(t *testing.T) { tc.fn(t, p) })
	}
}

// seed inserts a card row in the given state.
func seed(t *testing.T, p *pgxpool.Pool, id string, status Status) {
	t.Helper()
	if _, err := p.Exec(context.Background(), `INSERT INTO card_ref (card_request_id, status) VALUES ($1, $2)`, id, string(status)); err != nil {
		t.Fatal(err)
	}
}

// statusOf reads the row straight from the table; "missing" when absent.
func statusOf(t *testing.T, p *pgxpool.Pool, id string) Status {
	t.Helper()
	var s Status
	err := p.QueryRow(context.Background(), `SELECT status FROM card_ref WHERE card_request_id = $1`, id).Scan(&s)
	if err != nil {
		return Missing
	}
	return s
}

// every state a row can hold, plus the absence of a row
var allStates = []Status{Missing, Pending, Open, PickPending, Decided, Void}

// run is t.Run, but an unexpected error in must() fails this subtest only.
func run(t *testing.T, name string, fn func(*testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("unexpected error: %v", r)
			}
		}()
		fn(t)
	})
}
