package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func insertRefusal(t *testing.T, issueID uuid.UUID, windowID, state, hookPhase string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := sharedTestPool(t).Exec(context.Background(),
		`INSERT INTO run_budget_refusal (id, workspace_id, issue_id, window_id, state, hook_phase) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, uuid.New(), issueID, windowID, state, hookPhase)
	if err != nil {
		t.Fatalf("insert %s/%s: %v", state, hookPhase, err)
	}
	t.Cleanup(func() {
		sharedTestPool(t).Exec(context.Background(), `DELETE FROM run_budget_refusal WHERE id=$1`, id)
	})
	return id
}

func TestRunBudgetRefusalOneRowPerIssueWindow(t *testing.T) {
	issue := uuid.New()
	insertRefusal(t, issue, "w1", "open", "intent")
	if _, err := sharedTestPool(t).Exec(context.Background(),
		`INSERT INTO run_budget_refusal (id, workspace_id, issue_id, window_id, state, hook_phase) VALUES ($1,$2,$3,'w1','open','intent')`,
		uuid.New(), uuid.New(), issue); err == nil {
		t.Fatal("second refusal for the same issue and window was accepted")
	}
	insertRefusal(t, issue, "w2", "open", "intent")
}

func TestRunBudgetRefusalRejectsUnknownStateAndPhase(t *testing.T) {
	pool := sharedTestPool(t)
	for _, c := range [][2]string{{"bogus", "intent"}, {"open", "bogus"}} {
		_, err := pool.Exec(context.Background(),
			`INSERT INTO run_budget_refusal (id, workspace_id, issue_id, window_id, state, hook_phase) VALUES ($1,$2,$3,'w','`+c[0]+`','`+c[1]+`')`,
			uuid.New(), uuid.New(), uuid.New())
		if err == nil {
			t.Fatalf("state=%s phase=%s accepted", c[0], c[1])
		}
	}
	for _, state := range []string{"open", "resume_pending", "still_blocked", "run_queued", "status_restored", "superseded"} {
		insertRefusal(t, uuid.New(), "w", state, "intent")
	}
	for _, phase := range []string{"intent", "blocked", "done", "retired"} {
		insertRefusal(t, uuid.New(), "w", "open", phase)
	}
}

func TestFindOpenRunBudgetRefusalSkipsTerminalRows(t *testing.T) {
	q := db.New(sharedTestPool(t))
	issue := uuid.New()
	for window, state := range map[string]string{"a": "open", "b": "resume_pending", "c": "still_blocked", "d": "run_queued", "e": "status_restored", "f": "superseded"} {
		id := insertRefusal(t, issue, window, state, "intent")
		got, err := FindOpenRunBudgetRefusal(context.Background(), q, pgtype.UUID{Bytes: issue, Valid: true}, window)
		terminal := state == "run_queued" || state == "status_restored" || state == "superseded"
		switch {
		case terminal && !errors.Is(err, ErrRunBudgetRefusalNotFound):
			t.Fatalf("%s: err = %v, want not found", state, err)
		case !terminal && (err != nil || got.Bytes != id):
			t.Fatalf("%s: got %v, %v want %v", state, got, err, id)
		}
	}
	if _, err := FindOpenRunBudgetRefusal(context.Background(), q, pgtype.UUID{Bytes: issue, Valid: true}, "none"); !errors.Is(err, ErrRunBudgetRefusalNotFound) {
		t.Fatalf("missing window: %v", err)
	}
}

func TestRunBudgetRefusalBindsChecksIssueAndWindow(t *testing.T) {
	q := db.New(sharedTestPool(t))
	issue, other := uuid.New(), uuid.New()
	id := insertRefusal(t, issue, "w1", "open", "intent")
	ref, iss := pgtype.UUID{Bytes: id, Valid: true}, pgtype.UUID{Bytes: issue, Valid: true}
	if err := RunBudgetRefusalBinds(context.Background(), q, ref, iss, "w1"); err != nil {
		t.Fatalf("matching refusal: %v", err)
	}
	if err := RunBudgetRefusalBinds(context.Background(), q, ref, pgtype.UUID{Bytes: other, Valid: true}, "w1"); !errors.Is(err, ErrRunBudgetRefusalMismatch) {
		t.Fatalf("other issue: %v", err)
	}
	if err := RunBudgetRefusalBinds(context.Background(), q, ref, iss, "w2"); !errors.Is(err, ErrRunBudgetRefusalMismatch) {
		t.Fatalf("other window: %v", err)
	}
	if err := RunBudgetRefusalBinds(context.Background(), q, pgtype.UUID{Bytes: uuid.New(), Valid: true}, iss, "w1"); !errors.Is(err, ErrRunBudgetRefusalMismatch) {
		t.Fatalf("unknown refusal: %v", err)
	}
}
