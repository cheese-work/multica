package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Scoped-event outbox capture (CHE-1082 L9): the source transaction writes one
// reference-only row when a definition selecting that event type exists in the
// issue's workspace, current project or issue scope, and nothing otherwise.

const scopedRuleA = "9c2d7a4e-5b61-4f08-8d3a-1e7f6b2c0a01"
const scopedRuleB = "9c2d7a4e-5b61-4f08-8d3a-1e7f6b2c0a02"

type scopedKit struct {
	f       principalFixture
	owner   string
	project string
	issue   string
	agent   string
}

func newScopedKit(t *testing.T) scopedKit {
	t.Helper()
	f, owner := newPrincipalFixture(t)
	if err := f.q.SeedIssueStatusEntries(context.Background(), parseTestUUID(t, f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
	// Registered before the rows it must outlive: cleanup runs in reverse order,
	// and deleting the fixture's own comments and issue captures events too.
	f.Cleanup(t, "DELETE FROM wakeup_scoped_event WHERE workspace_id=$1", f.WorkspaceID)
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
	f.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE workspace_id=$1)", f.WorkspaceID)
	f.Cleanup(t, "DELETE FROM issue_wakeup WHERE workspace_id=$1", f.WorkspaceID)
	project := f.Project(t, "scoped project")
	return scopedKit{
		f: f, owner: owner, project: project,
		issue: f.Issue(t, "scoped issue", testutil.Cols{"project_id": project}),
		agent: f.privateAgentOwnedBy(t, owner, "scoped"),
	}
}

// eventConfig is the stored patch of an event rule. Validation of event
// triggers belongs to a later layer, so fixtures write definitions directly.
func (k scopedKit) eventConfig(instruction string, events ...string) string {
	trigger, _ := json.Marshal(map[string]any{"kind": "event", "events": events})
	return fmt.Sprintf(`{"v":1,"enabled":true,"trigger":%s,"target":{"type":"agent","id":%q},"instruction":%q}`, trigger, k.agent, instruction)
}

func (k scopedKit) define(t *testing.T, scopeKind, scopeID, rule string, root bool, config string, events ...string) {
	t.Helper()
	k.f.Exec(t, `INSERT INTO issue_wakeup_definition(workspace_id,scope_kind,scope_id,rule_key,root,config,event_types,created_by,updated_by)
 VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$8)`, k.f.WorkspaceID, scopeKind, scopeID, rule, root, config, append([]string{}, events...), k.owner)
}

func (k scopedKit) defineRoot(t *testing.T, scopeKind, scopeID, rule string, events ...string) {
	t.Helper()
	// An issue scope only overrides: it can never be a rule's root.
	k.define(t, scopeKind, scopeID, rule, scopeKind != "issue", k.eventConfig("look at it", events...), events...)
}

func (k scopedKit) comment(t *testing.T, body string) string {
	t.Helper()
	return k.f.Comment(t, k.issue, body)
}

func (k scopedKit) pending(t *testing.T) int {
	t.Helper()
	return k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1 AND handled_at IS NULL`, k.f.WorkspaceID)
}

func (k scopedKit) total(t *testing.T) int {
	t.Helper()
	return k.f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1`, k.f.WorkspaceID)
}

func TestScopedCaptureWritesReferencesOnly(t *testing.T) {
	k := newScopedKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	const body = "top secret comment body"
	comment := k.comment(t, body)
	if got := k.total(t); got != 1 {
		t.Fatalf("outbox rows = %d, want 1: the first event arrives before any runtime instance exists", got)
	}
	var eventType, project, payload, actorType, actorID string
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT event_type,project_id::text,payload::text,actor_type,actor_id FROM wakeup_scoped_event WHERE workspace_id=$1`, k.f.WorkspaceID).
		Scan(&eventType, &project, &payload, &actorType, &actorID); err != nil {
		t.Fatal(err)
	}
	if eventType != "comment.created" || project != k.project || actorType != "member" || actorID != k.f.UserID {
		t.Fatalf("row = %s project %s actor %s/%s", eventType, project, actorType, actorID)
	}
	if strings.Contains(payload, body) {
		t.Fatalf("the outbox must hold references, never a comment body: %s", payload)
	}
	if !strings.Contains(payload, comment) {
		t.Fatalf("payload lacks the comment reference: %s", payload)
	}
	if got := k.f.Count(t, `SELECT count(*) FROM issue_wakeup WHERE workspace_id=$1`, k.f.WorkspaceID); got != 0 {
		t.Fatalf("capture must not create an instance, found %d", got)
	}
}

func TestScopedCaptureOnlyForRelevantDefinitions(t *testing.T) {
	k := newScopedKit(t)
	other := k.f.Project(t, "another project")
	otherIssue := k.f.Issue(t, "other issue")

	// No definition at all: no row.
	k.comment(t, "none")
	// A definition selecting another event type, another project's definition and
	// another issue's definition are none of this issue's business.
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "task.failed")
	k.defineRoot(t, "project", other, scopedRuleB, "comment.created")
	k.defineRoot(t, "issue", otherIssue, "9c2d7a4e-5b61-4f08-8d3a-1e7f6b2c0a03", "comment.created")
	k.comment(t, "still none")
	if got := k.total(t); got != 0 {
		t.Fatalf("outbox rows = %d, want 0 without a relevant definition", got)
	}

	k.defineRoot(t, "project", k.project, "9c2d7a4e-5b61-4f08-8d3a-1e7f6b2c0a04", "comment.created")
	k.comment(t, "project scope")
	k.defineRoot(t, "issue", k.issue, "9c2d7a4e-5b61-4f08-8d3a-1e7f6b2c0a05", "comment.created")
	k.comment(t, "issue scope")
	if got := k.total(t); got != 2 {
		t.Fatalf("outbox rows = %d, want one per relevant event", got)
	}
}

func TestScopedCaptureSkipsClosedIssue(t *testing.T) {
	k := newScopedKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	k.f.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, k.issue)
	k.comment(t, "after close")
	if got := k.total(t); got != 0 {
		t.Fatalf("a closed issue wakes nothing, outbox rows = %d", got)
	}
}

func TestScopedCaptureRollsBackWithSourceTransaction(t *testing.T) {
	k := newScopedKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	ctx := context.Background()
	tx, err := k.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'member',$3,'rolled back','comment')`,
		k.issue, k.f.WorkspaceID, k.f.UserID); err != nil {
		t.Fatal(err)
	}
	var inTx int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1`, k.f.WorkspaceID).Scan(&inTx); err != nil || inTx != 1 {
		t.Fatalf("the source transaction must see its own outbox row, got %d (%v)", inTx, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := k.total(t); got != 0 {
		t.Fatalf("a rolled-back source write left %d outbox rows", got)
	}
}

func TestScopedCaptureConcurrentWrites(t *testing.T) {
	k := newScopedKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	const writers, each = 8, 10
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				if _, err := k.f.Pool.Exec(context.Background(), `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'member',$3,'c','comment')`,
					k.issue, k.f.WorkspaceID, k.f.UserID); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := k.total(t); got != writers*each {
		t.Fatalf("outbox rows = %d, want %d: no concurrent source write may lose its capture", got, writers*each)
	}
	k.f.Exec(t, `DELETE FROM comment WHERE issue_id=$1`, k.issue)
}

// An issue change is captured as the same reference-only facts the legacy path
// keeps, with no description text.
func TestScopedCaptureOfIssueChangeKeepsNoDescription(t *testing.T) {
	k := newScopedKit(t)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "issue.updated", "issue.status_changed")
	k.f.Exec(t, `UPDATE issue SET status='in_progress',description='private description' WHERE id=$1`, k.issue)
	if got := k.total(t); got != 2 {
		t.Fatalf("outbox rows = %d, want issue.updated and issue.status_changed", got)
	}
	var payloads string
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT string_agg(payload::text,' ') FROM wakeup_scoped_event WHERE workspace_id=$1`, k.f.WorkspaceID).Scan(&payloads); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payloads, "private description") {
		t.Fatalf("payload carries issue text: %s", payloads)
	}
	if !strings.Contains(payloads, "changed_fields") || !strings.Contains(payloads, "in_progress") {
		t.Fatalf("payload lacks the changed-field references: %s", payloads)
	}
}

// Rules with no scoped definition behave exactly as before: a local rule still
// receives its receipt and nothing is written to the outbox.
func TestScopedCaptureLeavesLegacyRulesUnchanged(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	f.Cleanup(t, "DELETE FROM wakeup_scoped_event WHERE workspace_id=$1", f.WorkspaceID)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "check"})
	f.Comment(t, util.UUIDToString(issue), "legacy")
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1`, w.ID); got != 1 {
		t.Fatalf("legacy receipts = %d, want 1", got)
	}
	if got := f.Count(t, `SELECT count(*) FROM wakeup_scoped_event WHERE workspace_id=$1`, f.WorkspaceID); got != 0 {
		t.Fatalf("legacy capture wrote %d outbox rows", got)
	}
}

// The early-exit lookup is three scope-restricted index probes. At the 32 per
// scope ceiling it examines at most 96 candidate rows however many other
// definitions the workspace holds. The plan is read from the function body the
// trigger really runs.
func TestScopedCaptureEarlyExitPlanIsIndexedAndBounded(t *testing.T) {
	k := newScopedKit(t)
	ctx := context.Background()
	ws := k.f.WorkspaceID
	// Noise: 320 other projects at the ceiling, half selecting the event type.
	k.f.Exec(t, `INSERT INTO issue_wakeup_definition(workspace_id,scope_kind,scope_id,rule_key,root,config,event_types)
 SELECT $1,'project',p,gen_random_uuid()::text,true,'{"v":1}'::jsonb,CASE WHEN r%2=0 THEN ARRAY['comment.created'] ELSE ARRAY['task.failed'] END
 FROM (SELECT gen_random_uuid() p FROM generate_series(1,320)) ps, generate_series(1,32) r`, ws)
	// The ceiling in each of this issue's own scopes, none matching.
	for _, scope := range [][2]string{{"workspace", ws}, {"project", k.project}, {"issue", k.issue}} {
		k.f.Exec(t, `INSERT INTO issue_wakeup_definition(workspace_id,scope_kind,scope_id,rule_key,root,config,event_types)
 SELECT $1,$2,$3::uuid,gen_random_uuid()::text,$2<>'issue','{"v":1}'::jsonb,ARRAY['task.failed'] FROM generate_series(1,32)`, ws, scope[0], scope[1])
	}
	// The issue lookup must be a primary-key probe on a realistic table too.
	k.f.Cleanup(t, `DELETE FROM issue WHERE workspace_id=$1 AND title='filler'`, ws)
	k.f.Exec(t, `INSERT INTO issue(workspace_id,title,status,priority,creator_type,creator_id,position,number)
 SELECT $1,'filler','todo','none','member',$2,0,1000+n FROM generate_series(1,5000) n`, ws, k.f.UserID)
	k.f.Exec(t, `ANALYZE issue_wakeup_definition`)
	k.f.Exec(t, `ANALYZE issue`)

	conn, err := k.f.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var src string
	if err := conn.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname='wakeup_scoped_event_probe'`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	// The probe's own statement, as the trigger path runs it.
	m := regexp.MustCompile(`(?s)RETURN\s+(EXISTS.*?);\s*END`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("probe body not recognised:\n%s", src)
	}
	for _, stmt := range []string{`SET plan_cache_mode = force_custom_plan`, `PREPARE probe(uuid,text,uuid,text) AS SELECT ` + m[1]} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	for _, scope := range [][2]string{{"workspace", ws}, {"project", k.project}, {"issue", k.issue}} {
		for _, event := range []string{"comment.created", "task.started", ""} {
			arg := "NULL"
			if event != "" {
				arg = "'" + event + "'"
			}
			rows, err := conn.Query(ctx, fmt.Sprintf(`EXPLAIN (ANALYZE, BUFFERS) EXECUTE probe('%s','%s','%s',%s)`, ws, scope[0], scope[1], arg))
			if err != nil {
				t.Fatal(err)
			}
			var plan []string
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, line)
			}
			rows.Close()
			text := strings.Join(plan, "\n")
			t.Logf("EXPLAIN (ANALYZE, BUFFERS) probe(%s scope, %s):\n%s", scope[0], arg, text)
			if strings.Contains(text, "Seq Scan on issue_wakeup_definition") {
				t.Fatalf("the early exit scans all definitions:\n%s", text)
			}
			if !regexp.MustCompile(`Index Cond: \(\(workspace_id = .*\) AND \(scope_kind = .*\) AND \(scope_id = .*\)\)`).MatchString(text) {
				t.Fatalf("the probe must be restricted to (workspace_id, scope_kind, scope_id):\n%s", text)
			}
			if !strings.Contains(text, "issue_wakeup_definition_identity_idx") {
				t.Fatalf("the probe must use the identity index:\n%s", text)
			}
		}
	}
	// The issue lookup that feeds the probes is a primary-key probe.
	var issuePlan []string
	rows, err := conn.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS) SELECT workspace_id,project_id FROM issue WHERE id='`+k.issue+`'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		issuePlan = append(issuePlan, line)
	}
	rows.Close()
	t.Logf("EXPLAIN (ANALYZE, BUFFERS) issue lookup:\n%s", strings.Join(issuePlan, "\n"))
	if strings.Contains(strings.Join(issuePlan, "\n"), "Seq Scan") {
		t.Fatalf("the issue lookup must use its primary key:\n%s", strings.Join(issuePlan, "\n"))
	}
}

// Capture overhead on the source write, with no definition and at the ceiling
// of non-matching definitions in all three scopes. Rounds alternate between the
// two so a loaded host skews both alike, and the median of each is compared.
func TestScopedCaptureOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("timing measurement")
	}
	k := newScopedKit(t)
	ctx := context.Background()
	const writes, rounds = 300, 7
	insert := func() time.Duration {
		start := time.Now()
		for range writes {
			if _, err := k.f.Pool.Exec(ctx, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type) VALUES($1,$2,'member',$3,'c','comment')`,
				k.issue, k.f.WorkspaceID, k.f.UserID); err != nil {
				t.Fatal(err)
			}
		}
		d := time.Since(start) / writes
		k.f.Exec(t, `DELETE FROM comment WHERE issue_id=$1`, k.issue)
		return d
	}
	ceiling := func(on bool) {
		k.f.Exec(t, `DELETE FROM issue_wakeup_definition WHERE workspace_id=$1`, k.f.WorkspaceID)
		if !on {
			return
		}
		for _, scope := range [][2]string{{"workspace", k.f.WorkspaceID}, {"project", k.project}, {"issue", k.issue}} {
			k.f.Exec(t, `INSERT INTO issue_wakeup_definition(workspace_id,scope_kind,scope_id,rule_key,root,config,event_types)
 SELECT $1,$2,$3::uuid,gen_random_uuid()::text,$2<>'issue','{"v":1}'::jsonb,ARRAY['task.failed'] FROM generate_series(1,32)`, k.f.WorkspaceID, scope[0], scope[1])
		}
	}
	median := func(ds []time.Duration) time.Duration {
		slices.Sort(ds)
		return ds[len(ds)/2]
	}
	insert() // warm the plans
	var none, atCeiling []time.Duration
	for range rounds {
		ceiling(false)
		none = append(none, insert())
		ceiling(true)
		atCeiling = append(atCeiling, insert())
	}
	ceiling(false)
	k.defineRoot(t, "workspace", k.f.WorkspaceID, scopedRuleA, "comment.created")
	matching := insert()

	// Server-side cost of the early exit alone: plan cached, no round trips.
	conn, err := k.f.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var perCall [2]time.Duration
	const calls = 5000
	for i, on := range []bool{false, true} {
		ceiling(on)
		if _, err := conn.Exec(ctx, `DO $$ DECLARE s timestamptz; b boolean; BEGIN s:=clock_timestamp();
 FOR i IN 1..`+strconv.Itoa(calls)+` LOOP b:=wakeup_scoped_event_wanted('`+k.issue+`','comment.created'); END LOOP;
 PERFORM set_config('che1199.us',(extract(epoch FROM clock_timestamp()-s)*1000000)::text,false); END $$`); err != nil {
			t.Fatal(err)
		}
		var micros float64
		if err := conn.QueryRow(ctx, `SELECT current_setting('che1199.us')::float8`).Scan(&micros); err != nil {
			t.Fatal(err)
		}
		perCall[i] = time.Duration(micros / calls * float64(time.Microsecond))
	}
	// The same call against the pre-L9 capture function, copied from its
	// migration, is the baseline the added lookup is measured against.
	legacy := legacyCaptureProbe(t, k.f)
	var vsLegacy [2][2]time.Duration
	for i, on := range []bool{false, true} {
		ceiling(on)
		var pre, post []time.Duration
		for range rounds {
			for _, name := range []string{legacy, "capture_issue_wakeup"} {
				start := time.Now()
				for range writes {
					if _, err := k.f.Pool.Exec(ctx, `SELECT `+name+`($1,'comment.created',gen_random_uuid()::text,NULL,NULL,'{}'::jsonb)`, k.issue); err != nil {
						t.Fatal(err)
					}
				}
				d := time.Since(start) / writes
				if name == legacy {
					pre = append(pre, d)
				} else {
					post = append(post, d)
				}
			}
		}
		vsLegacy[i] = [2]time.Duration{median(pre), median(post)}
	}
	t.Logf("capture_issue_wakeup call, one transaction each, median over %d alternating rounds: pre-L9 vs L9 with no definition %v vs %v (+%v); at the 96-definition ceiling %v vs %v (+%v)",
		rounds, vsLegacy[0][0], vsLegacy[0][1], vsLegacy[0][1]-vsLegacy[0][0], vsLegacy[1][0], vsLegacy[1][1], vsLegacy[1][1]-vsLegacy[1][0])
	t.Logf("source write (comment insert) median per event over %d alternating rounds: no definition %v, 96 non-matching definitions (ceiling) %v; ceiling overhead %v; matching definition + outbox row %v",
		rounds, median(none), median(atCeiling), median(atCeiling)-median(none), matching)
	t.Logf("early-exit function alone, plan cached: no definition %v per call, ceiling %v per call", perCall[0], perCall[1])
	if over := median(atCeiling) - median(none); over > 2*time.Millisecond {
		t.Fatalf("ceiling overhead %v per source write exceeds the 2ms budget", over)
	}
	if over := vsLegacy[1][1] - vsLegacy[1][0]; over > time.Millisecond {
		t.Fatalf("capture at the ceiling costs %v more than before L9, over the 1ms budget", over)
	}
}

// legacyCaptureProbe installs the capture function exactly as migration 532
// defined it under another name and returns that name.
func legacyCaptureProbe(t *testing.T, f principalFixture) string {
	t.Helper()
	raw, err := os.ReadFile("../../migrations/532_wakeup_actor_capture.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "CREATE OR REPLACE FUNCTION capture_issue_wakeup(")
	end := strings.Index(src[start:], "END $$;")
	if start < 0 || end < 0 {
		t.Fatal("pre-L9 capture function not found in migration 532")
	}
	const name = "capture_issue_wakeup_pre_l9_probe"
	f.Exec(t, strings.Replace(src[start:start+end+len("END $$;")], "capture_issue_wakeup(", name+"(", 1))
	f.Cleanup(t, "DROP FUNCTION IF EXISTS "+name+"(uuid, text, text, uuid, uuid, jsonb)")
	return name
}
