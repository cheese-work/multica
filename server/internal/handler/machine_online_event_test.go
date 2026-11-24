package handler

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Durable offline-period id (CHE-9254 stage 2, plan r3). Every online
// transition of a machine commits one machine_online_event row, written by a
// trigger on agent_runtime. The offline event_id is
// offline:<owner>/<daemon>:e<generation>.<committed events of the machine>.
//
// Tests E0 to E9 and FP-01 pin the trigger, the producer and the bounded,
// fail-closed insert on a real database. The migration bounds (M1 to M4,
// FP-02, FP-03) live in server/cmd/migrate.

const (
	moeInsertTrigger = "agent_runtime_online_event_ins"
	moeUpdateTrigger = "agent_runtime_online_event_upd"
)

// moeCleanup removes the events a test wrote. Events are append-only in
// production; the test database is shared, so a test removes its own rows.
func moeCleanup(t *testing.T, daemonPrefix string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM machine_online_event WHERE daemon_id LIKE $1`, daemonPrefix+"%")
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE daemon_id LIKE $1`, daemonPrefix+"%")
	})
}

func moeCount(t *testing.T, ownerID, daemonID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM machine_online_event WHERE machine_owner_id = $1 AND daemon_id = $2`, ownerID, daemonID)
}

// moeOfflineID reads the offline event id of one machine through the existing GET.
func moeOfflineID(t *testing.T, daemonID string) string {
	t.Helper()
	got := affectedFor(affectedAs(t, testUserID), daemonID)
	if got == nil || got.State != "offline" {
		t.Fatalf("machine %s = %+v, want an offline row in the affected list", daemonID, got)
	}
	return got.EventID
}

func moeSecondRuntime(t *testing.T, daemonID, name, provider string, offlineSecondsAgo int) string {
	t.Helper()
	id := dbfx.Runtime(t, name, testutil.Cols{"daemon_id": daemonID, "provider": provider})
	setRuntimeOffline(t, id, offlineSecondsAgo)
	return id
}

func moeRequireDB(t *testing.T) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
}

func TestMachineOnlineEvent_E0_SchemaSelfCheck(t *testing.T) {
	moeRequireDB(t)
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica_test?sslmode=disable"
	}
	parsed, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	var name string
	var events, generation *string
	dbfx.QueryRow(t, `SELECT current_database(), to_regclass('machine_online_event')::text, to_regclass('machine_online_event_generation')::text`).Scan(&name, &events, &generation)
	if want := strings.TrimPrefix(parsed.Path, "/"); name != want {
		t.Fatalf("current_database() = %q, DATABASE_URL names %q", name, want)
	}
	if events == nil || generation == nil {
		t.Fatalf("machine_online_event = %v, machine_online_event_generation = %v; both tables must exist in %s", events, generation, name)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'agent_runtime'::regclass AND tgname IN ($1, $2) AND tgenabled = 'O'`, moeInsertTrigger, moeUpdateTrigger); got != 2 {
		t.Fatalf("enabled online-event triggers on agent_runtime = %d, want 2 (%s, %s)", got, moeInsertTrigger, moeUpdateTrigger)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM machine_online_event_generation`); got != 1 {
		t.Fatalf("generation rows = %d, want exactly 1", got)
	}
}

// E1 (gap 1): a runtime that reconnects and is then deleted must not let the id
// fall back to the old one. On main the MAX(last_seen_at) returns to runtime A.
func TestMachineOnlineEvent_E1_ReconnectThenDeleteChangesTheId(t *testing.T) {
	moeRequireDB(t)
	const daemonID = "moe-e1"
	moeCleanup(t, daemonID)
	cleanupBannerRows(t, daemonID)
	runtimeA, _ := seedPausableRuntime(t, context.Background(), daemonID, daemonID)
	setRuntimeOffline(t, runtimeA, 600)
	runtimeB := moeSecondRuntime(t, daemonID, "moe-e1 B", "handler_test_runtime_b", 7200)

	id1 := moeOfflineID(t, daemonID)
	setRuntimeOnline(t, runtimeB, 0)
	setRuntimeOffline(t, runtimeB, 5)
	if err := testHandler.Queries.DeleteAgentRuntime(context.Background(), parseUUID(runtimeB)); err != nil {
		t.Fatalf("DeleteAgentRuntime: %v", err)
	}
	if id2 := moeOfflineID(t, daemonID); id2 == id1 {
		t.Fatalf("id after reconnect and delete = %q, same as before: a new outage would stay hidden", id2)
	}
}

// E2 (gap 2): deleting the idle runtime that holds the MAX during one outage
// must not change the id. On main the MAX drops and the id changes.
func TestMachineOnlineEvent_E2_DeleteDuringOutageKeepsTheId(t *testing.T) {
	moeRequireDB(t)
	const daemonID = "moe-e2"
	moeCleanup(t, daemonID)
	cleanupBannerRows(t, daemonID)
	runtimeA, _ := seedPausableRuntime(t, context.Background(), daemonID, daemonID)
	setRuntimeOffline(t, runtimeA, 7200)
	runtimeB := moeSecondRuntime(t, daemonID, "moe-e2 B", "handler_test_runtime_b", 600)

	id1 := moeOfflineID(t, daemonID)
	if err := testHandler.Queries.DeleteAgentRuntime(context.Background(), parseUUID(runtimeB)); err != nil {
		t.Fatalf("DeleteAgentRuntime: %v", err)
	}
	if id2 := moeOfflineID(t, daemonID); id2 != id1 {
		t.Fatalf("id after delete = %q, want %q: a closed banner would come back", id2, id1)
	}
}

// E3: a runtime that comes online and goes offline again starts a new period.
// Green on main too: a regression guard.
func TestMachineOnlineEvent_E3_BackOnlineThenOfflineIsANewPeriod(t *testing.T) {
	moeRequireDB(t)
	const daemonID = "moe-e3"
	moeCleanup(t, daemonID)
	runtimeID := seedOfflineMachine(t, daemonID, 3600)

	id1 := moeOfflineID(t, daemonID)
	setRuntimeOnline(t, runtimeID, 30)
	setRuntimeOffline(t, runtimeID, 20)
	if id2 := moeOfflineID(t, daemonID); id2 == id1 {
		t.Fatalf("id after a reconnect = %q, same as the old period", id2)
	}
}

// E4: while the machine stays offline nothing but an online transition changes
// the id. Green on main too: a regression guard.
func TestMachineOnlineEvent_E4_PeriodIsStableWhileOffline(t *testing.T) {
	moeRequireDB(t)
	const daemonID = "moe-e4"
	moeCleanup(t, daemonID)
	runtimeID := seedOfflineMachine(t, daemonID, 3600)
	ctx := context.Background()
	q := testHandler.Queries
	rid := parseUUID(runtimeID)

	id1 := moeOfflineID(t, daemonID)
	for i := 0; i < 3; i++ {
		if got := moeOfflineID(t, daemonID); got != id1 {
			t.Fatalf("GET %d = %q, want %q", i+1, got, id1)
		}
	}
	if n, err := q.TouchAgentRuntimeLastSeen(ctx, rid); err != nil || n != 0 {
		t.Fatalf("TouchAgentRuntimeLastSeen on an offline runtime = %d, %v; want 0 rows", n, err)
	}
	if ids, err := q.TouchAgentRuntimesLastSeenBatch(ctx, []pgtype.UUID{rid}); err != nil || len(ids) != 0 {
		t.Fatalf("TouchAgentRuntimesLastSeenBatch on an offline runtime = %v, %v; want 0 rows", ids, err)
	}
	if err := q.SetAgentRuntimeOffline(ctx, rid); err != nil {
		t.Fatalf("SetAgentRuntimeOffline: %v", err)
	}
	if _, err := q.UpdateAgentRuntimeVisibility(ctx, db.UpdateAgentRuntimeVisibilityParams{Visibility: "public", SharedWith: []pgtype.UUID{}, ID: rid}); err != nil {
		t.Fatalf("UpdateAgentRuntimeVisibility: %v", err)
	}
	if _, err := q.UpdateAgentRuntimeCustomName(ctx, db.UpdateAgentRuntimeCustomNameParams{CustomName: pgtype.Text{String: "renamed", Valid: true}, ID: rid}); err != nil {
		t.Fatalf("UpdateAgentRuntimeCustomName: %v", err)
	}
	if got := moeOfflineID(t, daemonID); got != id1 {
		t.Fatalf("id after the non-transition writes = %q, want %q", got, id1)
	}
}

// E5: exact event counts for every online writer and every non-writer.
func TestMachineOnlineEvent_E5_WriterCoverage(t *testing.T) {
	moeRequireDB(t)
	const prefix = "moe-e5"
	moeCleanup(t, prefix)
	ctx := context.Background()
	q := testHandler.Queries
	owner := dbfx.User(t, "moe e5 owner", "moe-e5-owner@multica.test")
	other := dbfx.User(t, "moe e5 other", "moe-e5-other@multica.test")

	count := func(daemonID string) int { return moeCount(t, owner, daemonID) }
	mustDelta := func(label, daemonID string, before, want int) {
		t.Helper()
		if got := count(daemonID) - before; got != want {
			t.Fatalf("%s: events +%d, want +%d", label, got, want)
		}
	}
	upsertParams := func(daemonID, provider, status string) db.UpsertAgentRuntimeParams {
		return db.UpsertAgentRuntimeParams{
			WorkspaceID: parseUUID(testWorkspaceID), DaemonID: pgtype.Text{String: daemonID, Valid: true},
			Name: daemonID + " " + provider, RuntimeMode: "local", Provider: provider, Status: status,
			Metadata: []byte("{}"), OwnerID: parseUUID(owner),
		}
	}

	// UpsertAgentRuntime: a new online row, a steady-state online upsert, offline then online.
	d := prefix + "-upsert"
	b := count(d)
	row, err := q.UpsertAgentRuntime(ctx, upsertParams(d, "moe_p", "online"))
	if err != nil {
		t.Fatalf("UpsertAgentRuntime new: %v", err)
	}
	mustDelta("upsert new online row", d, b, 1)
	if _, err := q.UpsertAgentRuntime(ctx, upsertParams(d, "moe_p", "online")); err != nil {
		t.Fatalf("UpsertAgentRuntime steady: %v", err)
	}
	mustDelta("steady-state online upsert", d, b, 1)
	if _, err := q.UpsertAgentRuntime(ctx, upsertParams(d, "moe_p", "offline")); err != nil {
		t.Fatalf("UpsertAgentRuntime offline: %v", err)
	}
	mustDelta("upsert with status offline", d, b, 1)
	if _, err := q.UpsertAgentRuntime(ctx, upsertParams(d, "moe_p", "online")); err != nil {
		t.Fatalf("UpsertAgentRuntime offline to online: %v", err)
	}
	mustDelta("upsert offline to online", d, b, 2)

	// A new row inserted offline writes nothing.
	dOff := prefix + "-upsert-off"
	bOff := count(dOff)
	if _, err := q.UpsertAgentRuntime(ctx, upsertParams(dOff, "moe_p", "offline")); err != nil {
		t.Fatalf("UpsertAgentRuntime new offline: %v", err)
	}
	mustDelta("new row inserted offline", dOff, bOff, 0)

	// The ownership fence turns the conflict branch into no update: no row, no event.
	fenced := upsertParams(dOff, "moe_p", "online")
	fenced.RequiredOwnerID = parseUUID(other)
	if _, err := q.UpsertAgentRuntime(ctx, fenced); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("fenced upsert err = %v, want pgx.ErrNoRows", err)
	}
	mustDelta("fenced upsert", dOff, bOff, 0)
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_runtime WHERE daemon_id = $1`, dOff).Scan(&status)
	if status != "offline" {
		t.Fatalf("fenced upsert left status %q, want offline", status)
	}

	// UpsertAgentRuntimeWithProfile: offline to online.
	dProf := prefix + "-profile"
	bProf := count(dProf)
	profile := db.UpsertAgentRuntimeWithProfileParams{
		WorkspaceID: parseUUID(testWorkspaceID), DaemonID: pgtype.Text{String: dProf, Valid: true},
		Name: dProf, RuntimeMode: "local", Provider: "moe_p", Status: "offline", Metadata: []byte("{}"),
		OwnerID: parseUUID(owner), ProfileID: parseUUID(other),
	}
	if _, err := q.UpsertAgentRuntimeWithProfile(ctx, profile); err != nil {
		t.Fatalf("UpsertAgentRuntimeWithProfile offline: %v", err)
	}
	mustDelta("profile upsert, new offline row", dProf, bProf, 0)
	profile.Status = "online"
	if _, err := q.UpsertAgentRuntimeWithProfile(ctx, profile); err != nil {
		t.Fatalf("UpsertAgentRuntimeWithProfile online: %v", err)
	}
	mustDelta("profile upsert, offline to online", dProf, bProf, 1)

	// MarkAgentRuntimeOnline and MarkAgentRuntimeOnlineIfOffline.
	dMark := prefix + "-mark"
	markID := dbfx.Runtime(t, dMark, testutil.Cols{"daemon_id": dMark, "owner_id": owner, "status": "offline"})
	bMark := count(dMark)
	if _, err := q.MarkAgentRuntimeOnline(ctx, parseUUID(markID)); err != nil {
		t.Fatalf("MarkAgentRuntimeOnline: %v", err)
	}
	mustDelta("MarkAgentRuntimeOnline on an offline row", dMark, bMark, 1)
	if _, err := q.MarkAgentRuntimeOnline(ctx, parseUUID(markID)); err != nil {
		t.Fatalf("MarkAgentRuntimeOnline again: %v", err)
	}
	mustDelta("MarkAgentRuntimeOnline on an online row", dMark, bMark, 1)
	if n, err := q.TouchAgentRuntimeLastSeen(ctx, parseUUID(markID)); err != nil || n != 1 {
		t.Fatalf("TouchAgentRuntimeLastSeen = %d, %v", n, err)
	}
	if ids, err := q.TouchAgentRuntimesLastSeenBatch(ctx, []pgtype.UUID{parseUUID(markID)}); err != nil || len(ids) != 1 {
		t.Fatalf("TouchAgentRuntimesLastSeenBatch = %v, %v", ids, err)
	}
	mustDelta("heartbeat touch and batch touch", dMark, bMark, 1)
	if err := q.SetAgentRuntimeOffline(ctx, parseUUID(markID)); err != nil {
		t.Fatalf("SetAgentRuntimeOffline: %v", err)
	}
	mustDelta("SetAgentRuntimeOffline", dMark, bMark, 1)
	if n, err := q.MarkAgentRuntimeOnlineIfOffline(ctx, parseUUID(markID)); err != nil || n != 1 {
		t.Fatalf("MarkAgentRuntimeOnlineIfOffline = %d, %v; want 1 row", n, err)
	}
	mustDelta("MarkAgentRuntimeOnlineIfOffline", dMark, bMark, 2)
	if n, err := q.MarkAgentRuntimeOnlineIfOffline(ctx, parseUUID(markID)); err != nil || n != 0 {
		t.Fatalf("MarkAgentRuntimeOnlineIfOffline on an online row = %d, %v; want 0 rows", n, err)
	}
	mustDelta("MarkAgentRuntimeOnlineIfOffline on an online row", dMark, bMark, 2)

	// The owner_id move of an online runtime (qoder_connection.go qoderReady shape):
	// one event on the destination machine key, none on the source key.
	dMove := prefix + "-move"
	dbfx.Runtime(t, dMove, testutil.Cols{"daemon_id": dMove, "owner_id": owner})
	srcBefore, dstBefore := moeCount(t, owner, dMove), moeCount(t, other, dMove)
	dbfx.Exec(t, `UPDATE agent_runtime SET visibility = 'public', owner_id = $2 WHERE daemon_id = $1`, dMove, other)
	if got := moeCount(t, owner, dMove) - srcBefore; got != 0 {
		t.Fatalf("owner move: source key events +%d, want +0", got)
	}
	if got := moeCount(t, other, dMove) - dstBefore; got != 1 {
		t.Fatalf("owner move: destination key events +%d, want +1", got)
	}

	// A row with a null daemon_id has no machine: no event under any owner.
	before := dbfx.Count(t, `SELECT count(*) FROM machine_online_event WHERE machine_owner_id = $1`, owner)
	nullID := dbfx.Runtime(t, prefix+"-null", testutil.Cols{"owner_id": owner, "status": "offline"})
	dbfx.Exec(t, `UPDATE agent_runtime SET status = 'online' WHERE id = $1`, nullID)
	if got := dbfx.Count(t, `SELECT count(*) FROM machine_online_event WHERE machine_owner_id = $1`, owner) - before; got != 0 {
		t.Fatalf("null daemon_id: events +%d, want +0", got)
	}
	_ = row
}

// E6 (a): a blocked event table fails the online flip within the trigger's
// budget and leaves the runtime offline. (b): a rolled-back flip leaves no event.
func TestMachineOnlineEvent_E6_FailClosedAndBounded(t *testing.T) {
	moeRequireDB(t)
	const daemonID = "moe-e6"
	moeCleanup(t, daemonID)
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, daemonID, testutil.Cols{"daemon_id": daemonID, "status": "offline"})
	before := moeCount(t, testUserID, daemonID)

	holder, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := holder.Exec(ctx, `LOCK TABLE machine_online_event IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}
	start := time.Now()
	_, err = testHandler.Queries.MarkAgentRuntimeOnline(ctx, parseUUID(runtimeID))
	elapsed := time.Since(start)
	_, _ = holder.Exec(ctx, `ROLLBACK`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("flip while the event table is locked: err = %v, want SQLSTATE 55P03", err)
	}
	if elapsed < 400*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("flip failed after %v, want 0.4 to 2 s", elapsed)
	}
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&status)
	if status != "offline" || moeCount(t, testUserID, daemonID) != before {
		t.Fatalf("after the failed flip: status %q, events +%d; want offline and +0", status, moeCount(t, testUserID, daemonID)-before)
	}

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := testHandler.Queries.WithTx(tx).MarkAgentRuntimeOnline(ctx, parseUUID(runtimeID)); err != nil {
		t.Fatalf("flip in tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	dbfx.QueryRow(t, `SELECT status FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&status)
	if status != "offline" || moeCount(t, testUserID, daemonID) != before {
		t.Fatalf("after the rolled-back flip: status %q, events +%d; want offline and +0", status, moeCount(t, testUserID, daemonID)-before)
	}
}

// FP-01: the trigger's lock wait is capped at 500 ms and never leaks into the
// caller's lock_timeout; once the table is free the retry commits one event.
func TestMachineOnlineEvent_FP01_LockBudgetThenRetry(t *testing.T) {
	moeRequireDB(t)
	const daemonID = "moe-fp01"
	moeCleanup(t, daemonID)
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, daemonID, testutil.Cols{"daemon_id": daemonID, "status": "offline"})
	before := moeCount(t, testUserID, daemonID)

	holder, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := holder.Exec(ctx, `LOCK TABLE machine_online_event IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}
	start := time.Now()
	_, err = testHandler.Queries.MarkAgentRuntimeOnline(ctx, parseUUID(runtimeID))
	elapsed := time.Since(start)
	_, _ = holder.Exec(ctx, `ROLLBACK`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("first attempt: err = %v, want SQLSTATE 55P03", err)
	}
	if elapsed < 400*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Fatalf("first attempt failed after %v, want 0.4 to 1.5 s", elapsed)
	}
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&status)
	if status != "offline" || moeCount(t, testUserID, daemonID) != before {
		t.Fatalf("first attempt left status %q, events +%d; want offline and +0", status, moeCount(t, testUserID, daemonID)-before)
	}

	// Retry: one flip, one event, and lock_timeout is the caller's value afterwards.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var lockBefore, lockAfter string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&lockBefore); err != nil {
		t.Fatalf("show lock_timeout: %v", err)
	}
	if _, err := testHandler.Queries.WithTx(tx).MarkAgentRuntimeOnline(ctx, parseUUID(runtimeID)); err != nil {
		t.Fatalf("retry flip: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&lockAfter); err != nil {
		t.Fatalf("show lock_timeout: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if lockBefore != lockAfter {
		t.Fatalf("lock_timeout before the flip %q, after %q; the trigger must restore the caller's value", lockBefore, lockAfter)
	}
	if got := moeCount(t, testUserID, daemonID) - before; got != 1 {
		t.Fatalf("retry wrote %d events, want exactly 1", got)
	}

	// A non-default caller value is restored too.
	dbfx.Exec(t, `UPDATE agent_runtime SET status = 'offline' WHERE id = $1`, runtimeID)
	tx, err = testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
		t.Fatalf("set local: %v", err)
	}
	if _, err := testHandler.Queries.WithTx(tx).MarkAgentRuntimeOnline(ctx, parseUUID(runtimeID)); err != nil {
		t.Fatalf("flip with a 3s caller value: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&lockAfter); err != nil {
		t.Fatalf("show lock_timeout: %v", err)
	}
	if lockAfter != "3s" {
		t.Fatalf("lock_timeout after the flip = %q, want the caller's 3s", lockAfter)
	}
}

// E7: a transition that commits after another one of the same machine still
// changes the id, and neither flip waits on the other's open transaction.
func TestMachineOnlineEvent_E7_LateCommitStillChangesTheId(t *testing.T) {
	moeRequireDB(t)
	const daemonID = "moe-e7"
	moeCleanup(t, daemonID)
	cleanupBannerRows(t, daemonID)
	ctx := context.Background()
	runtimeA, _ := seedPausableRuntime(t, ctx, daemonID, daemonID)
	setRuntimeOffline(t, runtimeA, 3600)
	runtimeB := moeSecondRuntime(t, daemonID, "moe-e7 B", "handler_test_runtime_b", 3600)
	before := moeCount(t, testUserID, daemonID)

	s1, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire s1: %v", err)
	}
	defer s1.Release()
	if _, err := s1.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("s1 begin: %v", err)
	}
	if _, err := s1.Exec(ctx, `UPDATE agent_runtime SET status = 'online' WHERE id = $1`, runtimeA); err != nil {
		t.Fatalf("s1 flip A online: %v", err)
	}

	s2, err := testPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire s2: %v", err)
	}
	defer s2.Release()
	if _, err := s2.Exec(ctx, `SET statement_timeout = '1s'`); err != nil {
		t.Fatalf("s2 set: %v", err)
	}
	defer s2.Exec(ctx, `RESET statement_timeout`)
	if _, err := s2.Exec(ctx, `UPDATE agent_runtime SET status = 'online' WHERE id = $1`, runtimeB); err != nil {
		t.Fatalf("s2 flip B online while s1 is open: %v", err)
	}
	if _, err := s2.Exec(ctx, `UPDATE agent_runtime SET status = 'offline' WHERE id = $1`, runtimeB); err != nil {
		t.Fatalf("s2 flip B offline: %v", err)
	}
	idMid := moeOfflineID(t, daemonID)

	if _, err := s1.Exec(ctx, `COMMIT`); err != nil {
		t.Fatalf("s1 commit: %v", err)
	}
	if _, err := s2.Exec(ctx, `UPDATE agent_runtime SET status = 'offline' WHERE id = $1`, runtimeA); err != nil {
		t.Fatalf("flip A offline: %v", err)
	}
	if idAfter := moeOfflineID(t, daemonID); idAfter == idMid {
		t.Fatalf("id after the late commit = %q, same as before it: a dismissal of the old id would hide the outage", idAfter)
	}
	if got := moeCount(t, testUserID, daemonID) - before; got != 2 {
		t.Fatalf("events +%d, want exactly +2 (A and B each went online once)", got)
	}
}

// E9: a new generation changes every machine's id, a machine with no events
// included, so a dismissal stored before cannot match.
func TestMachineOnlineEvent_E9_GenerationFence(t *testing.T) {
	moeRequireDB(t)
	setBannerCapability(t, "epoch-v1")
	const daemonX, daemonY = "moe-e9-x", "moe-e9-y"
	moeCleanup(t, "moe-e9")
	cleanupBannerRows(t, "moe-e9")
	ctx := context.Background()
	// X: no events (inserted offline). Y: three online transitions, ends offline.
	runtimeX, _ := seedPausableRuntime(t, ctx, daemonX, daemonX)
	setRuntimeOffline(t, runtimeX, 3600)
	runtimeY, _ := seedPausableRuntime(t, ctx, daemonY, daemonY)
	for i := 0; i < 2; i++ {
		setRuntimeOffline(t, runtimeY, 3600)
		setRuntimeOnline(t, runtimeY, 60)
	}
	setRuntimeOffline(t, runtimeY, 3600)
	if got := moeCount(t, testUserID, daemonY); got != 3 {
		t.Fatalf("machine Y has %d events, want 3", got)
	}

	idX, idY := bannerRowOf(t, testUserID, daemonX).EventID, bannerRowOf(t, testUserID, daemonY).EventID
	if got := dismissIDs(t, testUserID, idX, idY); len(got["dismissed"]) != 2 {
		t.Fatalf("dismiss = %v, want both ids dismissed", got)
	}
	if !isDismissed(t, testUserID, daemonX) || !isDismissed(t, testUserID, daemonY) {
		t.Fatal("both machines must read as dismissed before the generation changes")
	}

	dbfx.Exec(t, `UPDATE machine_online_event_generation SET generation = gen_random_uuid()`)
	rowX, rowY := bannerRowOf(t, testUserID, daemonX), bannerRowOf(t, testUserID, daemonY)
	if rowX.EventID == idX || rowY.EventID == idY {
		t.Fatalf("ids after a new generation: X %q (was %q), Y %q (was %q); every id must change, a machine with 0 events too", rowX.EventID, idX, rowY.EventID, idY)
	}
	if (rowX.Dismissed != nil && *rowX.Dismissed) || (rowY.Dismissed != nil && *rowY.Dismissed) {
		t.Fatal("a dismissal stored before the new generation still matches")
	}
	for i := 0; i < 3; i++ {
		if got := bannerRowOf(t, testUserID, daemonX).EventID; got != rowX.EventID {
			t.Fatalf("GET %d for X = %q, want the stable %q", i+1, got, rowX.EventID)
		}
		if got := bannerRowOf(t, testUserID, daemonY).EventID; got != rowY.EventID {
			t.Fatalf("GET %d for Y = %q, want the stable %q", i+1, got, rowY.EventID)
		}
	}
}
