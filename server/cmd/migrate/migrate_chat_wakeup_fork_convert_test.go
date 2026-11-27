package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CHE-10195 (CHE-7486 N1a, v0.13.0 train): migration X converts the fork's
// chat_wakeup table to upstream's schema (99zzz15_chat_wakeup).

const chatWakeupXStem = "99zzz00_chat_wakeup_fork_convert"

func wakeupScratchDatabase(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	admin := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("wakeup_x_%d_%d", time.Now().UnixNano(), rand.Uint32())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	config, err := pgxpool.ParseConfig(testDatabaseURL())
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if _, err := admin.Exec(cctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop scratch database %s: %v", name, err)
		}
	})
	return pool, name
}

// wakeupUpFiles returns the real up files in apply order whose stem sorts
// before `before` (empty: all).
func wakeupUpFiles(t *testing.T, before string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	var picked []string
	for _, f := range files {
		stem := strings.TrimSuffix(filepath.Base(f), ".up.sql")
		if before != "" && stem >= before {
			continue
		}
		picked = append(picked, f)
	}
	return picked
}

// applyUpFiles runs in-process the real up files before X: the fork database
// X upgrades. Test setup only; the step under test runs in the binary.
func applyUpFiles(t *testing.T, pool *pgxpool.Pool, files []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := runMigrations(ctx, pool, runOptions{
		Direction:       "up",
		Files:           files,
		AdvisoryLockKey: int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:           preMigrationHooks,
		Conditions:      conditionsForDirection("up"),
	}); err != nil {
		t.Fatalf("apply pre-X up files: %v", err)
	}
}

var (
	migrateBinOnce sync.Once
	migrateBinDir  string
	migrateBinErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if migrateBinDir != "" {
		_ = os.RemoveAll(migrateBinDir)
	}
	os.Exit(code)
}

// migrateBinary builds server/cmd/migrate from the tree under test, once per
// run, so every test here runs the binary of this head (runner pin, P11).
func migrateBinary(t *testing.T) string {
	t.Helper()
	migrateBinOnce.Do(func() {
		migrateBinDir, migrateBinErr = os.MkdirTemp("", "che10195-migrate-")
		if migrateBinErr != nil {
			return
		}
		out, err := exec.Command("go", "build", "-o", filepath.Join(migrateBinDir, "migrate"), ".").CombinedOutput()
		if err != nil {
			migrateBinErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if migrateBinErr != nil {
		t.Fatalf("build migrate binary: %v", migrateBinErr)
	}
	return filepath.Join(migrateBinDir, "migrate")
}

// runMigrateUp runs `migrate up` from the built binary against the scratch
// database. extraEnv is appended (for example PGOPTIONS).
func runMigrateUp(t *testing.T, dbName string, extraEnv ...string) (string, error) {
	t.Helper()
	config, err := pgxpool.ParseConfig(testDatabaseURL())
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = dbName
	u := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", config.ConnConfig.User, config.ConnConfig.Password, config.ConnConfig.Host, config.ConnConfig.Port, dbName)
	cmd := exec.Command(migrateBinary(t), "up")
	cmd.Env = append(os.Environ(), append([]string{"DATABASE_URL=" + u}, extraEnv...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// ---- seed -----------------------------------------------------------------

func wid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }

var (
	wsID      = wid(0x1001)
	userOwner = wid(0x2001) // chat session creator
	userMem   = wid(0x2002) // a member named in an actor filter
	userOrig  = wid(0x2003) // task originator
	userInit  = wid(0x2004) // task initiator
	agentID   = wid(0x3001)
	otherAgt  = wid(0x3002)
	sessOne   = wid(0x4001)
	sessGone  = wid(0x4999) // no chat_session row
	issueOne  = wid(0x5001)
	issueGone = wid(0x5999) // no issue row
	taskInit  = wid(0x6001) // initiator=userInit, originator=userOrig
	taskOrig  = wid(0x6002) // originator=userOrig only
	taskNone  = wid(0x6003) // neither
)

const (
	issuePrefix = "WK"
	issueNumber = 42
	memberName  = "Member Two"
)

// forkRow is one row of the fork's chat_wakeup (701 + 796 + 980).
type forkRow struct {
	label                                string
	id                                   string
	session                              string
	createdByTask                        string // "" NULL
	title, instruction                   string
	fireAt                               string // SQL expression
	status                               string
	firedTask                            string // "" NULL
	createdAt                            string // SQL literal
	resolvedAt                           string // SQL expression, "" NULL
	issue                                string // "" NULL
	eventType, actorType, actorID, until string
	onTimeout                            string
}

const (
	createdFrac  = "2026-11-20 10:00:00.123456+00" // since = ...00.123456Z
	createdExact = "2026-11-20 10:00:00+00"        // since = ...00Z (RFC 3339 trims zeros)
)

// wantRow is the expected upstream row for a fork row.
type wantRow struct {
	kind, status, endReason string // endReason "" means NULL
	requester               string
	title                   string
	cond                    map[string]any // nil means NULL
}

func since(frac bool) string {
	if frac {
		return "2026-11-20T10:00:00.123456Z"
	}
	return "2026-11-20T10:00:00Z"
}

func replyCond(issue, ident string, frac bool, extra map[string]any) map[string]any {
	c := map[string]any{"type": "issue_reply", "issue_id": issue, "since": since(frac)}
	if ident != "" {
		c["identifier"] = ident
	}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

var longInstruction = strings.Repeat("0123456789", 14) // 140 chars

// forkSeedSet covers every kind and status and every fork path to a terminal
// status (N1a seed set), plus an empty title, an orphan and the requester
// fallbacks. The first 17 entries are the plan's list in order.
func forkSeedSet() ([]forkRow, map[string]wantRow) {
	ident := fmt.Sprintf("%s-%d", issuePrefix, issueNumber)
	rows := []forkRow{
		{label: "at_pending", id: wid(0x7001), createdByTask: taskInit, title: "at pending", instruction: "do at", fireAt: "now() - interval '1 hour'", status: "pending", createdAt: createdExact},
		{label: "watch_status_pending", id: wid(0x7002), createdByTask: taskOrig, title: "w status", instruction: "do status", fireAt: "now() + interval '2 days'", status: "pending", createdAt: createdExact, issue: issueOne, until: "done", onTimeout: "wake"},
		{label: "watch_member_actor_pending", id: wid(0x7003), createdByTask: taskInit, title: "w member actor", instruction: "do m1", fireAt: "now() + interval '2 days'", status: "pending", createdAt: createdFrac, issue: issueOne, eventType: "comment.created", actorType: "member", actorID: userMem, onTimeout: "wake"},
		{label: "watch_member_noactor_pending", id: wid(0x7004), createdByTask: taskInit, title: "w member", instruction: "do m2", fireAt: "now() + interval '2 days'", status: "pending", createdAt: createdFrac, issue: issueOne, eventType: "comment.created", actorType: "member"},
		{label: "watch_agent_actor_pending", id: wid(0x7005), createdByTask: taskInit, title: "w agent actor", instruction: "do a1", fireAt: "now() + interval '2 days'", status: "pending", createdAt: createdFrac, issue: issueOne, eventType: "comment.created", actorType: "agent", actorID: otherAgt, onTimeout: "wake"},
		{label: "watch_agent_noactor_pending", id: wid(0x7006), createdByTask: taskInit, title: "w agent", instruction: "do a2", fireAt: "now() - interval '1 hour'", status: "pending", createdAt: createdFrac, issue: issueOne, eventType: "comment.created", actorType: "agent", onTimeout: "wake"},
		{label: "watch_any_pending", id: wid(0x7007), createdByTask: taskInit, title: "w any", instruction: "do any", fireAt: "now() + interval '2 days'", status: "pending", createdAt: createdExact, issue: issueOne, eventType: "comment.created"},
		{label: "at_fired", id: wid(0x7008), createdByTask: taskInit, title: "at fired", instruction: "fired at", fireAt: "now() - interval '3 days'", status: "fired", firedTask: wid(0x8001), createdAt: createdExact, resolvedAt: "now() - interval '3 days'"},
		{label: "at_cancelled", id: wid(0x7009), createdByTask: taskNone, title: "at cancelled", instruction: "cancelled at", fireAt: "now() + interval '3 days'", status: "cancelled", createdAt: createdExact, resolvedAt: "now() - interval '2 days'"},
		{label: "watch_fired_before_deadline", id: wid(0x700a), createdByTask: taskInit, title: "w fired early", instruction: "fired early", fireAt: "now() + interval '1 day'", status: "fired", firedTask: wid(0x8002), createdAt: createdFrac, resolvedAt: "now() - interval '2 days'", issue: issueOne, eventType: "comment.created", actorType: "member", actorID: userMem, onTimeout: "wake"},
		{label: "watch_fired_after_deadline", id: wid(0x700b), createdByTask: taskInit, title: "w fired late", instruction: "fired late", fireAt: "now() - interval '2 days'", status: "fired", firedTask: wid(0x8003), createdAt: createdFrac, resolvedAt: "now() - interval '1 day'", issue: issueOne, eventType: "comment.created", onTimeout: "drop"},
		{label: "watch_fired_at_deadline_wake", id: wid(0x700c), createdByTask: taskInit, title: "w deadline wake", instruction: "deadline wake", fireAt: "now() - interval '2 days'", status: "fired", firedTask: wid(0x8004), createdAt: createdFrac, resolvedAt: "now() - interval '2 days'", issue: issueOne, until: "done", onTimeout: "wake"},
		{label: "watch_cancelled_before_deadline", id: wid(0x700d), createdByTask: taskInit, title: "w cancel early", instruction: "cancel early", fireAt: "now() + interval '1 day'", status: "cancelled", createdAt: createdFrac, resolvedAt: "now() - interval '1 day'", issue: issueOne, until: "done", onTimeout: "wake"},
		{label: "watch_cancelled_after_deadline", id: wid(0x700e), createdByTask: taskInit, title: "w cancel late", instruction: "cancel late", fireAt: "now() - interval '3 days'", status: "cancelled", createdAt: createdFrac, resolvedAt: "now() - interval '1 day'", issue: issueOne, eventType: "comment.created", actorType: "agent", onTimeout: "wake"},
		{label: "watch_dropped_at_deadline", id: wid(0x700f), createdByTask: taskInit, title: "w dropped", instruction: "dropped", fireAt: "now() - interval '2 days'", status: "cancelled", createdAt: createdFrac, resolvedAt: "now() - interval '2 days'", issue: issueOne, eventType: "comment.created", onTimeout: "drop"},
		{label: "empty_title", id: wid(0x7010), title: "", instruction: longInstruction, fireAt: "now() + interval '1 day'", status: "pending", createdAt: createdExact},
		{label: "orphan_session_gone", id: wid(0x7011), session: sessGone, createdByTask: taskInit, title: "orphan", instruction: "never fires", fireAt: "now() + interval '1 day'", status: "pending", createdAt: createdExact},
		{label: "watch_issue_gone", id: wid(0x7012), createdByTask: taskInit, title: "w gone", instruction: "gone issue", fireAt: "now() + interval '2 days'", status: "pending", createdAt: createdExact, issue: issueGone, until: "done", onTimeout: "wake"},
	}
	want := map[string]wantRow{
		"at_pending":                      {kind: "at", status: "active", requester: userInit, title: "at pending"},
		"watch_status_pending":            {kind: "condition", status: "active", requester: userOrig, title: "w status", cond: map[string]any{"type": "issue", "issue_id": issueOne, "identifier": ident, "status": "done"}},
		"watch_member_actor_pending":      {kind: "condition", status: "active", requester: userInit, title: "w member actor", cond: replyCond(issueOne, ident, true, map[string]any{"author_type": "member", "author_id": userMem, "author_name": memberName})},
		"watch_member_noactor_pending":    {kind: "condition", status: "active", requester: userInit, title: "w member", cond: replyCond(issueOne, ident, true, map[string]any{"author_type": "member"})},
		"watch_agent_actor_pending":       {kind: "condition", status: "active", requester: userInit, title: "w agent actor", cond: replyCond(issueOne, ident, true, map[string]any{"author_type": "agent", "author_id": otherAgt})},
		"watch_agent_noactor_pending":     {kind: "condition", status: "active", requester: userInit, title: "w agent", cond: replyCond(issueOne, ident, true, map[string]any{"author_type": "agent"})},
		"watch_any_pending":               {kind: "condition", status: "active", requester: userInit, title: "w any", cond: replyCond(issueOne, ident, false, map[string]any{"author_type": "any"})},
		"at_fired":                        {kind: "at", status: "fired", endReason: "time", requester: userInit, title: "at fired"},
		"at_cancelled":                    {kind: "at", status: "cancelled", endReason: "fork_migrated", requester: userOwner, title: "at cancelled"},
		"watch_fired_before_deadline":     {kind: "condition", status: "fired", endReason: "fork_migrated", requester: userInit, title: "w fired early", cond: replyCond(issueOne, ident, true, map[string]any{"author_type": "member", "author_id": userMem, "author_name": memberName})},
		"watch_fired_after_deadline":      {kind: "condition", status: "fired", endReason: "fork_migrated", requester: userInit, title: "w fired late", cond: replyCond(issueOne, ident, true, map[string]any{"author_type": "any"})},
		"watch_fired_at_deadline_wake":    {kind: "condition", status: "fired", endReason: "fork_migrated", requester: userInit, title: "w deadline wake", cond: map[string]any{"type": "issue", "issue_id": issueOne, "identifier": ident, "status": "done"}},
		"watch_cancelled_before_deadline": {kind: "condition", status: "cancelled", endReason: "fork_migrated", requester: userInit, title: "w cancel early", cond: map[string]any{"type": "issue", "issue_id": issueOne, "identifier": ident, "status": "done"}},
		"watch_cancelled_after_deadline":  {kind: "condition", status: "cancelled", endReason: "fork_migrated", requester: userInit, title: "w cancel late", cond: replyCond(issueOne, ident, true, map[string]any{"author_type": "agent"})},
		"watch_dropped_at_deadline":       {kind: "condition", status: "cancelled", endReason: "fork_migrated", requester: userInit, title: "w dropped", cond: replyCond(issueOne, ident, true, map[string]any{"author_type": "any"})},
		"empty_title":                     {kind: "at", status: "active", requester: userOwner, title: longInstruction[:120]},
		"watch_issue_gone":                {kind: "condition", status: "active", requester: userInit, title: "w gone", cond: map[string]any{"type": "issue", "issue_id": issueGone, "status": "done"}},
	}
	return rows, want
}

// seedCounts derives the three conversion counts from the seed table.
func seedCounts(rows []forkRow) (active, terminal, dropped int) {
	for _, r := range rows {
		switch {
		case r.session == sessGone:
			dropped++
		case r.status == "pending":
			active++
		default:
			terminal++
		}
	}
	return
}

func seedForkDatabase(t *testing.T, pool *pgxpool.Pool) []forkRow {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO workspace (id, name, slug, issue_prefix) VALUES ($1, 'W', 'che10195-w', $2)`, wsID, issuePrefix)
	for i, u := range []string{userOwner, userMem, userOrig, userInit} {
		name := fmt.Sprintf("User %d", i)
		if u == userMem {
			name = memberName
		}
		exec(`INSERT INTO "user" (id, name, email) VALUES ($1, $2, $3)`, u, name, fmt.Sprintf("u%d@che10195.test", i))
		exec(`INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, wsID, u)
	}
	for _, a := range []string{agentID, otherAgt} {
		exec(`INSERT INTO agent (id, workspace_id, name, runtime_mode) VALUES ($1, $2, $3, 'local')`, a, wsID, "agent-"+a[len(a)-4:])
	}
	exec(`INSERT INTO chat_session (id, workspace_id, agent_id, creator_id) VALUES ($1, $2, $3, $4)`, sessOne, wsID, agentID, userOwner)
	exec(`INSERT INTO issue (id, workspace_id, title, creator_type, creator_id, number) VALUES ($1, $2, 'watched', 'member', $3, $4)`, issueOne, wsID, userOwner, issueNumber)
	exec(`INSERT INTO agent_task_queue (id, agent_id, initiator_user_id, originator_user_id, accountable_user_id, status, completed_at) VALUES ($1, $2, $3, $4, $4, 'completed', now())`, taskInit, agentID, userInit, userOrig)
	exec(`INSERT INTO agent_task_queue (id, agent_id, originator_user_id, accountable_user_id, status, completed_at) VALUES ($1, $2, $3, $3, 'completed', now())`, taskOrig, agentID, userOrig)
	exec(`INSERT INTO agent_task_queue (id, agent_id, status, completed_at) VALUES ($1, $2, 'completed', now())`, taskNone, agentID)

	rows, _ := forkSeedSet()
	for _, r := range rows {
		sess := sessOne
		if r.session != "" {
			sess = r.session
		}
		nul := func(v string) any {
			if v == "" {
				return nil
			}
			return v
		}
		onTimeout := r.onTimeout
		if onTimeout == "" {
			onTimeout = "drop"
		}
		resolved := "NULL"
		if r.resolvedAt != "" {
			resolved = r.resolvedAt
		}
		exec(fmt.Sprintf(`INSERT INTO chat_wakeup (id, workspace_id, chat_session_id, agent_id, title, instruction, fire_at, status,
				created_by_task_id, fired_task_id, created_at, resolved_at, issue_id, event_type, filter_actor_type, filter_actor_id, until_status, on_timeout)
			VALUES ($1, $2, $3, $4, $5, $6, %s, $7, $8, $9, $10::timestamptz, %s, $11, $12, $13, $14, $15, $16)`, r.fireAt, resolved),
			r.id, wsID, sess, agentID, r.title, r.instruction, r.status,
			nul(r.createdByTask), nul(r.firedTask), r.createdAt, nul(r.issue), r.eventType, r.actorType, nul(r.actorID), r.until, onTimeout)
		if r.firedTask != "" {
			// the fired input: role user, task_id = fired_task_id, same session
			exec(`INSERT INTO chat_message (id, chat_session_id, role, content, task_id) VALUES (gen_random_uuid(), $1, 'user', $2, $3)`, sess, "wake: "+r.instruction, r.firedTask)
			// decoy: the turn's reply must not become a wakeup input
			exec(`INSERT INTO chat_message (id, chat_session_id, role, content, task_id) VALUES (gen_random_uuid(), $1, 'assistant', 'reply', $2)`, sess, r.firedTask)
		}
	}
	// decoy: an ordinary user message on an unrelated task
	exec(`INSERT INTO chat_message (id, chat_session_id, role, content, task_id) VALUES (gen_random_uuid(), $1, 'user', 'hello', $2)`, sessOne, taskInit)
	return rows
}

// ---- run helpers ------------------------------------------------------------

// upgradeFixture builds the pre-X fork database, seeds it (or not), applies a
// stand-in for the S1 rename (CHE-7535, not in this base) and returns the pool.
func upgradeFixture(t *testing.T, seed bool) (*pgxpool.Pool, string) {
	t.Helper()
	pool, name := wakeupScratchDatabase(t)
	applyUpFiles(t, pool, wakeupUpFiles(t, chatWakeupXStem))
	if seed {
		seedForkDatabase(t, pool)
	}
	// Stand-in for S1's conversion (CHE-7535 owns it): the fork's
	// chat_task_supplement would stop 99zzz01 before the chat_wakeup files.
	if _, err := pool.Exec(context.Background(), `ALTER TABLE chat_task_supplement RENAME TO chat_task_supplement_fork`); err != nil {
		t.Fatalf("S1 stand-in: %v", err)
	}
	return pool, name
}

func migrateUpOK(t *testing.T, name string, extraEnv ...string) string {
	t.Helper()
	out, err := runMigrateUp(t, name, extraEnv...)
	if err != nil {
		t.Fatalf("migrate up: %v\n%s", err, tail(out, 4000))
	}
	return out
}

func attemptEnv(attempt string) []string {
	return []string{"PGOPTIONS=-c multica.contract_attempt=" + attempt}
}

// ---- tests ------------------------------------------------------------------

// Contract line (P5). N5r's parser test holds the same line verbatim.
func TestChatWakeupForkConvertContractLine(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", chatWakeupXStem+".up.sql"))
	if err != nil {
		t.Fatalf("read X: %v", err)
	}
	first, _, _ := strings.Cut(string(b), "\n")
	const want = "-- multica:contract snapshot=chat_wakeup restore=server/cmd/migrate/recovery/chat_wakeup_fork_restore.sql"
	if first != want {
		t.Fatalf("X first line = %q, want %q", first, want)
	}
	// transaction control statements; a plpgsql block's own BEGIN is not one
	txControl := regexp.MustCompile(`(?im)^\s*(begin\s*(;|work|transaction)|start\s+transaction|commit|rollback|savepoint|release\s+savepoint)\b`)
	if m := txControl.FindString(string(b)); m != "" {
		t.Fatalf("X holds transaction control: %q", strings.TrimSpace(m))
	}
}

// Test 1: seed, migrate up, assert the converted rows and the counts.
func TestChatWakeupForkConvertUpgrade(t *testing.T) {
	pool, name := upgradeFixture(t, true)
	ctx := context.Background()
	seedRows, want := forkSeedSet()

	// fork values read back before X, to compare timestamps exactly
	type forkTimes struct {
		fireAt, createdAt time.Time
		resolvedAt        *time.Time
	}
	fork := map[string]forkTimes{}
	for _, r := range seedRows {
		var ft forkTimes
		if err := pool.QueryRow(ctx, `SELECT fire_at, created_at, resolved_at FROM chat_wakeup WHERE id=$1`, r.id).Scan(&ft.fireAt, &ft.createdAt, &ft.resolvedAt); err != nil {
			t.Fatal(err)
		}
		fork[r.label] = ft
	}
	var before time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	out := migrateUpOK(t, name)
	var after time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&after); err != nil {
		t.Fatal(err)
	}

	active, terminal, dropped := seedCounts(seedRows)
	if active+terminal+dropped != len(seedRows) {
		t.Fatalf("seed counts %d+%d+%d != %d rows", active, terminal, dropped, len(seedRows))
	}
	notice := fmt.Sprintf("converted_active=%d converted_terminal=%d dropped_orphan=%d", active, terminal, dropped)
	if !strings.Contains(out, notice) {
		t.Fatalf("X NOTICE with the three counts %q not in migrate output:\n%s", notice, tail(out, 2500))
	}

	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_wakeup`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != active+terminal {
		t.Fatalf("chat_wakeup holds %d rows, want converted %d (orphans dropped)", total, active+terminal)
	}
	if n := tableCount(t, pool, "chat_wakeup_fork_old"); n != -1 {
		t.Fatalf("fork table left behind (%d rows)", n)
	}

	for _, r := range seedRows {
		w, converted := want[r.label]
		var (
			kind, status                                  string
			endReason                                     *string
			requester                                     string
			sourceTask, sourceMsg                         *string
			title, instruction, condState                 string
			cond                                          []byte
			interval                                      *int64
			fireAt, nextEval, expires, endedAt, lastFired *time.Time
			lastTask                                      *string
			checkCount                                    int
			updatedAt, createdAt                          time.Time
			lastNote, lastErr                             *string
			lastChecked                                   *time.Time
			lastCheckin                                   *string
		)
		err := pool.QueryRow(ctx, `SELECT kind, status, end_reason, requester_id::text, source_task_id::text, source_message_id::text,
				title, instruction, condition_state, condition, interval_seconds, fire_at, next_eval_at, expires_at, ended_at,
				last_task_id::text, last_fired_at, check_count, updated_at, created_at, last_check_note, last_error, last_checked_at, last_checkin_task_id::text
			FROM chat_wakeup WHERE id=$1`, r.id).Scan(&kind, &status, &endReason, &requester, &sourceTask, &sourceMsg,
			&title, &instruction, &condState, &cond, &interval, &fireAt, &nextEval, &expires, &endedAt,
			&lastTask, &lastFired, &checkCount, &updatedAt, &createdAt, &lastNote, &lastErr, &lastChecked, &lastCheckin)
		if !converted {
			if err == nil {
				t.Errorf("%s: orphan row was converted", r.label)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: converted row missing: %v", r.label, err)
			continue
		}
		ft := fork[r.label]
		if kind != w.kind || status != w.status {
			t.Errorf("%s: kind/status = %s/%s, want %s/%s", r.label, kind, status, w.kind, w.status)
		}
		if got := derefStr(endReason); got != w.endReason {
			t.Errorf("%s: end_reason = %q, want %q", r.label, got, w.endReason)
		}
		if requester != w.requester {
			t.Errorf("%s: requester_id = %s, want %s", r.label, requester, w.requester)
		}
		if title != w.title || instruction != r.instruction {
			t.Errorf("%s: title/instruction = %q/%q, want %q/%q", r.label, title, instruction, w.title, r.instruction)
		}
		if want := r.createdByTask; derefStr(sourceTask) != want {
			t.Errorf("%s: source_task_id = %q, want %q", r.label, derefStr(sourceTask), want)
		}
		if sourceMsg != nil || interval != nil || lastNote != nil || lastErr != nil || lastChecked != nil || lastCheckin != nil || condState != "" || checkCount != 0 {
			t.Errorf("%s: columns that must stay NULL/default are set", r.label)
		}
		if !createdAt.Equal(ft.createdAt) {
			t.Errorf("%s: created_at changed", r.label)
		}
		if !expires.Equal(ft.fireAt) {
			t.Errorf("%s: expires_at = %v, want fork fire_at %v", r.label, expires, ft.fireAt)
		}
		// condition
		if w.cond == nil {
			if cond != nil {
				t.Errorf("%s: condition = %s, want NULL", r.label, cond)
			}
		} else {
			var got map[string]any
			if err := json.Unmarshal(cond, &got); err != nil {
				t.Errorf("%s: condition %s: %v", r.label, cond, err)
			} else if !reflect.DeepEqual(got, w.cond) {
				t.Errorf("%s: condition = %v, want %v", r.label, got, w.cond)
			}
		}
		// fire_at: kind at keeps the fork value, condition is NULL
		if w.kind == "at" {
			if fireAt == nil || !fireAt.Equal(ft.fireAt) {
				t.Errorf("%s: fire_at = %v, want %v", r.label, fireAt, ft.fireAt)
			}
		} else if fireAt != nil {
			t.Errorf("%s: condition row has fire_at %v", r.label, fireAt)
		}
		// next_eval_at, ended_at, last_*, updated_at
		switch {
		case status == "active" && kind == "at":
			if !nextEval.Equal(ft.fireAt) {
				t.Errorf("%s: next_eval_at = %v, want fork fire_at", r.label, nextEval)
			}
		case status == "active":
			if nextEval.Before(before.Add(-time.Minute)) || nextEval.After(after.Add(time.Second)) {
				t.Errorf("%s: next_eval_at = %v, want the migration's now() (%v..%v)", r.label, nextEval, before, after)
			}
		default:
			if ft.resolvedAt == nil || !nextEval.Equal(*ft.resolvedAt) || endedAt == nil || !endedAt.Equal(*ft.resolvedAt) {
				t.Errorf("%s: terminal next_eval_at/ended_at = %v/%v, want resolved_at", r.label, nextEval, endedAt)
			}
		}
		if status == "fired" {
			if derefStr(lastTask) != r.firedTask || lastFired == nil || !lastFired.Equal(*ft.resolvedAt) {
				t.Errorf("%s: last_task_id/last_fired_at = %v/%v", r.label, derefStr(lastTask), lastFired)
			}
		} else if lastTask != nil || lastFired != nil {
			t.Errorf("%s: last_task_id/last_fired_at set on a %s row", r.label, status)
		}
		wantUpdated := ft.createdAt
		if ft.resolvedAt != nil {
			wantUpdated = *ft.resolvedAt
		}
		if !updatedAt.Equal(wantUpdated) {
			t.Errorf("%s: updated_at = %v, want %v", r.label, updatedAt, wantUpdated)
		}
	}

	// The fired input rows (and only those) are wakeup turns now.
	var wakeupInputs, decoys int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_message m JOIN chat_wakeup w ON w.id = m.wakeup_id
		WHERE m.message_kind = 'wakeup' AND m.role = 'user' AND m.task_id = w.last_task_id AND m.chat_session_id = w.chat_session_id`).Scan(&wakeupInputs); err != nil {
		t.Fatal(err)
	}
	if wantInputs := 4; wakeupInputs != wantInputs {
		t.Errorf("%d wakeup input rows, want %d", wakeupInputs, wantInputs)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_message WHERE wakeup_id IS NOT NULL OR message_kind = 'wakeup'`).Scan(&decoys); err != nil {
		t.Fatal(err)
	}
	if decoys != 4 {
		t.Errorf("%d rows are marked as wakeup input, want exactly 4 (replies and other messages stay)", decoys)
	}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// tableCount returns the row count of a table, or -1 when it does not exist.
func tableCount(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		return -1
	}
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// wakeupSchemaSignature describes everything X owns: chat_wakeup, its
// indexes, chat_message.wakeup_id and the conversion table.
func wakeupSchemaSignature(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	ctx := context.Background()
	var sig []string
	collect := func(label, q string, args ...any) {
		rows, err := pool.Query(ctx, q, args...)
		if err != nil {
			t.Fatalf("signature %s: %v", label, err)
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			sig = append(sig, label+" "+s)
		}
	}
	collect("col", `SELECT table_name||'.'||column_name||' '||data_type||' '||is_nullable||' '||coalesce(column_default,'')
		FROM information_schema.columns WHERE table_schema='public'
		  AND (table_name IN ('chat_wakeup','chat_wakeup_fork_conversion') OR (table_name='chat_message' AND column_name='wakeup_id'))
		ORDER BY table_name, ordinal_position`)
	collect("check", `SELECT conrelid::regclass::text||' '||contype::text||' '||regexp_replace(pg_get_constraintdef(oid), '\s+', ' ', 'g')
		FROM pg_constraint WHERE conrelid IN (SELECT oid FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relname IN ('chat_wakeup','chat_wakeup_fork_conversion')) ORDER BY 1`)
	collect("index", `SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND tablename IN ('chat_wakeup','chat_wakeup_fork_conversion') ORDER BY indexdef`)
	return sig
}

// Test 2: fresh install (the fork table exists from 701 and is empty).
func TestChatWakeupForkConvertFreshInstall(t *testing.T) {
	freshPool, freshName := upgradeFixture(t, false)
	upPool, upName := upgradeFixture(t, true)
	migrateUpOK(t, freshName)
	migrateUpOK(t, upName)
	ctx := context.Background()

	if n := tableCount(t, freshPool, "chat_wakeup"); n != 0 {
		t.Fatalf("fresh install: chat_wakeup rows = %d, want 0", n)
	}
	var rows int
	var digest string
	if err := freshPool.QueryRow(ctx, `SELECT fork_rows, fork_digest FROM chat_wakeup_fork_conversion`).Scan(&rows, &digest); err != nil {
		t.Fatalf("fresh install: conversion record: %v", err)
	}
	var emptyDigest string
	if err := freshPool.QueryRow(ctx, `SELECT md5('')`).Scan(&emptyDigest); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || digest != emptyDigest {
		t.Fatalf("fresh install: fork_rows/digest = %d/%s, want 0/%s", rows, digest, emptyDigest)
	}
	if got, want := wakeupSchemaSignature(t, freshPool), wakeupSchemaSignature(t, upPool); !reflect.DeepEqual(got, want) {
		t.Fatalf("fresh install differs from upgrade:\nfresh:   %s\nupgrade: %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for _, idx := range []string{"idx_chat_wakeup_pending", "idx_chat_wakeup_fired_task"} {
		var n int
		if err := freshPool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname=$1`, idx).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("fork index %s survived the conversion", idx)
		}
	}
}

// X on a database with no fork chat_wakeup table: 0 rows, digest of no rows.
func TestChatWakeupForkConvertNoForkTable(t *testing.T) {
	pool, name := upgradeFixture(t, false)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE chat_wakeup`); err != nil {
		t.Fatal(err)
	}
	migrateUpOK(t, name)
	var rows int
	var digest, emptyDigest string
	if err := pool.QueryRow(ctx, `SELECT fork_rows, fork_digest, md5('') FROM chat_wakeup_fork_conversion`).Scan(&rows, &digest, &emptyDigest); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || digest != emptyDigest {
		t.Fatalf("fork_rows/digest = %d/%s, want 0/%s", rows, digest, emptyDigest)
	}
}

// copyForkSnapshot copies the fork table in the COPY text format N5a writes
// and loads it into a scratch table with the 980 columns.
func copyForkSnapshot(t *testing.T, pool *pgxpool.Pool, scratch string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `CREATE TABLE `+scratch+` (LIKE chat_wakeup INCLUDING DEFAULTS)`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := conn.Conn().PgConn().CopyTo(ctx, &buf, `COPY chat_wakeup TO STDOUT`); err != nil {
		t.Fatalf("copy out: %v", err)
	}
	if _, err := conn.Conn().PgConn().CopyFrom(ctx, &buf, `COPY `+scratch+` FROM STDIN`); err != nil {
		t.Fatalf("copy in: %v", err)
	}
}

// Test 4: the conversion record and the digest.
func TestChatWakeupForkConvertRecord(t *testing.T) {
	pool, name := upgradeFixture(t, true)
	ctx := context.Background()
	rows, _ := forkSeedSet()
	copyForkSnapshot(t, pool, "chat_wakeup_snapshot_scratch")
	migrateUpOK(t, name, attemptEnv("t4")...)

	var n int
	var attempt *string
	var forkRows int
	var forkDigest string
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_wakeup_fork_conversion`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("conversion rows = %d (%v), want 1", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT attempt_id, fork_rows, fork_digest FROM chat_wakeup_fork_conversion`).Scan(&attempt, &forkRows, &forkDigest); err != nil {
		t.Fatal(err)
	}
	if attempt == nil || *attempt != "t4" {
		t.Fatalf("attempt_id = %v, want t4", derefStr(attempt))
	}
	if forkRows != len(rows) {
		t.Fatalf("fork_rows = %d, want the seed count %d", forkRows, len(rows))
	}
	var want string
	if err := pool.QueryRow(ctx, `SELECT chat_wakeup_fork_digest('chat_wakeup_snapshot_scratch'::regclass)`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if forkDigest != want {
		t.Fatalf("fork_digest = %s, want chat_wakeup_fork_digest(snapshot) = %s", forkDigest, want)
	}

	// Digest properties: stable under row order and session settings, moved by
	// a content change, and the function refuses another column list.
	if _, err := pool.Exec(ctx, `CREATE TABLE wakeup_digest_reorder (LIKE chat_wakeup_snapshot_scratch INCLUDING DEFAULTS);
		INSERT INTO wakeup_digest_reorder SELECT * FROM chat_wakeup_snapshot_scratch ORDER BY id DESC`); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, set := range []string{`SET TimeZone = 'Asia/Ho_Chi_Minh'`, `SET DateStyle = 'SQL, DMY'`, `SET IntervalStyle = 'iso_8601'`, `SET extra_float_digits = -3`} {
		if _, err := conn.Exec(ctx, set); err != nil {
			t.Fatal(err)
		}
	}
	var got string
	if err := conn.QueryRow(ctx, `SELECT chat_wakeup_fork_digest('wakeup_digest_reorder'::regclass)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("digest changed with row order or session settings: %s != %s", got, want)
	}
	if _, err := conn.Exec(ctx, `UPDATE wakeup_digest_reorder SET on_timeout = CASE on_timeout WHEN 'drop' THEN 'wake' ELSE 'drop' END WHERE id = $1`, rows[0].id); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT chat_wakeup_fork_digest('wakeup_digest_reorder'::regclass)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == want {
		t.Fatalf("one changed on_timeout left the digest unchanged")
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE wakeup_digest_wrong (id uuid PRIMARY KEY, extra text)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT chat_wakeup_fork_digest('wakeup_digest_wrong'::regclass)`).Scan(&got); err == nil {
		t.Fatalf("digest accepted a table with another column list")
	}
}

// The chat_wakeup that X creates equals the one 99zzz15 alone creates on an
// empty database (columns, types, defaults, checks, indexes), so a skipped
// 99zzz15 hides no drift.
func TestChatWakeupForkConvertMatchesUpstreamTable(t *testing.T) {
	xPool, xName := upgradeFixture(t, false)
	migrateUpOK(t, xName)

	upstream, _ := wakeupScratchDatabase(t)
	ctx := context.Background()
	if _, err := upstream.Exec(ctx, `CREATE TABLE chat_message (id uuid PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, stem := range []string{"99zzz15_chat_wakeup", "99zzz16_chat_wakeup_due_idx", "99zzz17_chat_wakeup_session_idx", "99zzz18_chat_wakeup_requester_idx"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", stem+".up.sql"))
		if err != nil {
			t.Fatal(err)
		}
		// the index files are CONCURRENTLY: run them outside a transaction
		sql := strings.ReplaceAll(string(b), "CONCURRENTLY ", "")
		if _, err := upstream.Exec(ctx, sql); err != nil {
			t.Fatalf("%s alone: %v", stem, err)
		}
	}
	pick := func(sig []string, prefixes ...string) []string {
		var out []string
		for _, s := range sig {
			for _, p := range prefixes {
				if strings.HasPrefix(s, p) {
					out = append(out, s)
				}
			}
		}
		return out
	}
	x := wakeupSchemaSignature(t, xPool)
	u := wakeupSchemaSignature(t, upstream)
	// the upstream-alone database has no conversion table; compare chat_wakeup only
	filter := func(sig []string) []string {
		var out []string
		for _, s := range pick(sig, "col chat_wakeup.", "check chat_wakeup ", "index ") {
			if strings.Contains(s, "chat_wakeup_fork_conversion") {
				continue
			}
			out = append(out, s)
		}
		return out
	}
	if !reflect.DeepEqual(filter(x), filter(u)) {
		t.Fatalf("X's chat_wakeup differs from 99zzz15 alone:\nX:        %s\nupstream: %s", strings.Join(filter(x), "\n  "), strings.Join(filter(u), "\n  "))
	}
}

// Dispatch eligibility. The dispatcher pass itself needs the server packages,
// which do not build at the C2c3 base (go.mod and sqlc output belong to later
// slices; step 8, CHE-9434, runs the full pass). This test runs the SQL of
// upstream's due scan (ListDueChatWakeupIDs) over the converted rows.
func TestChatWakeupForkConvertDueScan(t *testing.T) {
	pool, name := upgradeFixture(t, true)
	migrateUpOK(t, name)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT id::text FROM chat_wakeup WHERE status = 'active' AND next_eval_at <= now() ORDER BY next_eval_at ASC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got[id] = true
	}
	seed, _ := forkSeedSet()
	wantDue := map[string]bool{
		// overdue `at`
		wid(0x7001): true,
		// pending watches: evaluated at once; the one past its deadline gets its expired turn
		wid(0x7002): true, wid(0x7003): true, wid(0x7004): true, wid(0x7005): true, wid(0x7006): true, wid(0x7007): true, wid(0x7012): true,
	}
	for _, r := range seed {
		if got[r.id] != wantDue[r.id] {
			t.Errorf("%s: due = %v, want %v", r.label, got[r.id], wantDue[r.id])
		}
	}
	var expired int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_wakeup WHERE id = $1 AND status = 'active' AND expires_at <= now()`, wid(0x7006)).Scan(&expired); err != nil || expired != 1 {
		t.Errorf("the watch past its deadline must be active and expired (n=%d, %v)", expired, err)
	}
}

// runMigrateDown runs `migrate down --to <to>` from the built binary: every
// migration newer than `to` is rolled back, `to` stays applied.
func runMigrateDown(t *testing.T, dbName, to string) (string, error) {
	t.Helper()
	config, err := pgxpool.ParseConfig(testDatabaseURL())
	if err != nil {
		t.Fatal(err)
	}
	u := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", config.ConnConfig.User, config.ConnConfig.Password, config.ConnConfig.Host, config.ConnConfig.Port, dbName)
	cmd := exec.Command(migrateBinary(t), "down", "--to", to)
	cmd.Env = append(os.Environ(), "DATABASE_URL="+u)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// forkStateSnapshot reads everything X changes and its down must restore:
// the fork table's rows (text form, ordered by id), its columns, constraints
// and indexes, and message_kind of every chat_message.
func forkStateSnapshot(t *testing.T, pool *pgxpool.Pool) (rows, shape, kinds []string) {
	t.Helper()
	ctx := context.Background()
	read := func(q string) []string {
		t.Helper()
		rs, err := pool.Query(ctx, q)
		if err != nil {
			t.Fatalf("snapshot %q: %v", q, err)
		}
		defer rs.Close()
		var out []string
		for rs.Next() {
			var s string
			if err := rs.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		if err := rs.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	rows = read(`SELECT t::text FROM chat_wakeup t ORDER BY t.id`)
	shape = append(shape, read(`SELECT column_name||' '||data_type||' '||is_nullable||' '||coalesce(column_default,'')
		FROM information_schema.columns WHERE table_schema='public' AND table_name='chat_wakeup' ORDER BY ordinal_position`)...)
	shape = append(shape, read(`SELECT contype::text||' '||regexp_replace(pg_get_constraintdef(oid), '\s+', ' ', 'g')
		FROM pg_constraint WHERE conrelid = 'chat_wakeup'::regclass ORDER BY 1`)...)
	shape = append(shape, read(`SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND tablename='chat_wakeup' ORDER BY indexdef`)...)
	kinds = read(`SELECT id::text||' '||message_kind FROM chat_message ORDER BY id`)
	return
}

// Up, then down, then compare with the pre-up state: the fork table returns
// with every row (the orphan included), its schema and the message kinds.
func TestChatWakeupForkConvertDownRestoresForkTable(t *testing.T) {
	pool, name := upgradeFixture(t, true)
	ctx := context.Background()
	seed, _ := forkSeedSet()
	wantRows, wantShape, wantKinds := forkStateSnapshot(t, pool)
	if len(wantRows) != len(seed) {
		t.Fatalf("pre-up fork rows = %d, want %d", len(wantRows), len(seed))
	}
	var orphans int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_wakeup w WHERE NOT EXISTS (SELECT 1 FROM chat_session s WHERE s.id = w.chat_session_id)`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans == 0 {
		t.Fatal("seed has no row without a chat_session; the orphan case is untested")
	}
	var preDigest string
	if err := pool.QueryRow(ctx, `SELECT md5(coalesce(string_agg(t::text, E'\n' ORDER BY t.id), '')) FROM chat_wakeup t`).Scan(&preDigest); err != nil {
		t.Fatal(err)
	}

	migrateUpOK(t, name)
	if n := tableCount(t, pool, "chat_wakeup_fork_saved"); n != len(seed) {
		t.Fatalf("saved fork rows after up = %d, want %d (orphans included)", n, len(seed))
	}

	// the version just before X: down rolls back X and everything newer
	to := strings.TrimSuffix(filepath.Base(wakeupUpFiles(t, chatWakeupXStem)[len(wakeupUpFiles(t, chatWakeupXStem))-1]), ".up.sql")
	out, err := runMigrateDown(t, name, to)
	if err != nil {
		t.Fatalf("migrate down --to %s: %v\n%s", to, err, tail(out, 4000))
	}
	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version LIKE '99zzz%'`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("%d 99zzz ledger rows left after down", applied)
	}

	gotRows, gotShape, gotKinds := forkStateSnapshot(t, pool)
	if !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("fork rows after down differ from pre-up:\n got %d rows\nwant %d rows\nfirst got:  %v\nfirst want: %v", len(gotRows), len(wantRows), gotRows[:min(2, len(gotRows))], wantRows[:min(2, len(wantRows))])
	}
	if !reflect.DeepEqual(gotShape, wantShape) {
		t.Fatalf("fork table shape after down differs:\n got: %s\nwant: %s", strings.Join(gotShape, "\n  "), strings.Join(wantShape, "\n  "))
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("chat_message kinds after down differ from pre-up")
	}
	var wakeupID int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='chat_message' AND column_name='wakeup_id'`).Scan(&wakeupID); err != nil {
		t.Fatal(err)
	}
	if wakeupID != 0 {
		t.Fatal("chat_message.wakeup_id survived the down")
	}
	// audit record stays; the digest it holds is the pre-up digest and the saved copy is spent
	var recDigest string
	if err := pool.QueryRow(ctx, `SELECT fork_digest FROM chat_wakeup_fork_conversion`).Scan(&recDigest); err != nil {
		t.Fatal(err)
	}
	if recDigest != preDigest {
		t.Fatalf("recorded digest %s != pre-up digest %s", recDigest, preDigest)
	}
	if n := tableCount(t, pool, "chat_wakeup_fork_saved"); n != 0 {
		t.Fatalf("saved rows after down = %d, want 0", n)
	}
	if n := tableCount(t, pool, "chat_wakeup_fork_saved_input"); n != 0 {
		t.Fatalf("saved input rows after down = %d, want 0", n)
	}

	// up again from the restored state converts the same rows
	migrateUpOK(t, name)
	_, want := forkSeedSet()
	active, terminal, dropped := seedCounts(seed)
	if n := tableCount(t, pool, "chat_wakeup"); n != active+terminal || n != len(want) {
		t.Fatalf("second up: chat_wakeup rows = %d, want %d", n, active+terminal)
	}
	var records int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_wakeup_fork_conversion WHERE fork_rows = $1 AND fork_digest = $2`, len(seed), preDigest).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 2 {
		t.Fatalf("conversion records with the pre-up digest = %d, want 2 (dropped=%d)", records, dropped)
	}
}

// A down refuses to drop live upstream rows, and a second down has nothing
// to restore: neither case may leave a half-built fork table.
func TestChatWakeupForkConvertDownRefusals(t *testing.T) {
	pool, name := upgradeFixture(t, true)
	ctx := context.Background()
	migrateUpOK(t, name)
	to := strings.TrimSuffix(filepath.Base(wakeupUpFiles(t, chatWakeupXStem)[len(wakeupUpFiles(t, chatWakeupXStem))-1]), ".up.sql")

	// 99zzz15's down runs first and drops chat_wakeup; run X's own down on a
	// table that still holds rows by recreating the situation by hand.
	down, err := os.ReadFile(filepath.Join("..", "..", "migrations", chatWakeupXStem+".down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "refusing to drop them") {
		t.Fatalf("down over a populated upstream table: err = %v, want refusal", err)
	}
	if n := tableCount(t, pool, "chat_wakeup"); n <= 0 {
		t.Fatalf("refused down changed chat_wakeup: rows = %d", n)
	}
	if n := tableCount(t, pool, "chat_wakeup_fork_saved"); n <= 0 {
		t.Fatalf("refused down consumed the saved rows: %d", n)
	}

	if out, err := runMigrateDown(t, name, to); err != nil {
		t.Fatalf("down: %v\n%s", err, tail(out, 3000))
	}
	// the ledger no longer lists X, so the runner does not run it twice; the
	// file itself refuses a second run over the restored fork table.
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "already has the fork shape") {
		t.Fatalf("second down: err = %v, want refusal", err)
	}
}
