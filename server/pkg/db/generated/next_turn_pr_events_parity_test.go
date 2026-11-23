package db

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// CHE-9177 step 3: the rewritten ListNextTurnPREvents must select exactly what
// the old statement (kept as oldListNextTurnPREvents) selected. The fixture is
// the full product of the inputs the statement reads, one event per issue and
// per pull request so no combination shades another:
//
//	status (6) x kind (7) x PR state (3) x assignee (agent, member) x live
//	capable run on the issue (no, yes) x pr.event receipt scenario (10)
//	x legacy receipt scenario (3, for the two kinds the legacy rules cover)
//
// A third, hand-written oracle (expectNextTurnEligible) says which events must
// come back, so parity cannot hold by both statements being wrong the same way.

var (
	parityStatuses = []string{"accepted", "delivering", "delivered", "next_turn", "superseded", "notified"}
	parityKinds    = []string{"conflict", "head_changed", "checks_failed", "merged", "closed", "draft", "ready"}
	parityPRStates = []string{"open", "closed", "merged"}
)

// pr.event receipt scenarios (column sc). Eligible ones leave the event free.
const (
	scNone            = iota // no receipt
	scPending                // processed_at NULL
	scProcessedNoTask        // processed, no run: can never block
	scTaskQueued             // carried by a queued run
	scTaskDispatched         // carried by a dispatched run
	scTaskStarted            // carried by a running run that has started
	scTaskEndedUnst          // carried by a completed run that never started: discarded, event eligible again
	scTaskEndedStart         // carried by a completed run that had started
	scOtherEvent             // receipt for another event id
	scOtherRule              // pending receipt on a wakeup of another system_rule
)

// legacy receipt scenarios (column lg) for kinds checks_failed and conflict.
const (
	lgNone    = 0
	lgPending = 1
	lgStarted = 3
)

type parityEvent struct {
	id       [16]byte
	status   string
	kind     string
	prState  string
	assignee string
	running  bool
	sc, lg   int
}

func expectNextTurnEligible(e parityEvent) bool {
	switch e.kind {
	case "draft", "ready", "merged":
		return false
	}
	if (e.kind == "conflict" || e.kind == "head_changed" || e.kind == "checks_failed") && (e.prState == "merged" || e.prState == "closed") {
		return false
	}
	switch e.status {
	case "next_turn":
	case "notified":
		if e.assignee != "agent" {
			return false
		}
	case "accepted":
		if e.running {
			return false
		}
	default:
		return false
	}
	switch e.sc {
	case scPending, scTaskQueued, scTaskDispatched, scTaskStarted, scTaskEndedStart:
		return false
	}
	if (e.kind == "checks_failed" || e.kind == "conflict") && (e.lg == lgPending || e.lg == lgStarted) {
		return false
	}
	return true
}

func sqlTextArray(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + v + "'"
	}
	return "ARRAY[" + strings.Join(quoted, ",") + "]::text[]"
}

func seedNextTurnParity(ctx context.Context, t *testing.T, tx pgx.Tx) []parityEvent {
	t.Helper()
	var ws string
	if err := tx.QueryRow(ctx, `INSERT INTO workspace (name, slug, issue_prefix) VALUES ('che9177p', 'che9177-parity', 'C92') RETURNING id::text`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	fill := strings.NewReplacer(
		"$ws", ws,
		"$statuses", sqlTextArray(parityStatuses),
		"$kinds", sqlTextArray(parityKinds),
		"$prstates", sqlTextArray(parityPRStates),
	)
	seed := []string{
		`INSERT INTO agent (id, workspace_id, name, runtime_mode) VALUES (gen_random_uuid(), '$ws', 'parity', 'local')`,
		`INSERT INTO agent_runtime (id, workspace_id, name, runtime_mode, provider) VALUES (gen_random_uuid(), '$ws', 'parity', 'local', 'x')`,
		`CREATE TEMP TABLE np_ev ON COMMIT DROP AS
		 SELECT row_number() OVER () n, gen_random_uuid() id, gen_random_uuid() issue_id, gen_random_uuid() pr_id,
		        st, kd, ps, asg, run, sc, lg
		 FROM unnest($statuses) st, unnest($kinds) kd, unnest($prstates) ps, unnest(ARRAY['agent','member']) asg,
		      unnest(ARRAY[false,true]) run, generate_series(0, 9) sc, unnest(ARRAY[0,1,3]) lg
		 WHERE kd IN ('checks_failed', 'conflict') OR lg = 0`,
		`INSERT INTO issue (id, workspace_id, title, creator_type, creator_id, assignee_type, number)
		 SELECT issue_id, '$ws', 't', 'member', gen_random_uuid(), asg, n FROM np_ev`,
		`INSERT INTO issue_wakeup (id, workspace_id, issue_id, instruction, kind, mode, system_rule)
		 SELECT gen_random_uuid(), '$ws', issue_id, 'x', 'event', 'continuous', r
		 FROM np_ev, unnest(ARRAY['pr_needs_attention','pr_checks_failed']) r`,
		`INSERT INTO github_pull_request (id, workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha, head_repo_same)
		 SELECT pr_id, '$ws', 1, 'acme', 'widget', n, 't', ps, 'u', now(), now(), 'h' || n, true FROM np_ev`,
		`INSERT INTO issue_pr_event (id, workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key, status)
		 SELECT id, '$ws', issue_id, pr_id, kd, 'h' || n, 'b', now() - make_interval(secs => 100000 - n), 'd' || n, st FROM np_ev`,
		// Shared carriers: one task per state the statement tells apart.
		`CREATE TEMP TABLE np_task ON COMMIT DROP AS SELECT g, gen_random_uuid() id FROM generate_series(1, 5) g`,
		`INSERT INTO agent_task_queue (id, agent_id, runtime_id, status, started_at, completed_at)
		 SELECT id, (SELECT id FROM agent WHERE workspace_id = '$ws'), (SELECT id FROM agent_runtime WHERE workspace_id = '$ws'),
		        (ARRAY['queued','dispatched','running','completed','completed'])[g],
		        CASE WHEN g IN (3, 5) THEN now() END, CASE WHEN g >= 4 THEN now() END
		 FROM np_task`,
		// pr.event receipts on the pr_needs_attention wakeup.
		`INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload, task_id, processed_at)
		 SELECT gen_random_uuid(), w.id, 1, 'k' || e.n, 'pr.event',
		        jsonb_build_object('event_id', CASE WHEN e.sc = 8 THEN gen_random_uuid()::text ELSE e.id::text END, 'head_sha', 'z'),
		        CASE e.sc WHEN 3 THEN (SELECT id FROM np_task WHERE g = 1) WHEN 4 THEN (SELECT id FROM np_task WHERE g = 2)
		                  WHEN 5 THEN (SELECT id FROM np_task WHERE g = 3) WHEN 6 THEN (SELECT id FROM np_task WHERE g = 4)
		                  WHEN 7 THEN (SELECT id FROM np_task WHERE g = 5) END,
		        CASE WHEN e.sc = 1 THEN NULL ELSE now() END
		 FROM np_ev e JOIN issue_wakeup w ON w.issue_id = e.issue_id AND w.system_rule = CASE WHEN e.sc = 9 THEN 'pr_checks_failed' ELSE 'pr_needs_attention' END
		 WHERE e.sc <> 0`,
		// Legacy receipts, as the checks_failed and conflict rules write them.
		`INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload, task_id, processed_at)
		 SELECT gen_random_uuid(), w.id, 1, 'l' || e.n, CASE e.kd WHEN 'checks_failed' THEN 'pr.checks_failed' ELSE 'pr.mergeability' END,
		        jsonb_build_object('head_sha', 'h' || e.n, 'repo_owner', 'acme', 'repo_name', 'widget', 'pr_number', e.n::text,
		                           'merge_state', 'DIRTY', 'base_sha', 'b'),
		        CASE WHEN e.lg = 3 THEN (SELECT id FROM np_task WHERE g = 3) END,
		        CASE WHEN e.lg = 1 THEN NULL ELSE now() END
		 FROM np_ev e JOIN issue_wakeup w ON w.issue_id = e.issue_id
		   AND w.system_rule = CASE e.kd WHEN 'checks_failed' THEN 'pr_checks_failed' ELSE 'pr_needs_attention' END
		 WHERE e.lg <> 0 AND e.kd IN ('checks_failed', 'conflict')`,
		// A live run that offers the PR-event channel on the event's own issue.
		`INSERT INTO agent_task_queue (id, agent_id, runtime_id, issue_id, status, started_at)
		 SELECT gen_random_uuid(), (SELECT id FROM agent WHERE workspace_id = '$ws'), (SELECT id FROM agent_runtime WHERE workspace_id = '$ws'),
		        e.issue_id, 'running', now() FROM np_ev e WHERE e.run`,
		// Chat runs: running and capable, with no issue. They must not hide every accepted event.
		`INSERT INTO agent_task_queue (id, agent_id, runtime_id, issue_id, status, started_at)
		 SELECT gen_random_uuid(), (SELECT id FROM agent WHERE workspace_id = '$ws'), (SELECT id FROM agent_runtime WHERE workspace_id = '$ws'),
		        NULL, 'running', now() FROM generate_series(1, 3)`,
		`INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability)
		 SELECT t.id, '$ws', COALESCE(t.issue_id, gen_random_uuid()), 'task-supplement-pr-event-v2'
		 FROM agent_task_queue t WHERE t.status = 'running' AND t.agent_id = (SELECT id FROM agent WHERE workspace_id = '$ws')
		   AND t.id NOT IN (SELECT id FROM np_task)`,
	}
	for _, statement := range seed {
		statement = fill.Replace(statement)
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("seed: %v\n%s", err, statement)
		}
	}
	// Plans on the planner's statistics, as in production, not on empty ones.
	for _, table := range []string{"issue_pr_event", "issue_wakeup", "issue_wakeup_receipt", "github_pull_request", "agent_task_queue", "task_pr_event_capability", "issue"} {
		if _, err := tx.Exec(ctx, "ANALYZE "+table); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := tx.Query(ctx, `SELECT id, st, kd, ps, asg, run, sc, lg FROM np_ev`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []parityEvent
	for rows.Next() {
		var e parityEvent
		var id pgtype.UUID
		var sc, lg int32
		if err := rows.Scan(&id, &e.status, &e.kind, &e.prState, &e.assignee, &e.running, &sc, &lg); err != nil {
			t.Fatal(err)
		}
		e.id, e.sc, e.lg = id.Bytes, int(sc), int(lg)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func listNextTurnWith(ctx context.Context, t *testing.T, tx pgx.Tx, sql string, arg ListNextTurnPREventsParams) []ListNextTurnPREventsRow {
	t.Helper()
	rows, err := tx.Query(ctx, sql, arg.WorkspaceIds, arg.AfterObservedAt, arg.AfterID, arg.RowLimit)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []ListNextTurnPREventsRow{}
	for rows.Next() {
		var i ListNextTurnPREventsRow
		if err := rows.Scan(&i.ID, &i.WorkspaceID, &i.IssueID, &i.Kind, &i.HeadSha, &i.BaseSha, &i.ObservedAt,
			&i.PrNumber, &i.RepoOwner, &i.RepoName, &i.OldHeadSha, &i.CheckName); err != nil {
			t.Fatal(err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestListNextTurnPREventsParityWithOldStatement(t *testing.T) {
	ctx, tx := oracleTx(t)
	events := seedNextTurnParity(ctx, t, tx)
	q := New(tx)
	// Result parity does not depend on JIT; the old statement's compile time
	// (seconds per run, worse under load) would only slow the comparison.
	if _, err := tx.Exec(ctx, `SET LOCAL jit = off`); err != nil {
		t.Fatal(err)
	}

	want := map[[16]byte]bool{}
	for _, e := range events {
		if expectNextTurnEligible(e) {
			want[e.id] = true
		}
	}
	if len(want) < 100 || len(want) > len(events)/2 {
		t.Fatalf("the fixture does not discriminate: %d of %d events eligible", len(want), len(events))
	}

	all := ListNextTurnPREventsParams{RowLimit: 1_000_000}
	oldAll := listNextTurnWith(ctx, t, tx, oldListNextTurnPREvents, all)
	newAll, err := q.ListNextTurnPREvents(ctx, all)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(oldAll, newAll) {
		t.Fatalf("full read: old statement returned %d rows, new returned %d; the sets or rows differ", len(oldAll), len(newAll))
	}
	got := map[[16]byte]bool{}
	for _, r := range newAll {
		got[r.ID.Bytes] = true
	}
	if len(got) != len(want) {
		t.Errorf("oracle expects %d eligible events, statements return %d", len(want), len(got))
	}
	byID := map[[16]byte]parityEvent{}
	for _, e := range events {
		byID[e.id] = e
	}
	shown := 0
	for id := range got {
		if !want[id] && shown < 5 {
			t.Errorf("returned but not eligible: %+v", byID[id])
			shown++
		}
	}
	for id := range want {
		if !got[id] && shown < 10 {
			t.Errorf("eligible but not returned: %+v", byID[id])
			shown++
		}
	}

	// Both cursor forms and a small limit, against the old statement.
	mid := newAll[len(newAll)/2]
	for _, c := range []struct {
		name string
		arg  ListNextTurnPREventsParams
	}{
		{"cursor NULL limit 100", ListNextTurnPREventsParams{RowLimit: 100}},
		{"cursor NULL limit 7", ListNextTurnPREventsParams{RowLimit: 7}},
		{"mid cursor limit 100", ListNextTurnPREventsParams{AfterObservedAt: mid.ObservedAt, AfterID: mid.ID, RowLimit: 100}},
		{"mid cursor limit 1000000", ListNextTurnPREventsParams{AfterObservedAt: mid.ObservedAt, AfterID: mid.ID, RowLimit: 1_000_000}},
		{"cursor at the last row", ListNextTurnPREventsParams{AfterObservedAt: newAll[len(newAll)-1].ObservedAt, AfterID: newAll[len(newAll)-1].ID, RowLimit: 100}},
		{"workspace filter", ListNextTurnPREventsParams{WorkspaceIds: []pgtype.UUID{mid.WorkspaceID}, RowLimit: 100}},
		{"other workspace", ListNextTurnPREventsParams{WorkspaceIds: []pgtype.UUID{{Bytes: [16]byte{1}, Valid: true}}, RowLimit: 100}},
	} {
		oldRows := listNextTurnWith(ctx, t, tx, oldListNextTurnPREvents, c.arg)
		newRows, err := q.ListNextTurnPREvents(ctx, c.arg)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(oldRows, newRows) {
			t.Errorf("%s: old statement returned %d rows, new returned %d; they differ", c.name, len(oldRows), len(newRows))
		}
	}
}
