package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/metrics"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

const askGuardTestFix = "multica decision request ..."

func setAskGuardMode(t *testing.T, mode string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'), '{ask_guard_mode}', to_jsonb($1::text)) WHERE id = $2`,
		mode, testWorkspaceID); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = settings - 'ask_guard_mode' WHERE id = $1`, testWorkspaceID)
	})
}

func askGuardOp(kind, payload string) askguard.Op {
	return askguard.Op{
		IssueID: dbid.NewV7(), TaskID: dbid.NewV7(), Kind: kind, Payload: []byte(payload),
	}
}

func askGuardEventCount(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM ask_guard_event WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func hitAskGuard(op askguard.Op) (*httptest.ResponseRecorder, bool) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	return w, testHandler.askGuardHit(w, r, parseUUID(testWorkspaceID), op, askGuardTestFix)
}

func TestAskGuardOffWritesNothing(t *testing.T) {
	op := askGuardOp("comment", "x")
	w, proceed := hitAskGuard(op)
	if !proceed || w.Code != http.StatusOK {
		t.Fatalf("off mode: proceed=%v code=%d", proceed, w.Code)
	}
	if n := askGuardEventCount(t, "task_id = $1", op.TaskID); n != 0 {
		t.Fatalf("off mode wrote %d events", n)
	}
}

// TestAskGuardWarnStoresEventOnly: warn never refuses and never creates the
// rows a notice or a repair would (comment, inbox item, task); counts are
// table-wide, which is exact here because no other test runs alongside.
func TestAskGuardWarnStoresEventOnly(t *testing.T) {
	setAskGuardMode(t, "warn")
	count := func(table string) int {
		var n int
		if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	tables := []string{"comment", "inbox_item", "agent_task_queue"}
	before := map[string]int{}
	for _, tbl := range tables {
		before[tbl] = count(tbl)
	}

	op := askGuardOp("comment", "ask")
	w, proceed := hitAskGuard(op)
	if !proceed || w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("warn mode refused: proceed=%v code=%d body=%q", proceed, w.Code, w.Body.String())
	}
	if n := askGuardEventCount(t, "task_id = $1 AND mode = 'warn'", op.TaskID); n != 1 {
		t.Fatalf("warn events = %d, want 1", n)
	}
	for _, tbl := range tables {
		if got := count(tbl); got != before[tbl] {
			t.Errorf("%s rows %d -> %d; warn must create none", tbl, before[tbl], got)
		}
	}
}

func TestAskGuardRetryFindsSameEvent(t *testing.T) {
	setAskGuardMode(t, "warn")
	op := askGuardOp("comment", "same")
	hitAskGuard(op)
	hitAskGuard(op)
	if n := askGuardEventCount(t, "task_id = $1", op.TaskID); n != 1 {
		t.Fatalf("retry of the same operation made %d events, want 1", n)
	}
	other := op
	other.Payload = []byte("different")
	hitAskGuard(other)
	laterTask := op
	laterTask.TaskID = dbid.NewV7()
	hitAskGuard(laterTask)
	if n := askGuardEventCount(t, "task_id = $1", op.TaskID); n != 2 {
		t.Fatalf("a different payload in the same task: %d events, want 2", n)
	}
	if n := askGuardEventCount(t, "task_id = $1", laterTask.TaskID); n != 1 {
		t.Fatalf("the same text in a later task: %d events, want 1", n)
	}
}

// TestAskGuardBlockCommitsEventBeforeRefusalRollback: the event is written on
// its own connection, so rolling back the refused operation's transaction
// leaves the event behind.
func TestAskGuardBlockCommitsEventBeforeRefusalRollback(t *testing.T) {
	setAskGuardMode(t, "block")
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	marker := "ask-guard-rollback-" + uuidToString(dbid.NewV7())
	if _, err := tx.Exec(ctx, `UPDATE workspace SET description = $1 WHERE id = $2`, marker, testWorkspaceID); err != nil {
		t.Fatal(err)
	}

	op := askGuardOp("comment", "refused")
	w, proceed := hitAskGuard(op)
	if proceed || w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("block mode: proceed=%v code=%d", proceed, w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, askGuardTestFix) {
		t.Fatalf("422 body %q lacks the fix command", body)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if n := askGuardEventCount(t, "task_id = $1 AND mode = 'block'", op.TaskID); n != 1 {
		t.Fatalf("event survived rollback %d times, want 1", n)
	}
	var n int
	testPool.QueryRow(ctx, `SELECT count(*) FROM workspace WHERE id = $1 AND description = $2`, testWorkspaceID, marker).Scan(&n)
	if n != 0 {
		t.Fatal("the refused operation's write was not rolled back")
	}
}

type failingEventWrites struct{ db.DBTX }

var errEventWrite = errors.New("event write failed")

func (f failingEventWrites) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO ask_guard_event") {
		return askGuardErrRow{}
	}
	return f.DBTX.QueryRow(ctx, sql, args...)
}

type askGuardErrRow struct{}

func (askGuardErrRow) Scan(...any) error { return errEventWrite }

func TestAskGuardEventWriteFailureIs503AndWritesNothing(t *testing.T) {
	setAskGuardMode(t, "block")
	g := askguard.Guard{Queries: db.New(failingEventWrites{testPool})}
	op := askGuardOp("comment", "unwritable")
	w := httptest.NewRecorder()
	if refuseOnAskGuard(w, context.Background(), g, parseUUID(testWorkspaceID), op, askGuardTestFix) {
		t.Fatal("operation proceeded although the event was not written")
	}
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("code=%d Retry-After=%q, want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	if n := askGuardEventCount(t, "task_id = $1", op.TaskID); n != 0 {
		t.Fatalf("%d events written", n)
	}
}

// TestAskGuardModeSwitchKeepsEarlierWarnEvents: an event keeps the mode it was
// created under, so only events created in block mode are recovery work.
func TestAskGuardModeSwitchKeepsEarlierWarnEvents(t *testing.T) {
	setAskGuardMode(t, "warn")
	op := askGuardOp("comment", "before-switch")
	hitAskGuard(op)
	setAskGuardMode(t, "block")
	op2 := askGuardOp("comment", "after-switch")
	hitAskGuard(op2)

	if n := askGuardEventCount(t, "task_id = $1 AND mode = 'block'", op.TaskID); n != 0 {
		t.Fatalf("warn event became block work after the switch (%d rows)", n)
	}
	if n := askGuardEventCount(t, "task_id = $1 AND mode = 'warn'", op.TaskID); n != 1 {
		t.Fatalf("warn event lost its mode snapshot (%d rows)", n)
	}
	if n := askGuardEventCount(t, "task_id = $1 AND mode = 'block'", op2.TaskID); n != 1 {
		t.Fatalf("event after the switch has no block snapshot (%d rows)", n)
	}
}

// TestAskGuardSameOperationWarnThenBlock: retrying an operation that already
// has a warn event, after the switch to block, still leaves a block event
// behind the 422 and keeps the warn event as it was.
func TestAskGuardSameOperationWarnThenBlock(t *testing.T) {
	setAskGuardMode(t, "warn")
	op := askGuardOp("comment", "retried-across-switch")
	hitAskGuard(op)
	setAskGuardMode(t, "block")
	for range 2 {
		w, proceed := hitAskGuard(op)
		if proceed || w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("block retry: proceed=%v code=%d, want 422", proceed, w.Code)
		}
	}
	if n := askGuardEventCount(t, "task_id = $1 AND mode = 'warn'", op.TaskID); n != 1 {
		t.Fatalf("warn event rows = %d, want 1 unchanged", n)
	}
	if n := askGuardEventCount(t, "task_id = $1 AND mode = 'block'", op.TaskID); n != 1 {
		t.Fatalf("block event rows = %d, want 1 behind the 422", n)
	}
}

func TestAskGuardMetric(t *testing.T) {
	setAskGuardMode(t, "warn")
	m := metrics.NewBusinessMetrics()
	g := askguard.Guard{Queries: testHandler.Queries, Metrics: m}
	op := askGuardOp("comment", "metric")
	for range 2 {
		if _, err := g.Hit(context.Background(), parseUUID(testWorkspaceID), op); err != nil {
			t.Fatal(err)
		}
	}
	fam := metrics.GatherForTest(t, m)["multica_ask_guard_event_total"]
	got := map[string]float64{}
	for _, mtr := range fam.GetMetric() {
		var mode, result string
		for _, l := range mtr.GetLabel() {
			switch l.GetName() {
			case "mode":
				mode = l.GetValue()
			case "result":
				result = l.GetValue()
			}
		}
		got[mode+"/"+result] = mtr.GetCounter().GetValue()
	}
	if got["warn/created"] != 1 || got["warn/retry"] != 1 {
		t.Fatalf("metric = %v, want warn/created=1 warn/retry=1", got)
	}
}

func TestValidateAskGuardSetting(t *testing.T) {
	for _, v := range []any{nil, map[string]any{}, map[string]any{"ask_guard_mode": "off"}, map[string]any{"ask_guard_mode": "warn"}, map[string]any{"ask_guard_mode": "block"}} {
		if err := validateAskGuardSetting(v); err != nil {
			t.Errorf("%v rejected: %v", v, err)
		}
	}
	for _, v := range []any{map[string]any{"ask_guard_mode": "strict"}, map[string]any{"ask_guard_mode": 1}, map[string]any{"ask_guard_mode": nil}} {
		if err := validateAskGuardSetting(v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
}
