package db

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// CHE-9177: ListNextTurnPREvents ran 15 to 27 s on C00 (wait event
// IO/DataFileRead), over the 15 s quiescence threshold, so cd-deploy.yml refused
// the deploy. Two things cost the time, measured on the step 2 copy (CHE-9238):
//
//   - the pr.event NOT EXISTS arm read every receipt of each candidate event
//     (about 168,000 receipts per read; 99.75% of them were processed with no
//     run and can never match), and
//   - the planner costed the EXISTS arms inside the status OR as per-row
//     SubPlans, put the plan over jit_above_cost, and compiled the query with
//     JIT (1.3 s of 1.8 s).
//
// CHE-6889 passed on a small fixture while C00 stayed slow, so this fixture has
// the C00 skew (CHE-9238 "Fixture ratios"): receipts concentrated on a few
// hundred (wakeup, event) pairs, 96% pr.event, 99.75% of those processed with
// no run. The assertions do not depend on host speed: rows examined, buffers,
// plan shape and the planner's JIT threshold.
const (
	nextTurnHeavyEvents    = 190  // candidate events, each with a large pile of dead receipts
	nextTurnHeavyReceipts  = 1600 // processed-with-no-run pr.event receipts per heavy event (C00: 857 to 3,354)
	nextTurnLightEvents    = 190  // candidate events with no receipts of this kind
	nextTurnMergedEvents   = 2000 // accepted events of kind merged: out of the candidate set by kind
	nextTurnSettledEvents  = 18000
	nextTurnFillerIssues   = 8800 // issue rows the notified arm has to hash
	nextTurnHeavyPerIssue  = 10
	nextTurnRunningTasks   = 23
	nextTurnPendingRcpts   = 112
	nextTurnCarriedRcpts   = 600
	nextTurnExtraHeavyWake = 12000 // one wakeup holds about 28,000 receipts in all (C00 max 28,498)
)

func seedNextTurnPlan(ctx context.Context, t *testing.T, tx pgx.Tx) {
	t.Helper()
	var ws string
	if err := tx.QueryRow(ctx, `INSERT INTO workspace (name, slug, issue_prefix) VALUES ('che9177', 'che9177-plan', 'C91') RETURNING id::text`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	nHeavyIssues := (nextTurnHeavyEvents + nextTurnHeavyPerIssue - 1) / nextTurnHeavyPerIssue
	// Issues 1..nHeavyIssues hold the heavy events; the next block holds the light
	// ones; the rest are filler. Every issue below the filler has a
	// pr_needs_attention wakeup.
	nWakeIssues := nHeavyIssues + nextTurnLightEvents
	seed := []string{
		`INSERT INTO agent (id, workspace_id, name, runtime_mode) VALUES (gen_random_uuid(), '$ws', 'plan', 'local')`,
		`INSERT INTO agent_runtime (id, workspace_id, name, runtime_mode, provider) VALUES (gen_random_uuid(), '$ws', 'plan', 'local', 'x')`,
		`CREATE TEMP TABLE np_issue ON COMMIT DROP AS SELECT g, gen_random_uuid() id FROM generate_series(1, $wake + $filler) g`,
		`INSERT INTO issue (id, workspace_id, title, creator_type, creator_id, assignee_type, number)
		 SELECT id, '$ws', 't', 'member', gen_random_uuid(), CASE WHEN g % 5 = 0 THEN 'member' ELSE 'agent' END, g FROM np_issue`,
		`INSERT INTO issue_wakeup (id, workspace_id, issue_id, instruction, kind, mode, system_rule)
		 SELECT gen_random_uuid(), '$ws', id, 'x', 'event', 'continuous', 'pr_needs_attention' FROM np_issue WHERE g <= $wake`,
		`INSERT INTO github_pull_request (id, workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha, head_repo_same)
		 SELECT gen_random_uuid(), '$ws', 1, 'acme', 'widget', g, 't', 'open', 'u', now(), now(), 'h', true FROM generate_series(1, $heavy + $light + 1) g`,
		`CREATE TEMP TABLE np_pr ON COMMIT DROP AS SELECT row_number() OVER (ORDER BY pr_number) g, id FROM github_pull_request WHERE workspace_id = '$ws'`,
		// Heavy candidates: head_changed, next_turn, on the first nHeavyIssues issues.
		`CREATE TEMP TABLE np_heavy ON COMMIT DROP AS SELECT g, gen_random_uuid() id FROM generate_series(1, $heavy) g`,
		`INSERT INTO issue_pr_event (id, workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key, status)
		 SELECT h.id, '$ws', i.id, p.id, 'head_changed', 'h' || h.g, 'b', now() - make_interval(secs => 100000 - h.g), 'd' || h.g, 'next_turn'
		 FROM np_heavy h JOIN np_issue i ON i.g = 1 + (h.g - 1) / $perissue JOIN np_pr p ON p.g = h.g`,
		// Light candidates: a mix of the three statuses and four kinds, no receipts of this kind.
		`INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key, status)
		 SELECT '$ws', i.id, p.id, (ARRAY['head_changed', 'checks_failed', 'conflict', 'closed'])[1 + l % 4], 'h' || l, 'b',
		        now() - make_interval(secs => 50000 - l), 'dl' || l, (ARRAY['next_turn', 'notified', 'accepted'])[1 + l % 3]
		 FROM generate_series(1, $light) l JOIN np_issue i ON i.g = $heavyissues + l JOIN np_pr p ON p.g = $heavy + l`,
		// Accepted events of kind merged (never candidates) and settled events, on filler issues.
		`INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key, status)
		 SELECT '$ws', i.id, (SELECT id FROM np_pr WHERE g = 1), 'merged', 'h', 'b', now(), 'dm' || m, 'accepted'
		 FROM generate_series(1, $merged) m JOIN np_issue i ON i.g = $wake + 1 + m % $filler`,
		`INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key, status)
		 SELECT '$ws', i.id, (SELECT id FROM np_pr WHERE g = 1), (ARRAY['head_changed', 'checks_failed', 'conflict', 'closed', 'draft', 'ready'])[1 + m % 6],
		        'h', 'b', now(), 'ds' || m, (ARRAY['delivered', 'superseded'])[1 + m % 2]
		 FROM generate_series(1, $settled) m JOIN np_issue i ON i.g = $wake + 1 + m % $filler`,
		// Dead receipts: processed, no run. About 99% of the pr.event receipts.
		`INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload, task_id, processed_at)
		 SELECT gen_random_uuid(), w.id, 1, 'k' || h.g || '-' || k, 'pr.event',
		        jsonb_build_object('event_id', h.id::text, 'head_sha', 'z' || k, 'repo_owner', 'acme', 'repo_name', 'widget', 'pr_number', '1',
		                           'detail', repeat('x', 200)),
		        NULL, now()
		 FROM np_heavy h
		 JOIN np_issue i ON i.g = 1 + (h.g - 1) / $perissue
		 JOIN issue_wakeup w ON w.issue_id = i.id AND w.system_rule = 'pr_needs_attention'
		 CROSS JOIN LATERAL generate_series(1, $receipts) k`,
		// One wakeup holds about 28,000 receipts in all: more for other event ids.
		`INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload, task_id, processed_at)
		 SELECT gen_random_uuid(), w.id, 1, 'x' || k, 'pr.event',
		        jsonb_build_object('event_id', gen_random_uuid()::text, 'head_sha', 'z', 'repo_owner', 'acme', 'repo_name', 'widget', 'pr_number', '1', 'detail', repeat('x', 200)),
		        NULL, now()
		 FROM np_issue i JOIN issue_wakeup w ON w.issue_id = i.id AND w.system_rule = 'pr_needs_attention', generate_series(1, $extra) k
		 WHERE i.g = 1`,
		// Other receipt kinds, as on C00: 4% of receipts.
		`INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload, task_id, processed_at)
		 SELECT gen_random_uuid(), w.id, 1, 'o' || k, (ARRAY['comment.created', 'issue.status_changed', 'time.due', 'condition.met'])[1 + k % 4],
		        jsonb_build_object('head_sha', 'z' || k), NULL, now()
		 FROM issue_wakeup w, generate_series(1, 40) k WHERE w.workspace_id = '$ws'`,
		// Pending receipts: processed_at NULL, on light events' wakeups.
		`INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload, task_id, processed_at)
		 SELECT gen_random_uuid(), w.id, 1, 'p' || l, 'pr.event',
		        jsonb_build_object('event_id', e.id::text, 'head_sha', 'h', 'repo_owner', 'acme', 'repo_name', 'widget', 'pr_number', '1'), NULL, NULL
		 FROM generate_series(1, $pending) l
		 JOIN issue_pr_event e ON e.dedupe_key = 'dl' || l AND e.workspace_id = '$ws'
		 JOIN issue_wakeup w ON w.issue_id = e.issue_id AND w.system_rule = 'pr_needs_attention'`,
		// Receipts that name a run: some in flight (hold the event), most ended.
		`CREATE TEMP TABLE np_task ON COMMIT DROP AS SELECT g, gen_random_uuid() id FROM generate_series(1, 4) g`,
		`INSERT INTO agent_task_queue (id, agent_id, runtime_id, status, started_at, completed_at)
		 SELECT id, (SELECT id FROM agent WHERE workspace_id = '$ws'), (SELECT id FROM agent_runtime WHERE workspace_id = '$ws'),
		        (ARRAY['queued', 'dispatched', 'completed', 'cancelled'])[g], CASE WHEN g = 3 THEN now() END, CASE WHEN g >= 3 THEN now() END
		 FROM np_task`,
		`INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload, task_id, processed_at)
		 SELECT gen_random_uuid(), w.id, 1, 'c' || l, 'pr.event',
		        jsonb_build_object('event_id', e.id::text, 'head_sha', 'h', 'repo_owner', 'acme', 'repo_name', 'widget', 'pr_number', '1'),
		        (SELECT id FROM np_task WHERE g = 1 + l % 4), now()
		 FROM generate_series(1, $pending + $carried) l
		 JOIN issue_pr_event e ON e.dedupe_key = 'dl' || (l - $pending) AND e.workspace_id = '$ws'
		 JOIN issue_wakeup w ON w.issue_id = e.issue_id AND w.system_rule = 'pr_needs_attention'
		 WHERE l > $pending`,
		// Running tasks that offer the PR-event channel (accepted events wait for them).
		`INSERT INTO agent_task_queue (id, agent_id, runtime_id, issue_id, status, started_at)
		 SELECT gen_random_uuid(), (SELECT id FROM agent WHERE workspace_id = '$ws'), (SELECT id FROM agent_runtime WHERE workspace_id = '$ws'),
		        i.id, 'running', now()
		 FROM np_issue i WHERE i.g BETWEEN $heavyissues + 1 AND $heavyissues + $running`,
		`INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability)
		 SELECT t.id, '$ws', t.issue_id, 'task-supplement-pr-event-v2' FROM agent_task_queue t WHERE t.status = 'running' AND t.issue_id IS NOT NULL`,
	}
	fill := strings.NewReplacer(
		"$ws", ws,
		"$wake", strconv.Itoa(nWakeIssues),
		"$filler", strconv.Itoa(nextTurnFillerIssues),
		"$heavyissues", strconv.Itoa(nHeavyIssues),
		"$heavy", strconv.Itoa(nextTurnHeavyEvents),
		"$light", strconv.Itoa(nextTurnLightEvents),
		"$merged", strconv.Itoa(nextTurnMergedEvents),
		"$settled", strconv.Itoa(nextTurnSettledEvents),
		"$perissue", strconv.Itoa(nextTurnHeavyPerIssue),
		"$receipts", strconv.Itoa(nextTurnHeavyReceipts),
		"$extra", strconv.Itoa(nextTurnExtraHeavyWake),
		"$pending", strconv.Itoa(nextTurnPendingRcpts),
		"$carried", strconv.Itoa(nextTurnCarriedRcpts),
		"$running", strconv.Itoa(nextTurnRunningTasks),
	)
	for _, statement := range seed {
		statement = fill.Replace(statement)
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("seed: %v\n%s", err, statement)
		}
	}
	for _, table := range []string{"issue_pr_event", "issue_wakeup", "issue_wakeup_receipt", "github_pull_request", "agent_task_queue", "task_pr_event_capability", "issue"} {
		if _, err := tx.Exec(ctx, "ANALYZE "+table); err != nil {
			t.Fatal(err)
		}
	}
}

type nextTurnPlan struct {
	work      explainWork
	totalCost float64
	text      string
	raw       []byte
}

// nextTurnPlanOf explains the statement as the wakeup tick runs it: workspace_ids
// NULL, row limit 100, the given cursor. It reads two plans: the estimated one
// (cost, JIT threshold) and the executed one (rows, buffers).
func nextTurnPlanOf(ctx context.Context, t *testing.T, tx pgx.Tx, sql string, afterObservedAt pgtype.Timestamptz, afterID pgtype.UUID) nextTurnPlan {
	t.Helper()
	args := []any{[]pgtype.UUID(nil), afterObservedAt, afterID, int32(100)}
	var estimate []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&estimate); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan struct {
			TotalCost float64 `json:"Total Cost"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal(estimate, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("read estimate: %v: %s", err, estimate)
	}
	work, raw, err := explainOracle(ctx, tx, sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	rows, err := tx.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		text.WriteString(line + "\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return nextTurnPlan{work: work, totalCost: plans[0].Plan.TotalCost, text: text.String(), raw: raw}
}

// Bounds, in units that do not depend on the host. Measured on this fixture:
// the index-backed form examines about 105,000 rows and reads about 2,500 buffers;
// the old form examined about 639,000 rows and read about 310,000 buffers (every
// dead receipt of every candidate event). The bounds sit between the two, so a
// return to a full receipt read breaks them by a wide margin and ordinary planner
// drift does not.
const (
	nextTurnRowsBound    = 250000
	nextTurnBuffersBound = 15000
)

func checkNextTurnPlan(t *testing.T, name string, p nextTurnPlan, jitAboveCost float64) []string {
	t.Helper()
	var problems []string
	if p.work.rowsExamined > nextTurnRowsBound {
		problems = append(problems, name+": rows examined "+strconv.FormatInt(p.work.rowsExamined, 10)+", want at most "+strconv.Itoa(nextTurnRowsBound))
	}
	if p.work.buffers > nextTurnBuffersBound {
		problems = append(problems, name+": shared buffers "+strconv.FormatInt(p.work.buffers, 10)+", want at most "+strconv.Itoa(nextTurnBuffersBound))
	}
	// The planner's total cost decides JIT: above jit_above_cost every run pays
	// compile time (1.3 s on the copy, 1.7 to 4 s on C00).
	if p.totalCost >= jitAboveCost {
		problems = append(problems, name+": planner total cost "+strconv.FormatFloat(p.totalCost, 'f', 0, 64)+" is not below jit_above_cost "+strconv.FormatFloat(jitAboveCost, 'f', 0, 64)+", so the query is JIT compiled")
	}
	if strings.Contains(string(p.raw), `"JIT"`) {
		problems = append(problems, name+": the executed plan carries a JIT block")
	}
	for _, line := range strings.Split(p.text, "\n") {
		if strings.Contains(line, "Seq Scan on issue_wakeup_receipt") {
			problems = append(problems, name+": reads issue_wakeup_receipt in full: "+strings.TrimSpace(line))
		}
		// idx_issue_wakeup_receipt_pr_event_id (no _task suffix) holds every pr.event
		// receipt, 99% of them processed with no run: the node that cost 12 to 21 s on C00.
		if strings.Contains(line, "using idx_issue_wakeup_receipt_pr_event_id on") {
			problems = append(problems, name+": reads the full receipt event_id index: "+strings.TrimSpace(line))
		}
	}
	return problems
}

func jitAboveCostOf(ctx context.Context, t *testing.T, tx pgx.Tx) float64 {
	t.Helper()
	var v float64
	if err := tx.QueryRow(ctx, `SELECT current_setting('jit_above_cost')::float8`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v <= 0 {
		t.Skip("jit_above_cost is off in this database; the JIT guard needs it set")
	}
	return v
}

// midCursor returns a cursor in the middle of the candidate walk, as the
// scheduler holds one after a full batch.
func midCursor(ctx context.Context, t *testing.T, tx pgx.Tx) (pgtype.Timestamptz, pgtype.UUID) {
	t.Helper()
	var at time.Time
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT observed_at, id FROM issue_pr_event WHERE dedupe_key = 'dl95'`).Scan(&at, &id); err != nil {
		t.Fatal(err)
	}
	return pgtype.Timestamptz{Time: at, Valid: true}, id
}

func TestListNextTurnPREventsPlanIsIndexBacked(t *testing.T) {
	ctx, tx := oracleTx(t)
	seedNextTurnPlan(ctx, t, tx)
	jit := jitAboveCostOf(ctx, t, tx)
	midAt, midID := midCursor(ctx, t, tx)

	var problems []string
	for _, c := range []struct {
		name string
		at   pgtype.Timestamptz
		id   pgtype.UUID
	}{{"cursor NULL", pgtype.Timestamptz{}, pgtype.UUID{}}, {"mid cursor", midAt, midID}} {
		p := nextTurnPlanOf(ctx, t, tx, listNextTurnPREvents, c.at, c.id)
		t.Logf("%s: rows examined %d, buffers %d, total cost %.0f\n%s", c.name, p.work.rowsExamined, p.work.buffers, p.totalCost, p.text)
		problems = append(problems, checkNextTurnPlan(t, c.name, p, jit)...)
	}
	for _, problem := range problems {
		t.Error(problem)
	}
}

// Negative control: the statement as it stood before the rewrite, on the same
// fixture, breaks the bounds. If it did not, the test above would prove nothing.
func TestListNextTurnPREventsOldFormBreaksTheBounds(t *testing.T) {
	ctx, tx := oracleTx(t)
	seedNextTurnPlan(ctx, t, tx)
	jit := jitAboveCostOf(ctx, t, tx)

	p := nextTurnPlanOf(ctx, t, tx, oldListNextTurnPREvents, pgtype.Timestamptz{}, pgtype.UUID{})
	t.Logf("old form: rows examined %d, buffers %d, total cost %.0f\n%s", p.work.rowsExamined, p.work.buffers, p.totalCost, p.text)
	problems := checkNextTurnPlan(t, "old form", p, jit)
	if len(problems) < 3 {
		t.Errorf("the old form broke only %d bounds (%v); the fixture no longer reproduces the C00 shape", len(problems), problems)
	}
}
