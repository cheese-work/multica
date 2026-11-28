package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-10691 (CHE-7486 N4a, v0.13.0 train): R, the hand-run restore script for
// migration X, rebuilds the fork chat_wakeup from the N5a snapshot and the
// upstream rows written since the deploy. One assertion set (A1-A6) runs
// against a pluggable recovery step: "none" is today's rollback path (the red
// to observe), "script" is R (the green). Choose with WAKEUP_RESTORE_STEP.

const (
	restoreScriptPath = "recovery/chat_wakeup_fork_restore.sql"
	restoreAttempt    = "0123456789abcdef0123456789abcdef"
	restoreAttempt2   = "fedcba9876543210fedcba9876543210"
	restoreNewSince   = "2026-11-27T08:15:30.5Z"
)

// ids of the rows this file adds to N1a's seed set (all fork rows of kind at)
var (
	rsSessTwo      = wid(0x4002)
	rsExtraExpired = wid(0x7103) // pending; a person cancels it after the deploy
	rsExtraMoved   = wid(0x7104) // overdue pending; moved 2 h later after the deploy
	rsExtraFired   = wid(0x7105) // overdue pending; the dispatcher fires it after the deploy
	rsExtraGone    = wid(0x7106) // pending, in a session that is deleted after the deploy
	rsTaskFired1   = wid(0x8101)
	rsTaskFired2   = wid(0x8102)
	rsTaskNew      = wid(0x8103)
	rsWeixinIssue  = wid(0x5002)
)

func rsUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		panic(err)
	}
	return u
}

func rsTS(ts time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: ts, Valid: true} }

func rsURL(t *testing.T, dbName string) string {
	t.Helper()
	config, err := pgxpool.ParseConfig(testDatabaseURL())
	if err != nil {
		t.Fatal(err)
	}
	c := config.ConnConfig
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", c.User, c.Password, c.Host, c.Port, dbName)
}

// ---- the pluggable recovery step ------------------------------------------------

type recoveryStep func(t *testing.T, fx *restoreFixture) (notices []string, err error)

// stepNone is today's rollback path: no data step runs.
func stepNone(t *testing.T, fx *restoreFixture) ([]string, error) { return nil, nil }

// stepScript runs R as an operator would: one connection, the attempt id in
// multica.restore_attempt, the whole file in the simple protocol.
func stepScript(t *testing.T, fx *restoreFixture) ([]string, error) {
	return runRestoreScript(t, fx.name, fx.attempt)
}

func runRestoreScript(t *testing.T, dbName, attempt string) ([]string, error) {
	t.Helper()
	body, err := os.ReadFile(restoreScriptPath)
	if err != nil {
		return nil, fmt.Errorf("read R: %w", err)
	}
	cfg, err := pgx.ParseConfig(rsURL(t, dbName))
	if err != nil {
		t.Fatal(err)
	}
	var notices []string
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notices = append(notices, n.Message) }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background()) // an aborted transaction rolls back here
	if attempt != "" {
		if _, err := conn.Exec(ctx, `SELECT set_config('multica.restore_attempt', $1, false)`, attempt); err != nil {
			t.Fatal(err)
		}
	}
	_, err = conn.PgConn().Exec(ctx, string(body)).ReadAll()
	return notices, err
}

func chosenStep(t *testing.T) (string, recoveryStep) {
	switch v := os.Getenv("WAKEUP_RESTORE_STEP"); v {
	case "", "script":
		return "script", stepScript
	case "none":
		return "none", stepNone
	default:
		t.Fatalf("WAKEUP_RESTORE_STEP=%q: want none or script", v)
		return "", nil
	}
}

// ---- fixture ---------------------------------------------------------------------

type restoreFixture struct {
	pool    *pgxpool.Pool
	name    string
	attempt string
	snap    string // snapshot table name
	saved   string // saved upstream table name

	want    map[string]string // id -> expected outcome, snapshot ids
	wantNew map[string]string // id -> expected outcome, ids created after the deploy
	label   map[string]string // id -> label
	preR    map[string]string // id -> to_jsonb of the upstream row just before R
	ids     map[string]string // label -> id
}

func (fx *restoreFixture) id(label string) string {
	id, ok := fx.ids[label]
	if !ok {
		panic("no such label " + label)
	}
	return id
}

// extraForkRows adds to N1a's seed set (18 rows) four fork rows of kind at.
func extraForkRows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO chat_session (id, workspace_id, agent_id, creator_id) VALUES ($1, $2, $3, $4)`, rsSessTwo, wsID, agentID, userOwner)
	type e struct {
		id, sess, title, fire string
	}
	for _, r := range []e{
		{rsExtraExpired, sessOne, "extra cancelled later", "now() + interval '1 day'"},
		{rsExtraMoved, sessOne, "extra moved", "now() - interval '1 hour'"},
		{rsExtraFired, sessOne, "extra fired", "now() - interval '1 hour'"},
		{rsExtraGone, rsSessTwo, "extra gone", "now() + interval '1 day'"},
	} {
		exec(fmt.Sprintf(`INSERT INTO chat_wakeup (id, workspace_id, chat_session_id, agent_id, title, instruction, fire_at, status, created_by_task_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, %s, 'pending', $7, $8::timestamptz)`, r.fire), r.id, wsID, r.sess, agentID, r.title, "do "+r.title, taskInit, createdExact)
	}
}

// loadSnapshot copies the fork table the way N5a writes it and registers it
// the way the script header documents.
func loadSnapshot(t *testing.T, pool *pgxpool.Pool, attempt string) string {
	t.Helper()
	snap := "chat_wakeup_fork_snapshot_" + attempt
	copyForkSnapshot(t, pool, snap)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS chat_wakeup_fork_snapshot_meta (attempt_id text PRIMARY KEY, rows bigint NOT NULL, sha256 text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO chat_wakeup_fork_snapshot_meta (attempt_id, rows, sha256) SELECT $1, count(*), repeat('0', 64) FROM `+snap, attempt); err != nil {
		t.Fatal(err)
	}
	return snap
}

func (fx *restoreFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := fx.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// buildRestoreFixture runs the fixture in the issue's order: the seed set, the
// snapshot, migrate up, one weixin_chat issue, then the post-deploy writes.
func buildRestoreFixture(t *testing.T, writes bool) *restoreFixture {
	t.Helper()
	pool, name := upgradeFixture(t, true)
	extraForkRows(t, pool)
	seed, _ := forkSeedSet()

	fx := &restoreFixture{pool: pool, name: name, attempt: restoreAttempt,
		want: map[string]string{}, wantNew: map[string]string{}, label: map[string]string{}, ids: map[string]string{}}
	fx.snap = loadSnapshot(t, pool, fx.attempt)
	fx.saved = "chat_wakeup_upstream_saved_" + fx.attempt
	migrateUpOK(t, name, attemptEnv(fx.attempt)...)
	fx.exec(t, `INSERT INTO issue (id, workspace_id, title, creator_type, creator_id, number, origin_type) VALUES ($1, $2, 'weixin', 'member', $3, 43, 'weixin_chat')`, rsWeixinIssue, wsID, userOwner)

	for _, r := range seed {
		fx.ids[r.label] = r.id
	}
	for l, id := range map[string]string{"extra_expired": rsExtraExpired, "extra_moved": rsExtraMoved, "extra_fired": rsExtraFired, "extra_gone": rsExtraGone} {
		fx.ids[l] = id
	}
	for l, id := range fx.ids {
		fx.label[id] = l
	}

	// expected outcomes of the writes below (drift guard: no writes)
	absent := map[string]bool{"orphan_session_gone": true}
	for l := range fx.ids {
		o := "restored_pending"
		if r := seedByLabel(seed, l); r != nil && r.status != "pending" {
			o = "restored_terminal"
		}
		if absent[l] {
			o = "absent"
		}
		fx.want[fx.ids[l]] = o
	}
	if writes {
		postDeployWrites(t, fx)
	}

	rows, err := pool.Query(context.Background(), `SELECT id::text, to_jsonb(c)::text FROM chat_wakeup c`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	fx.preR = map[string]string{}
	for rows.Next() {
		var id, js string
		if err := rows.Scan(&id, &js); err != nil {
			t.Fatal(err)
		}
		fx.preR[id] = js
	}
	return fx
}

func seedByLabel(seed []forkRow, label string) *forkRow {
	for i := range seed {
		if seed[i].label == label {
			return &seed[i]
		}
	}
	return nil
}

func cfgOf(r db.ChatWakeup) db.UpdateChatWakeupConfigParams {
	return db.UpdateChatWakeupConfigParams{
		Title: r.Title, Instruction: r.Instruction, Condition: r.Condition, ConditionState: r.ConditionState,
		IntervalSeconds: r.IntervalSeconds, FireAt: r.FireAt, NextEvalAt: r.NextEvalAt, ExpiresAt: r.ExpiresAt, ID: r.ID,
	}
}

// postDeployWrites are what the upstream image does to the converted rows, and
// what people do through it, written with the upstream queries.
func postDeployWrites(t *testing.T, fx *restoreFixture) {
	t.Helper()
	ctx := context.Background()
	q := db.New(fx.pool)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("post-deploy %s: %v", what, err)
		}
	}
	get := func(label string) db.ChatWakeup {
		t.Helper()
		r, err := q.GetChatWakeup(ctx, rsUUID(fx.id(label)))
		must("get "+label, err)
		return r
	}
	now := time.Now()
	set := func(label, outcome string) { fx.want[fx.id(label)] = outcome }

	// the dispatcher fires two converted pending rows
	for _, f := range []struct{ label, task string }{{"watch_issue_gone", rsTaskFired1}, {"extra_fired", rsTaskFired2}} {
		_, err := q.RecordChatWakeupFire(ctx, db.RecordChatWakeupFireParams{TaskID: rsUUID(f.task), EndStatus: "fired", EndReason: "time", NextEvalAt: rsTS(now), ID: rsUUID(fx.id(f.label))})
		must("fire "+f.label, err)
		set(f.label, "ended_after_deploy")
	}
	// a person cancels one
	_, err := q.EndChatWakeup(ctx, db.EndChatWakeupParams{Status: "cancelled", EndReason: pgtype.Text{String: "user", Valid: true}, ID: rsUUID(fx.id("extra_expired"))})
	must("cancel", err)
	set("extra_expired", "ended_after_deploy")

	// the dispatcher evaluates a wait and the display code names a member: not edits
	must("evaluate", q.SetChatWakeupEvaluation(ctx, db.SetChatWakeupEvaluationParams{ConditionState: "seen", NextEvalAt: rsTS(now.Add(time.Minute)), ID: rsUUID(fx.id("watch_status_pending"))}))
	fx.exec(t, `UPDATE chat_wakeup SET condition = jsonb_set(condition, '{author_name}', '"Renamed"') WHERE id = $1`, fx.id("watch_member_actor_pending"))

	// a converted at timer, overdue at the snapshot, is moved 2 h later
	r := get("extra_moved")
	c := cfgOf(r)
	c.FireAt, c.NextEvalAt, c.ExpiresAt = rsTS(now.Add(2*time.Hour)), rsTS(now.Add(2*time.Hour)), rsTS(now.Add(2*time.Hour))
	_, err = q.UpdateChatWakeupConfig(ctx, c)
	must("move", err)
	set("extra_moved", "from_current")

	// a converted at row gets a new instruction
	r = get("empty_title")
	c = cfgOf(r)
	c.Instruction = "new instruction after deploy"
	_, err = q.UpdateChatWakeupConfig(ctx, c)
	must("instruction", err)
	set("empty_title", "from_current")

	// a converted member-reply wait is edited: its since moves
	r = get("watch_member_noactor_pending")
	var cond map[string]any
	must("condition json", json.Unmarshal(r.Condition, &cond))
	cond["since"] = restoreNewSince
	c = cfgOf(r)
	c.Condition, _ = json.Marshal(cond)
	_, err = q.UpdateChatWakeupConfig(ctx, c)
	must("since", err)
	set("watch_member_noactor_pending", "from_current")

	// a converted issue wait is edited to a pull_request condition
	r = get("watch_any_pending")
	c = cfgOf(r)
	c.Condition = json.RawMessage(`{"type":"pull_request","repo":"o/r","number":5}`)
	_, err = q.UpdateChatWakeupConfig(ctx, c)
	must("pull_request", err)
	set("watch_any_pending", "ended_not_expressible")

	// one converted row's session is deleted
	must("delete wakeups of session", q.DeleteChatWakeupsBySession(ctx, rsUUID(rsSessTwo)))
	fx.exec(t, `DELETE FROM chat_session WHERE id = $1`, rsSessTwo)
	set("extra_gone", "absent")

	// the requester creates one wakeup of every upstream kind and condition
	exp := rsTS(now.Add(24 * time.Hour))
	next := rsTS(now.Add(time.Hour))
	newRow := func(label, kind string, cond string, mod func(*db.CreateChatWakeupParams)) {
		t.Helper()
		id := wid(0x7200 + len(fx.wantNew) + 1)
		p := db.CreateChatWakeupParams{ID: rsUUID(id), WorkspaceID: rsUUID(wsID), ChatSessionID: rsUUID(sessOne), AgentID: rsUUID(agentID), RequesterID: rsUUID(userOwner),
			SourceTaskID: rsUUID(taskInit), Title: "new " + label, Instruction: "do new " + label, Kind: kind, NextEvalAt: next, ExpiresAt: exp}
		if cond != "" {
			p.Condition = json.RawMessage(cond)
		}
		if kind == "at" {
			p.FireAt = rsTS(now.Add(24 * time.Hour))
		}
		if kind == "check" {
			p.IntervalSeconds = pgtype.Int8{Int64: 3600, Valid: true}
		}
		if mod != nil {
			mod(&p)
		}
		_, err := q.CreateChatWakeup(ctx, p)
		must("create "+label, err)
		fx.ids[label] = id
		fx.label[id] = label
		fx.wantNew[id] = "not_restored"
	}
	issue := fmt.Sprintf(`"issue_id":%q`, issueOne)
	newRow("n_at", "at", "", nil)
	newRow("n_issue", "condition", `{"type":"issue",`+issue+`,"status":"done"}`, nil)
	newRow("n_reply_member_author", "condition", `{"type":"issue_reply",`+issue+`,"author_type":"member","author_id":"`+userMem+`","since":"2026-11-28T01:02:03Z"}`, nil)
	newRow("n_reply_no_author_type", "condition", `{"type":"issue_reply",`+issue+`,"since":"2026-11-28T01:02:04.25Z"}`, nil)
	newRow("n_at_fired", "at", "", nil)
	newRow("n_at_cancelled", "at", "", nil)
	newRow("n_reply_any", "condition", `{"type":"issue_reply",`+issue+`,"author_type":"any","since":"2026-11-28T01:02:03Z"}`, nil)
	newRow("n_check", "check", "", nil)
	newRow("n_children_done", "condition", `{"type":"children_done",`+issue+`}`, nil)
	newRow("n_pull_request", "condition", `{"type":"pull_request","repo":"o/r","number":7}`, nil)
	newRow("n_run", "condition", `{"type":"run","task_id":"`+taskInit+`"}`, nil)
	newRow("n_issue_state", "condition", `{"type":"issue",`+issue+`,"state":"open"}`, nil)
	newRow("n_reply_bad_since", "condition", `{"type":"issue_reply",`+issue+`,"author_type":"member","since":"garbage"}`, nil)
	for _, l := range []string{"n_at", "n_issue", "n_reply_member_author", "n_reply_no_author_type", "n_at_fired", "n_at_cancelled"} {
		fx.wantNew[fx.id(l)] = "converted_new"
	}
	_, err = q.RecordChatWakeupFire(ctx, db.RecordChatWakeupFireParams{TaskID: rsUUID(rsTaskNew), EndStatus: "fired", EndReason: "time", NextEvalAt: next, ID: rsUUID(fx.id("n_at_fired"))})
	must("fire new", err)
	_, err = q.EndChatWakeup(ctx, db.EndChatWakeupParams{Status: "cancelled", EndReason: pgtype.Text{String: "user", Valid: true}, ID: rsUUID(fx.id("n_at_cancelled"))})
	must("cancel new", err)
}

// ---- assertions ----------------------------------------------------------------

func outcomeCounts(m ...map[string]string) map[string]int {
	c := map[string]int{}
	for _, mm := range m {
		for _, o := range mm {
			c[o]++
		}
	}
	return c
}

func noticeFor(c map[string]int) string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, c[k]))
	}
	return strings.Join(parts, " ")
}

// The previous image's queries, kept as a fixture (git show f2590543ee~1:server/pkg/db/queries/chat_wakeup.sql).
const (
	prevListDue = `SELECT id::text FROM chat_wakeup WHERE status = 'pending' AND fire_at <= now() ORDER BY fire_at, id LIMIT 100`
	prevClaim   = `UPDATE chat_wakeup SET status = 'fired', resolved_at = now()
		WHERE id = $1 AND status = 'pending' AND (fire_at <= now() OR issue_id IS NOT NULL) RETURNING id::text`
)

func queryStrings(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// assertRestored runs A1-A4 (and A5 when green) against the recovery step's result.
func assertRestored(t *testing.T, fx *restoreFixture, notices []string, green bool) {
	t.Helper()
	ctx := context.Background()
	pool := fx.pool
	wantAll := map[string]string{}
	for k, v := range fx.want {
		wantAll[k] = v
	}
	for k, v := range fx.wantNew {
		wantAll[k] = v
	}

	// A1: the previous image's own due query returns each overdue restored_pending row once; the claim takes it once.
	var dueWant []string
	for id, o := range fx.want {
		l := fx.label[id]
		if o == "restored_pending" && (l == "at_pending" || l == "watch_agent_noactor_pending") {
			dueWant = append(dueWant, id)
		}
	}
	sort.Strings(dueWant)
	due, err := queryStrings(ctx, pool, prevListDue)
	if err != nil {
		t.Errorf("A1: ListDueChatWakeups at P: %v", err)
	} else {
		sort.Strings(due)
		if !slices.Equal(due, dueWant) {
			t.Errorf("A1: ListDueChatWakeups returned %v, want each overdue restored_pending row once: %v", due, dueWant)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range dueWant {
		got, err := queryStrings(ctx, tx, prevClaim, id)
		if err != nil {
			t.Errorf("A1: ClaimChatWakeup at P for %s: %v", fx.label[id], err)
			break
		}
		again, err2 := queryStrings(ctx, tx, prevClaim, id)
		if err2 != nil || len(got) != 1 || len(again) != 0 {
			t.Errorf("A1: claim of %s: first %v, second %v (%v); want exactly once", fx.label[id], got, again, err2)
		}
	}
	_ = tx.Rollback(ctx)

	// A2: the moved timer is not due and cannot be claimed now; the new instruction is there; the edited reply wait starts at the new since.
	moved := fx.id("extra_moved")
	if slices.Contains(due, moved) {
		t.Errorf("A2: the moved timer is due")
	}
	if got, err := queryStrings(ctx, pool, `SELECT id::text FROM chat_wakeup WHERE id = $1 AND status = 'pending' AND fire_at <= now()`, moved); err != nil || len(got) != 0 {
		t.Errorf("A2: moved timer claimable now: %v %v", got, err)
	}
	if got, err := queryStrings(ctx, pool, `SELECT instruction FROM chat_wakeup WHERE id = $1`, fx.id("empty_title")); err != nil || !slices.Equal(got, []string{"new instruction after deploy"}) {
		t.Errorf("A2: instruction of the edited row = %v (%v)", got, err)
	}
	wantSince, _ := time.Parse(time.RFC3339Nano, restoreNewSince)
	var gotSince time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at FROM chat_wakeup WHERE id = $1`, fx.id("watch_member_noactor_pending")).Scan(&gotSince); err != nil || !gotSince.Equal(wantSince) {
		t.Errorf("A2: created_at of the edited reply wait = %v (%v), want since %v", gotSince, err, wantSince)
	}

	// A3: no row for an absent id; nothing that ended after the deploy is pending; the pull_request row is cancelled.
	for id, o := range wantAll {
		switch o {
		case "absent", "not_restored":
			if n, err := queryStrings(ctx, pool, `SELECT id::text FROM chat_wakeup WHERE id = $1`, id); err != nil || len(n) != 0 {
				t.Errorf("A3: %s (%s) has a row: %v %v", fx.label[id], o, n, err)
			}
		case "ended_after_deploy":
			if st, err := queryStrings(ctx, pool, `SELECT status FROM chat_wakeup WHERE id = $1`, id); err != nil || len(st) != 1 || st[0] == "pending" {
				t.Errorf("A3: %s ended after the deploy but its status is %v (%v)", fx.label[id], st, err)
			}
		}
	}
	if st, err := queryStrings(ctx, pool, `SELECT status FROM chat_wakeup WHERE id = $1`, fx.id("watch_any_pending")); err != nil || !slices.Equal(st, []string{"cancelled"}) {
		t.Errorf("A3: pull_request row status = %v (%v), want cancelled", st, err)
	}

	// A4: the report and the counts.
	if !green {
		return
	}
	reportRows, err := pool.Query(ctx, `SELECT id::text, outcome FROM chat_wakeup_restore_report WHERE attempt_id = $1 AND outcome <> 'saved_table_renames'`, fx.attempt)
	if err != nil {
		t.Errorf("A4: report: %v", err)
	} else {
		got := map[string]string{}
		for reportRows.Next() {
			var id, o string
			if err := reportRows.Scan(&id, &o); err != nil {
				t.Fatal(err)
			}
			got[id] = o
		}
		reportRows.Close()
		for id, o := range wantAll {
			if got[id] != o {
				t.Errorf("A4: report outcome of %s = %q, want %q", fx.label[id], got[id], o)
			}
		}
		if len(got) != len(wantAll) {
			t.Errorf("A4: report has %d rows, want %d", len(got), len(wantAll))
		}
	}
	wantNotice := noticeFor(outcomeCounts(fx.want, fx.wantNew))
	if !strings.Contains(strings.Join(notices, "\n"), wantNotice) {
		t.Errorf("A4: NOTICE counts %q not in %q", wantNotice, notices)
	}
	snapRows := len(fx.want)
	var sum int
	for o, n := range outcomeCounts(fx.want) {
		switch o {
		case "restored_pending", "from_current", "ended_not_expressible", "ended_after_deploy", "restored_terminal", "absent":
			sum += n
		default:
			t.Errorf("A4: snapshot id with outcome %s", o)
		}
	}
	if sum != snapRows {
		t.Errorf("A4: snapshot outcomes add to %d, want %d", sum, snapRows)
	}

	// A5: the saved table holds every upstream row, unchanged.
	saved, err := pool.Query(ctx, `SELECT id::text, to_jsonb(c)::text FROM `+fx.saved+` c`)
	if err != nil {
		t.Errorf("A5: saved table: %v", err)
		return
	}
	defer saved.Close()
	n := 0
	for saved.Next() {
		var id, js string
		if err := saved.Scan(&id, &js); err != nil {
			t.Fatal(err)
		}
		n++
		if fx.preR[id] != js {
			t.Errorf("A5: saved row %s differs from the upstream row before R", fx.label[id])
		}
	}
	if n != len(fx.preR) {
		t.Errorf("A5: saved table holds %d rows, want %d", n, len(fx.preR))
	}
}

// fork rows that must equal the snapshot row byte for byte
func assertSnapshotRowsEqual(t *testing.T, fx *restoreFixture, outcomes ...string) {
	t.Helper()
	var ids []string
	for id, o := range fx.want {
		if slices.Contains(outcomes, o) {
			ids = append(ids, id)
		}
	}
	diff, err := queryStrings(context.Background(), fx.pool,
		`SELECT s.id::text FROM `+fx.snap+` s LEFT JOIN chat_wakeup c ON c.id = s.id WHERE s.id = ANY($1::uuid[]) AND to_jsonb(c) IS DISTINCT FROM to_jsonb(s)`, ids)
	if err != nil {
		t.Errorf("compare with snapshot: %v", err)
		return
	}
	for _, id := range diff {
		t.Errorf("row %s (%s) differs from its snapshot row", fx.label[id], fx.want[id])
	}
}

// ---- tests -----------------------------------------------------------------------

// A1-A6 with the post-deploy writes.
func TestChatWakeupForkRestore(t *testing.T) {
	step, run := chosenStep(t)
	fx := buildRestoreFixture(t, true)
	notices, err := run(t, fx)
	if err != nil {
		t.Fatalf("recovery step %s: %v", step, err)
	}
	green := step == "script"
	assertRestored(t, fx, notices, green)
	if !green {
		return
	}
	assertSnapshotRowsEqual(t, fx, "restored_pending", "restored_terminal")
	assertRestoredValues(t, fx)
	assertLedgerAndShape(t, fx)
	assertRollForward(t, fx)
}

// With no post-deploy writes R changes nothing: it proves R's X(S) equals X's real output.
func TestChatWakeupForkRestoreDriftGuard(t *testing.T) {
	if step, _ := chosenStep(t); step != "script" {
		t.Skip("green-only check")
	}
	fx := buildRestoreFixture(t, false)
	if _, err := stepScript(t, fx); err != nil {
		t.Fatal(err)
	}
	for id, o := range fx.want {
		if o != "restored_pending" && o != "restored_terminal" && o != "absent" {
			t.Fatalf("drift guard fixture: %s expects %s", fx.label[id], o)
		}
	}
	rows, err := queryStrings(context.Background(), fx.pool, `SELECT id::text || ' ' || outcome FROM chat_wakeup_restore_report WHERE outcome <> 'saved_table_renames' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for id, o := range fx.want {
		want = append(want, id+" "+o)
	}
	sort.Strings(want)
	if !slices.Equal(rows, want) {
		t.Fatalf("drift: report = %v, want %v", rows, want)
	}
	assertSnapshotRowsEqual(t, fx, "restored_pending", "restored_terminal")
	if n := tableCount(t, fx.pool, "chat_wakeup"); n != len(fx.want)-1 {
		t.Fatalf("chat_wakeup holds %d rows, want %d", n, len(fx.want)-1)
	}
}

func assertRestoredValues(t *testing.T, fx *restoreFixture) {
	t.Helper()
	ctx := context.Background()
	type fr struct {
		status, title, instruction, event, actorType, until, onTimeout string
		fireAt, createdAt, resolvedAt                                  *time.Time
		issue, firedTask, actorID, createdByTask                       *string
	}
	read := func(label string) fr {
		var r fr
		err := fx.pool.QueryRow(ctx, `SELECT status, title, instruction, event_type, filter_actor_type, until_status, on_timeout,
			fire_at, created_at, resolved_at, issue_id::text, fired_task_id::text, filter_actor_id::text, created_by_task_id::text
			FROM chat_wakeup WHERE id = $1`, fx.id(label)).Scan(&r.status, &r.title, &r.instruction, &r.event, &r.actorType, &r.until, &r.onTimeout,
			&r.fireAt, &r.createdAt, &r.resolvedAt, &r.issue, &r.firedTask, &r.actorID, &r.createdByTask)
		if err != nil {
			t.Errorf("%s: %v", label, err)
		}
		return r
	}
	// ended after the deploy: fired keeps its task; the person's cancel has none
	if r := read("watch_issue_gone"); r.status != "fired" || derefStr(r.firedTask) != rsTaskFired1 || r.resolvedAt == nil {
		t.Errorf("watch_issue_gone: %+v", r)
	}
	if r := read("extra_expired"); r.status != "cancelled" || r.firedTask != nil || r.resolvedAt == nil {
		t.Errorf("extra_expired: %+v", r)
	}
	// ended_not_expressible: S's text, cancelled now
	if r := read("watch_any_pending"); r.status != "cancelled" || r.instruction != "do any" || r.resolvedAt == nil {
		t.Errorf("watch_any_pending: %+v", r)
	}
	// from_current: new text, the title X made, S's id and source task
	if r := read("empty_title"); r.status != "pending" || r.instruction != "new instruction after deploy" || r.title != longInstruction[:120] || derefStr(r.createdByTask) != "" {
		t.Errorf("empty_title: %+v", r)
	}
	if r := read("extra_moved"); r.status != "pending" || r.fireAt == nil || time.Until(*r.fireAt) < time.Hour {
		t.Errorf("extra_moved: %+v", r)
	}
	// converted_new
	if r := read("n_issue"); r.status != "pending" || r.until != "done" || derefStr(r.issue) != issueOne || r.onTimeout != "wake" || r.event != "" || derefStr(r.createdByTask) != taskInit {
		t.Errorf("n_issue: %+v", r)
	}
	if r := read("n_reply_member_author"); r.status != "pending" || r.event != "comment.created" || r.actorType != "member" || derefStr(r.actorID) != userMem || r.onTimeout != "wake" ||
		r.createdAt == nil || r.createdAt.UTC().Format(time.RFC3339) != "2026-11-28T01:02:03Z" {
		t.Errorf("n_reply_member_author: %+v", r)
	}
	if r := read("n_reply_no_author_type"); r.event != "comment.created" || r.actorType != "member" || r.actorID != nil {
		t.Errorf("n_reply_no_author_type must be a member reply: %+v", r)
	}
	if r := read("n_at"); r.status != "pending" || r.onTimeout != "drop" || r.issue != nil || r.fireAt == nil {
		t.Errorf("n_at: %+v", r)
	}
	if r := read("n_at_fired"); r.status != "fired" || derefStr(r.firedTask) != rsTaskNew || r.resolvedAt == nil {
		t.Errorf("n_at_fired: %+v", r)
	}
	if r := read("n_at_cancelled"); r.status != "cancelled" || r.firedTask != nil || r.resolvedAt == nil {
		t.Errorf("n_at_cancelled: %+v", r)
	}
	// the weixin_chat issue survives
	if n := tableCountWhere(t, fx.pool, "issue", "origin_type = 'weixin_chat'"); n != 1 {
		t.Errorf("weixin_chat issues = %d, want 1", n)
	}
}

func tableCountWhere(t *testing.T, pool *pgxpool.Pool, table, where string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE `+where).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// The ledger rows, the schema, the kept tables and the rename record.
func assertLedgerAndShape(t *testing.T, fx *restoreFixture) {
	t.Helper()
	ctx := context.Background()
	for _, v := range []string{"99zzz00_chat_wakeup_fork_convert", "99zzz00_chat_wakeup_fork_convert_sqlc_hint", "99zzz15_chat_wakeup", "99zzz16_chat_wakeup_due_idx", "99zzz17_chat_wakeup_session_idx", "99zzz18_chat_wakeup_requester_idx"} {
		if n := tableCountWhere(t, fx.pool, "schema_migrations", fmt.Sprintf("version = '%s'", v)); n != 0 {
			t.Errorf("ledger row %s kept", v)
		}
	}
	// other rows stay: the ledger still holds the pre-X versions
	if n := tableCountWhere(t, fx.pool, "schema_migrations", "version < '99zzz00'"); n == 0 {
		t.Errorf("R deleted other ledger rows")
	}
	if n := tableCountWhere(t, fx.pool, "schema_migrations", "version > '99zzz00_chat_wakeup_fork_convert_sqlc_hint' AND version NOT LIKE '99zzz1_%'"); n == 0 {
		// versions after the chat_wakeup block (99zzz19 onward) are applied by the same migrate up
		t.Logf("no ledger rows after the chat_wakeup block")
	}
	for _, tbl := range []string{fx.saved, fx.snap, "chat_wakeup_restore_report", "chat_wakeup_fork_conversion"} {
		if tableCount(t, fx.pool, tbl) < 1 {
			t.Errorf("table %s missing or empty", tbl)
		}
	}
	// the fork shape: no upstream columns, the fork status check, 702's and 797's indexes
	var cols []string
	rows, err := fx.pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'chat_wakeup' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		cols = append(cols, c)
	}
	rows.Close()
	wantCols := []string{"id", "workspace_id", "chat_session_id", "agent_id", "instruction", "fire_at", "status", "created_by_task_id", "fired_task_id", "created_at", "resolved_at", "title", "issue_id", "event_type", "filter_actor_type", "filter_actor_id", "until_status", "on_timeout"}
	if !slices.Equal(cols, wantCols) {
		t.Errorf("chat_wakeup columns = %v, want %v", cols, wantCols)
	}
	idx, _ := queryStrings(ctx, fx.pool, `SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'chat_wakeup' ORDER BY 1`)
	if want := []string{"chat_wakeup_pkey", "idx_chat_wakeup_fired_task", "idx_chat_wakeup_pending"}; !slices.Equal(idx, want) {
		t.Errorf("chat_wakeup indexes = %v, want %v", idx, want)
	}
	if _, err := fx.pool.Exec(ctx, `INSERT INTO chat_wakeup (workspace_id, chat_session_id, agent_id, instruction, fire_at, status) VALUES ($1, $2, $3, 'x', now(), 'expired')`, wsID, sessOne, agentID); err == nil {
		t.Errorf("the fork status check accepts 'expired'")
	}
	// the saved table's names cannot collide with a later restore
	saved, _ := queryStrings(ctx, fx.pool, `SELECT conname FROM pg_constraint WHERE conrelid = $1::regclass UNION ALL SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indrelid = $1::regclass`, fx.saved)
	for _, n := range saved {
		if !strings.HasPrefix(n, "cwus_"+fx.attempt+"_") && !regexp.MustCompile(`^chat_wakeup_(kind|check|status)`).MatchString(n) {
			t.Errorf("saved-table object %q keeps a fixed name", n)
		}
	}
	// the rename record
	var detail string
	if err := fx.pool.QueryRow(ctx, `SELECT detail::text FROM chat_wakeup_restore_report WHERE attempt_id = $1 AND outcome = 'saved_table_renames'`, fx.attempt).Scan(&detail); err != nil || !strings.Contains(detail, "chat_wakeup_upstream_saved_"+fx.attempt) {
		t.Errorf("rename record: %q (%v)", detail, err)
	}
}

// A6: a roll-forward after R runs X and 675-678 again and converts R's table.
func assertRollForward(t *testing.T, fx *restoreFixture) {
	t.Helper()
	ctx := context.Background()
	pre := map[string]string{}
	rows, err := fx.pool.Query(ctx, `SELECT id::text, status FROM chat_wakeup`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, st string
		_ = rows.Scan(&id, &st)
		pre[id] = st
	}
	rows.Close()
	out, err := runMigrateUp(t, fx.name, attemptEnv(restoreAttempt2)...)
	if err != nil {
		t.Fatalf("A6: migrate up after R: %v\n%s", err, tail(out, 4000))
	}
	for _, v := range []string{"99zzz00_chat_wakeup_fork_convert", "99zzz15_chat_wakeup", "99zzz16_chat_wakeup_due_idx", "99zzz17_chat_wakeup_session_idx", "99zzz18_chat_wakeup_requester_idx"} {
		if n := tableCountWhere(t, fx.pool, "schema_migrations", fmt.Sprintf("version = '%s'", v)); n != 1 {
			t.Errorf("A6: ledger row %s count = %d after the roll-forward", v, n)
		}
	}
	if n := tableCountWhere(t, fx.pool, "chat_wakeup_fork_conversion", "attempt_id = '"+restoreAttempt2+"'"); n != 1 {
		t.Errorf("A6: new conversion rows = %d, want 1", n)
	}
	if exists, _ := queryStrings(ctx, fx.pool, `SELECT to_regclass('schema_contracts')::text`); len(exists) == 1 && exists[0] != "" {
		if n := tableCountWhere(t, fx.pool, "schema_contracts", "version = '99zzz00_chat_wakeup_fork_convert' AND attempt_id = '"+restoreAttempt2+"'"); n != 1 {
			t.Errorf("A6: new schema_contracts rows = %d, want 1", n)
		}
		if n := tableCountWhere(t, fx.pool, "schema_contracts", "version = '99zzz00_chat_wakeup_fork_convert'"); n != 1 {
			t.Errorf("A6: schema_contracts rows of X = %d, want 1 (R deleted the old one)", n)
		}
	} else {
		t.Logf("A6: no schema_contracts table on this tree; that row is checked on the N5r stack")
	}
	// the new chat_wakeup equals X's conversion of R's table: status mapping per row, orphans dropped
	wantStatus := map[string]string{"pending": "active", "fired": "fired", "cancelled": "cancelled"}
	got := map[string]string{}
	rows, err = fx.pool.Query(ctx, `SELECT id::text, status FROM chat_wakeup`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, st string
		_ = rows.Scan(&id, &st)
		got[id] = st
	}
	rows.Close()
	if len(got) != len(pre) {
		t.Errorf("A6: converted %d rows, want %d", len(got), len(pre))
	}
	for id, st := range pre {
		if got[id] != wantStatus[st] {
			t.Errorf("A6: %s status %q after conversion, want %q", fx.label[id], got[id], wantStatus[st])
		}
	}
	assertRoundTrip(t, fx)
	idx, _ := queryStrings(ctx, fx.pool, `SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND tablename = 'chat_wakeup' ORDER BY 1`)
	for _, want := range []string{"idx_chat_wakeup_due", "idx_chat_wakeup_session", "idx_chat_wakeup_requester"} {
		if !slices.Contains(idx, want) {
			t.Errorf("A6: index %s missing after the roll-forward: %v", want, idx)
		}
	}
	// a second restore cannot collide with the first one's saved names: the first saved table is still there
	if tableCount(t, fx.pool, fx.saved) < 1 {
		t.Errorf("A6: first saved table gone")
	}
}

// assertRoundTrip: X(R(U)) == U on every column the fork table can carry. U is
// the upstream row R read (fx.preR). After the roll-forward, each row R
// restored must equal U on those columns. The columns the fork table has no
// place for are not compared, because X derives them again, by its own rule:
//   - updated_at, next_eval_at: clock values.
//   - requester_id: X takes the owner of the chat session.
//   - end_reason: X writes fork_migrated for an ended row.
//   - condition_state: the fork keeps none; X starts it empty.
//   - created_at of a condition row: R stores the watch's `since` there, and X
//     reads it back (the fork has no other column for it).
//   - condition.identifier and condition.author_name: X looks the names up.
//
// Rows R cannot express are excluded; assertRestored checks their outcome.
func assertRoundTrip(t *testing.T, fx *restoreFixture) {
	t.Helper()
	ctx := context.Background()
	outcome := map[string]string{}
	rows, err := fx.pool.Query(ctx, `SELECT id::text, outcome FROM chat_wakeup_restore_report WHERE attempt_id = $1 AND outcome <> 'saved_table_renames'`, fx.attempt)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, o string
		_ = rows.Scan(&id, &o)
		outcome[id] = o
	}
	rows.Close()
	post := map[string]string{}
	rows, err = fx.pool.Query(ctx, `SELECT id::text, to_jsonb(c)::text FROM chat_wakeup c`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, js string
		_ = rows.Scan(&id, &js)
		post[id] = js
	}
	rows.Close()
	volatile := []string{"updated_at", "next_eval_at", "requester_id", "end_reason", "condition_state"}
	decode := func(js string) map[string]any {
		m := map[string]any{}
		if err := json.Unmarshal([]byte(js), &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range volatile {
			delete(m, k)
		}
		if cond, ok := m["condition"].(map[string]any); ok {
			delete(cond, "identifier")
			delete(cond, "author_name")
			delete(m, "created_at") // holds the watch's since, set by R
		}
		return m
	}
	compared := 0
	for id, o := range outcome {
		switch o {
		case "restored_pending", "from_current", "restored_terminal", "ended_after_deploy", "converted_new":
		default:
			continue
		}
		if fx.label[id] == "n_reply_no_author_type" {
			continue // Decision A: R adds author_type member; checked in assertRestoredValues
		}
		want, ok := fx.preR[id]
		if !ok {
			t.Errorf("A6: %s (%s) has no pre-R row", fx.label[id], o)
			continue
		}
		got, ok := post[id]
		if !ok {
			t.Errorf("A6: %s (%s) is gone after the roll-forward", fx.label[id], o)
			continue
		}
		w, g := decode(want), decode(got)
		for k, wv := range w {
			if gv := g[k]; !reflect.DeepEqual(wv, gv) {
				t.Errorf("A6: %s (%s) column %s = %v after the roll-forward, want %v (the row R read)", fx.label[id], o, k, gv, wv)
			}
		}
		for k := range g {
			if _, ok := w[k]; !ok {
				t.Errorf("A6: %s (%s) has an extra column %s", fx.label[id], o, k)
			}
		}
		compared++
	}
	if compared < 15 {
		t.Errorf("A6: compared %d rows, want at least 15", compared)
	}
}

// ---- refusals ------------------------------------------------------------------

// Every refusal leaves the database as it was: the upstream table, the ledger rows, no saved table.
func TestChatWakeupForkRestoreRefusals(t *testing.T) {
	if step, _ := chosenStep(t); step != "script" {
		t.Skip("green-only check")
	}
	fx := buildRestoreFixture(t, false)
	ctx := context.Background()
	state := func() string {
		var s string
		err := fx.pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM chat_wakeup)::text || '/' ||
			(SELECT count(*) FROM schema_migrations)::text || '/' ||
			coalesce(to_regclass('`+fx.saved+`')::text, '-') || '/' ||
			(SELECT count(*) FROM pg_attribute WHERE attrelid = 'chat_wakeup'::regclass AND attname = 'kind')::text`).Scan(&s)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := state()
	cases := []struct {
		name, attempt string
		setup         string
		wantErr       string
	}{
		{"no attempt id", "", ``, "no attempt id"},
		{"attempt id not hex", "NOT-HEX", ``, "32 lower-case hexadecimal"},
		{"attempt id of no snapshot", strings.Repeat("a", 32), ``, "snapshot table"},
		{"meta count differs", restoreAttempt, `UPDATE chat_wakeup_fork_snapshot_meta SET rows = rows + 1`, "meta row says"},
		{"snapshot has another column list", restoreAttempt, `UPDATE chat_wakeup_fork_snapshot_meta SET rows = rows - 1; ALTER TABLE ` + fx.snap + ` ADD COLUMN extra text`, "fork chat_wakeup columns"},
	}
	for _, c := range cases {
		if c.setup != "" {
			fx.exec(t, c.setup)
		}
		_, err := runRestoreScript(t, fx.name, c.attempt)
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: error = %v, want it to contain %q", c.name, err, c.wantErr)
		}
		if after := state(); after != before {
			t.Errorf("%s: state %s, want unchanged %s", c.name, after, before)
		}
	}
}

// X has not run: nothing to restore.
func TestChatWakeupForkRestoreRefusesBeforeX(t *testing.T) {
	if step, _ := chosenStep(t); step != "script" {
		t.Skip("green-only check")
	}
	pool, name := upgradeFixture(t, true)
	loadSnapshot(t, pool, restoreAttempt)
	_, err := runRestoreScript(t, name, restoreAttempt)
	if err == nil || !strings.Contains(err.Error(), "has no schema_migrations row") {
		t.Fatalf("error = %v, want no schema_migrations row", err)
	}
	if n := tableCount(t, pool, "chat_wakeup"); n != 18 {
		t.Fatalf("fork table changed: %d rows", n)
	}
}

// R is not a migration: the runner must never pick it up, and it holds no gate-path files.
func TestChatWakeupForkRestoreIsNotAMigration(t *testing.T) {
	body, err := os.ReadFile(restoreScriptPath)
	if err != nil {
		t.Fatalf("read R: %v", err)
	}
	if strings.Contains(string(body), "multica:contract") {
		t.Errorf("R carries a contract marker; it is hand-run")
	}
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	for _, f := range files {
		if strings.Contains(filepath.Base(f), "chat_wakeup_fork_restore") {
			t.Errorf("R is in migrations: %s", f)
		}
	}
	// X's contract line names R
	x, _ := os.ReadFile(filepath.Join("..", "..", "migrations", chatWakeupXStem+".up.sql"))
	first, _, _ := strings.Cut(string(x), "\n")
	if !strings.HasSuffix(first, "restore=server/cmd/migrate/"+restoreScriptPath) {
		t.Errorf("X's contract line does not name R: %q", first)
	}
}

// R binds to the attempt X recorded. When schema_contracts exists, an absent or
// null row refuses (a snapshot from another attempt could replace current
// data), a different attempt refuses, and the matching row lets R run with or
// without the setting.
func TestChatWakeupForkRestoreBindsToXAttempt(t *testing.T) {
	if step, _ := chosenStep(t); step != "script" {
		t.Skip("green-only check")
	}
	fx := buildRestoreFixture(t, false)
	ctx := context.Background()
	const xv = "99zzz00_chat_wakeup_fork_convert"
	fx.exec(t, `CREATE TABLE IF NOT EXISTS schema_contracts (version TEXT PRIMARY KEY, attempt_id TEXT, snapshot_tables TEXT NOT NULL DEFAULT '', restore TEXT NOT NULL DEFAULT '', applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`)
	untouched := func(label string) {
		t.Helper()
		if got, _ := queryStrings(ctx, fx.pool, `SELECT coalesce(to_regclass('`+fx.saved+`')::text, '-')`); len(got) != 1 || got[0] != "-" {
			t.Errorf("%s: saved table exists: %v", label, got)
		}
	}
	refusals := []struct {
		name, setup, attempt, wantErr string
	}{
		{"no row, setting given", `DELETE FROM schema_contracts WHERE version = '` + xv + `'`, restoreAttempt, "has no row with an attempt id"},
		{"no row, no setting", `DELETE FROM schema_contracts WHERE version = '` + xv + `'`, "", "has no row with an attempt id"},
		{"row with null attempt", `DELETE FROM schema_contracts WHERE version = '` + xv + `'; INSERT INTO schema_contracts (version, attempt_id) VALUES ('` + xv + `', NULL)`, restoreAttempt, "has no row with an attempt id"},
		{"row with another attempt", `DELETE FROM schema_contracts WHERE version = '` + xv + `'; INSERT INTO schema_contracts (version, attempt_id) VALUES ('` + xv + `', '` + restoreAttempt2 + `')`, restoreAttempt, "differs from the attempt id"},
	}
	for _, c := range refusals {
		fx.exec(t, c.setup)
		_, err := runRestoreScript(t, fx.name, c.attempt)
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: error = %v, want it to contain %q", c.name, err, c.wantErr)
		}
		untouched(c.name)
	}
	// matching row, no setting: the row names the attempt
	fx.exec(t, `DELETE FROM schema_contracts WHERE version = '`+xv+`'; INSERT INTO schema_contracts (version, attempt_id) VALUES ('`+xv+`', '`+restoreAttempt+`')`)
	if _, err := runRestoreScript(t, fx.name, ""); err != nil {
		t.Fatalf("matching row, no setting: %v", err)
	}
	if got, _ := queryStrings(ctx, fx.pool, `SELECT to_regclass('`+fx.saved+`')::text`); len(got) != 1 || got[0] == "" {
		t.Errorf("matching row: saved table missing after R")
	}
	if n := tableCountWhere(t, fx.pool, "schema_contracts", "version = '"+xv+"'"); n != 0 {
		t.Errorf("matching row: R left %d schema_contracts rows of X, want 0", n)
	}
}
