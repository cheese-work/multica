package service

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Activation sweep, activation signal and capacity retry of custom condition
// rules (CHE-1082 L10).

func TestCustomSweepActivatesEligibleIssuesWithoutQueueingWork(t *testing.T) {
	k := newCustomEnv(t)
	backlog := k.f.Issue(t, "parked", testutil.Cols{"project_id": k.project, "status": "backlog"})
	closed := k.f.Issue(t, "finished", testutil.Cols{"project_id": k.project, "status": "done"})
	elsewhere := k.f.Issue(t, "other project", testutil.Cols{"project_id": k.f.Project(t, "elsewhere")})
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweepAll(t)
	got := k.customInstances(t, view.RuleKey)
	if len(got) != 1 || got[0].Issue != k.issue || !got[0].Enabled || got[0].CapacityReason != nil {
		t.Fatalf("instances = %+v, want one enabled instance on the project's open issue (not %s, %s, %s)", got, backlog, closed, elsewhere)
	}
	if !strings.Contains(string(got[0].Condition), `"todo"`) || got[0].State != "" {
		t.Fatalf("instance = %+v: a new rule acts on facts already true, so its state starts empty", got[0])
	}
	if n := k.taskCount(t); n != 0 {
		t.Fatalf("activation queued %d tasks, want none", n)
	}
	if n := k.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.workspace_id=$1`, k.f.WorkspaceID); n != 0 {
		t.Fatalf("activation wrote %d receipts, want none", n)
	}
}

func TestCustomSweepOfAnEventRuleCreatesNothing(t *testing.T) {
	k := newCustomEnv(t)
	k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	k.sweepAll(t)
	if n := k.f.Count(t, `SELECT count(*) FROM issue_wakeup WHERE workspace_id=$1`, k.f.WorkspaceID); n != 0 {
		t.Fatalf("an event default created %d instances before any event", n)
	}
	if n := k.taskCount(t); n != 0 {
		t.Fatalf("activating an event default queued %d tasks", n)
	}
}

func TestCustomSweepIsBoundedAndResumesAfterARestart(t *testing.T) {
	k := newCustomEnv(t)
	k.bulkIssues(t, 119, k.project, "todo") // with the kit's issue: 120
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweep(t)
	if n := len(k.customInstances(t, view.RuleKey)); n != wakeupSweepBatch {
		t.Fatalf("first pass activated %d issues, want the batch of %d", n, wakeupSweepBatch)
	}
	var cursor *string
	var done bool
	read := func() {
		if err := k.f.Pool.QueryRow(context.Background(), `SELECT sweep_cursor::text,sweep_done FROM issue_wakeup_definition WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, view.RuleKey).Scan(&cursor, &done); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if cursor == nil || done {
		t.Fatalf("after one pass cursor=%v done=%v, want a persisted cursor and a sweep still pending", cursor, done)
	}
	// A new scheduler process knows nothing but the database.
	k.s = &IssueWakeupService{Tasks: k.f.svc.TaskSvc}
	k.sweep(t)
	if n := len(k.customInstances(t, view.RuleKey)); n != 2*wakeupSweepBatch {
		t.Fatalf("second pass: %d instances, want %d", n, 2*wakeupSweepBatch)
	}
	k.sweepAll(t)
	if n := len(k.customInstances(t, view.RuleKey)); n != 120 {
		t.Fatalf("finished sweep: %d instances, want 120", n)
	}
	read()
	if !done {
		t.Fatal("the sweep did not finish")
	}
}

func TestCustomSweepConcurrentPassesNeverDoubleActivate(t *testing.T) {
	k := newCustomEnv(t)
	k.bulkIssues(t, 149, k.project, "todo")
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 6 {
				if err := k.s.SweepWakeupDefinitions(context.Background(), parseTestUUID(t, k.f.WorkspaceID)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	k.sweepAll(t)
	if n, distinct := len(k.customInstances(t, view.RuleKey)), k.f.Count(t, `SELECT count(DISTINCT issue_id) FROM issue_wakeup WHERE workspace_id=$1 AND default_rule_key=$2`, k.f.WorkspaceID, view.RuleKey); n != 150 || distinct != 150 {
		t.Fatalf("instances = %d on %d distinct issues, want exactly one per issue (150)", n, distinct)
	}
}

func TestCustomSweepRevisionChangeDiscardsTheOldCursor(t *testing.T) {
	k := newCustomEnv(t)
	k.bulkIssues(t, 119, k.project, "todo")
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweep(t)
	first := map[string]bool{}
	for _, i := range k.customInstances(t, view.RuleKey) {
		first[i.Issue] = true
	}
	if len(first) != wakeupSweepBatch {
		t.Fatalf("first pass activated %d issues", len(first))
	}
	// Forget the work so a reused cursor would show: it would skip these issues.
	k.f.Exec(t, `DELETE FROM issue_wakeup WHERE workspace_id=$1 AND default_rule_key=$2`, k.f.WorkspaceID, view.RuleKey)
	k.mustSave(t, WakeupScopeProject, view.RuleKey, k.conditionRule(statusIsTodo)+`,"name":"edited"`)
	if pending := k.f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1 AND (NOT sweep_done OR sweep_revision<>revision)`, k.f.WorkspaceID); pending != 1 {
		t.Fatalf("a revised definition is pending again: %d", pending)
	}
	k.sweep(t)
	var revision, sweepRevision int64
	var baseline bool
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT revision,sweep_revision,sweep_baseline FROM issue_wakeup_definition WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, view.RuleKey).Scan(&revision, &sweepRevision, &baseline); err != nil {
		t.Fatal(err)
	}
	if revision != 2 || sweepRevision != 2 || !baseline {
		t.Fatalf("revision %d sweep revision %d baseline %v, want the sweep to follow revision 2 and baseline", revision, sweepRevision, baseline)
	}
	for _, i := range k.customInstances(t, view.RuleKey) {
		if !first[i.Issue] {
			t.Fatalf("the new sweep started from the old cursor: it reached %s, outside the first batch", i.Issue)
		}
		// Baselined: the facts already true are recorded, so the edit wakes nothing.
		if i.State != "status:todo" {
			t.Fatalf("instance %+v, want the edit to baseline the facts already true", i)
		}
	}
	if n := len(k.customInstances(t, view.RuleKey)); n != wakeupSweepBatch {
		t.Fatalf("%d instances after one pass, want %d", n, wakeupSweepBatch)
	}
	k.sweepAll(t)
	if n := len(k.customInstances(t, view.RuleKey)); n != 120 {
		t.Fatalf("%d instances, want 120", n)
	}
}

func TestCustomSweepStopsWhenTheDefinitionIsDisabledOrDeleted(t *testing.T) {
	k := newCustomEnv(t)
	k.bulkIssues(t, 119, k.project, "todo")
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweep(t)
	first := len(k.customInstances(t, view.RuleKey))
	k.mustSave(t, WakeupScopeProject, view.RuleKey, `"enabled":false,"trigger":{"kind":"condition","condition":`+statusIsTodo+`},`+k.target()+`,"instruction":"it is ready"`)
	k.sweepAll(t)
	if n := len(k.customInstances(t, view.RuleKey)); n != first {
		t.Fatalf("a disabled definition activated more issues: %d, was %d", n, first)
	}
	var cursor *string
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT sweep_cursor::text FROM issue_wakeup_definition WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, view.RuleKey).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor != nil {
		t.Fatalf("a finished sweep left a cursor: %s", *cursor)
	}
	// Deleting the definition leaves nothing pending to sweep.
	ref := k.scopeRef(t, WakeupScopeProject)
	row, err := k.f.q.GetWakeupDefinition(context.Background(), wakeupDefinitionParams(ref, view.RuleKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := k.s.DeleteWakeupDefinition(context.Background(), ref, view.RuleKey, row.Revision); err != nil {
		t.Fatal(err)
	}
	if n := k.f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1 AND (NOT sweep_done OR sweep_revision<>revision)`, k.f.WorkspaceID); n != 0 {
		t.Fatalf("%d definitions pending after the delete", n)
	}
}

func TestCustomSweepRebaselinesAnEditedPredicateAndKeepsAnInstructionEdit(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweepAll(t)
	inst := k.customInstances(t, view.RuleKey)[0]
	// The condition fired already: its facts are recorded in the instance.
	k.f.Exec(t, `UPDATE issue_wakeup SET condition_state='status:todo' WHERE id=$1`, inst.ID)

	k.mustSave(t, WakeupScopeProject, view.RuleKey, strings.Replace(k.conditionRule(statusIsTodo), "it is ready", "a new instruction", 1))
	k.sweepAll(t)
	after := k.customInstances(t, view.RuleKey)[0]
	if after.State != "status:todo" || after.Revision != inst.Revision+1 {
		t.Fatalf("instruction-only edit: %+v, want the recorded facts kept on a new revision", after)
	}

	// The predicate now watches another value the issue already satisfies... it does not: it is todo.
	k.mustSave(t, WakeupScopeProject, view.RuleKey, k.conditionRule(`{"type":"issue_field","field":"status","value":"in_review"}`))
	k.f.Exec(t, `UPDATE issue SET status='in_review' WHERE id=$1`, k.issue)
	k.sweepAll(t)
	rebased := k.customInstances(t, view.RuleKey)[0]
	if rebased.State != "status:in_review" {
		t.Fatalf("edited predicate: state %q, want the facts already true recorded so the edit does not fire", rebased.State)
	}
}

func TestCustomActivationSignalAdmitsIssuesThatBecomeEligible(t *testing.T) {
	k := newCustomEnv(t)
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweepAll(t)
	created := k.f.Issue(t, "created after the sweep", testutil.Cols{"project_id": k.project})
	parked := k.f.Issue(t, "parked when created", testutil.Cols{"project_id": k.project, "status": "backlog"})
	k.drain(t)
	byIssue := map[string]customInstance{}
	for _, i := range k.customInstances(t, view.RuleKey) {
		byIssue[i.Issue] = i
	}
	if i, ok := byIssue[created]; !ok || !i.Enabled || i.State != "status:todo" {
		t.Fatalf("a new issue: %+v (present %v), want an instance baselined on the facts already true", i, ok)
	}
	if _, ok := byIssue[parked]; ok {
		t.Fatal("an issue created in backlog gained an instance")
	}
	k.f.Exec(t, `UPDATE issue SET status='todo' WHERE id=$1`, parked)
	k.drain(t)
	if i, ok := func() (customInstance, bool) {
		for _, i := range k.customInstances(t, view.RuleKey) {
			if i.Issue == parked {
				return i, true
			}
		}
		return customInstance{}, false
	}(); !ok || i.State != "status:todo" {
		t.Fatalf("an issue leaving backlog: %+v (present %v), want an instance baselined on the facts already true", i, ok)
	}
	if n := k.taskCount(t); n != 0 {
		t.Fatalf("activation queued %d tasks", n)
	}
}

func TestCustomSweepRetriesInstancesThatTheFullDefaultPoolHeldBack(t *testing.T) {
	k := newCustomEnv(t)
	k.f.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,default_rule_key,default_scope_kind,default_scope_id)
 SELECT gen_random_uuid(),$1,$2,$3,'default','event','continuous',gen_random_uuid()::text,'workspace',$1 FROM generate_series(1,32)`, k.f.WorkspaceID, k.issue, k.agent)
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweepAll(t)
	got := k.customInstances(t, view.RuleKey)
	if len(got) != 1 || got[0].Enabled || got[0].CapacityReason == nil {
		t.Fatalf("instances = %+v, want one held back by the full pool with its reason", got)
	}
	k.sweep(t)
	if got := k.customInstances(t, view.RuleKey); got[0].Enabled {
		t.Fatal("applied while the pool was still full")
	}
	k.f.Exec(t, `DELETE FROM issue_wakeup WHERE issue_id=$1 AND default_rule_key<>$2`, k.issue, view.RuleKey)
	k.sweep(t)
	if got := k.customInstances(t, view.RuleKey); !got[0].Enabled || got[0].CapacityReason != nil {
		t.Fatalf("instance = %+v, want it applied once capacity freed", got[0])
	}
}

// ---- execution ----------------------------------------------------------

// plan returns the EXPLAIN text of a query, run on its own connection.
func (k customEnv) plan(t *testing.T, sql string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	conn, err := k.f.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, sql)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	text := strings.Join(lines, "\n")
	t.Logf("EXPLAIN (ANALYZE, BUFFERS):\n%s\n%s", sql, text)
	return text
}

// The sweep never scans the workspace's issues or every definition: the pending
// definitions, a scope's next batch and the held-back instances each come from
// an index built for it, on a fixture whose other workspaces and projects are
// far larger than the one swept.
func TestCustomSweepQueriesAreBoundedByTheirIndexes(t *testing.T) {
	k := newCustomEnv(t)
	k.f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE config->>'name'='explain-neighbour'")
	// A large project is swept next to a large neighbourhood: 4,000 settled
	// definitions of other scopes and 6,000 issues of another project.
	k.f.Exec(t, `INSERT INTO issue_wakeup_definition(workspace_id,scope_kind,scope_id,rule_key,root,config,sweep_done,sweep_revision)
 SELECT gen_random_uuid(),'project',gen_random_uuid(),gen_random_uuid()::text,false,'{"v":1,"name":"explain-neighbour"}'::jsonb,true,1 FROM generate_series(1,4000) g`)
	other := k.f.Project(t, "explain neighbour")
	k.bulkIssues(t, 6000, other, "todo")
	k.bulkIssues(t, 6000, k.project, "todo")
	k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.f.Cleanup(t, "DELETE FROM issue_wakeup WHERE instruction='explain-neighbour'")
	k.f.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,enabled)
 SELECT gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),$1,'explain-neighbour','event','continuous',false FROM generate_series(1,6000)`, k.agent)
	k.f.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,default_rule_key,default_scope_kind,default_scope_id,capacity_reason,enabled)
 SELECT gen_random_uuid(),$1,$2,$3,'held','event','continuous',gen_random_uuid()::text,'workspace',$1,'default',false FROM generate_series(1,3)`, k.f.WorkspaceID, k.issue, k.agent)
	for _, table := range []string{"issue_wakeup_definition", "issue", "issue_wakeup"} {
		k.f.Exec(t, "ANALYZE "+table)
	}

	claim := k.plan(t, `SELECT workspace_id,scope_kind,scope_id,rule_key FROM issue_wakeup_definition
 WHERE (NOT sweep_done OR sweep_revision <> revision) AND (sweep_retry_at IS NULL OR sweep_retry_at <= now()) AND ($1::uuid[] IS NULL OR workspace_id = ANY($1::uuid[]))
 ORDER BY updated_at,rule_key LIMIT 1`, nil) // the scheduler sweeps every workspace
	if !strings.Contains(claim, "issue_wakeup_definition_sweep_idx") || strings.Contains(claim, "Seq Scan") {
		t.Fatalf("finding the pending sweep must use the partial index:\n%s", claim)
	}
	project := k.plan(t, `SELECT id FROM issue WHERE project_id=$1 AND workspace_id=$2 AND id>'00000000-0000-0000-0000-000000000000' ORDER BY id LIMIT 50`, k.project, k.f.WorkspaceID)
	// The planner may walk the project's own keyset index or the workspace's with
	// the project as a filter, whichever it costs lower; either stops after the
	// batch, which is the bound: never a sort of the project, never a table scan.
	if !strings.Contains(project, "_keyset") || strings.Contains(project, "Seq Scan") || strings.Contains(project, "Sort") {
		t.Fatalf("a project's next batch must be an ordered keyset scan:\n%s", project)
	}
	workspace := k.plan(t, `SELECT id FROM issue WHERE workspace_id=$1 AND id>'00000000-0000-0000-0000-000000000000' ORDER BY id LIMIT 50`, k.f.WorkspaceID)
	if !strings.Contains(workspace, "idx_issue_workspace_id_keyset") || strings.Contains(workspace, "Seq Scan") || strings.Contains(workspace, "Sort") {
		t.Fatalf("a workspace's next batch must be an ordered index scan:\n%s", workspace)
	}
	held := k.plan(t, `SELECT id,issue_id,workspace_id FROM issue_wakeup WHERE capacity_reason IS NOT NULL AND NOT enabled AND default_rule_key IS NOT NULL AND system_rule IS NULL
 AND ($1::uuid[] IS NULL OR workspace_id = ANY($1::uuid[])) ORDER BY updated_at,id LIMIT 50`, nil)
	if !strings.Contains(held, "issue_wakeup_capacity_held_idx") || strings.Contains(held, "Seq Scan") {
		t.Fatalf("finding held-back instances must use the partial index:\n%s", held)
	}
}

// failInstanceWrites makes every instance insert of the fixture's workspace fail
// with a real database error until the returned function is called.
func (k customEnv) failInstanceWrites(t *testing.T) (heal func()) {
	t.Helper()
	name := "che1200_fail_" + strings.ReplaceAll(k.f.WorkspaceID[:8], "-", "")
	k.f.Exec(t, `CREATE OR REPLACE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.workspace_id='`+k.f.WorkspaceID+`' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$`)
	k.f.Exec(t, `CREATE TRIGGER `+name+` BEFORE INSERT ON issue_wakeup FOR EACH ROW EXECUTE FUNCTION `+name+`()`)
	healed := false
	heal = func() {
		if !healed {
			healed = true
			k.f.Exec(t, `DROP TRIGGER `+name+` ON issue_wakeup`)
			k.f.Exec(t, `DROP FUNCTION `+name+`()`)
		}
	}
	t.Cleanup(heal)
	return heal
}

func (k customEnv) sweepState(t *testing.T, rule string) (attempts int, retryInFuture, done bool, errText string) {
	t.Helper()
	var retry *time.Time
	var e *string
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT sweep_attempts,sweep_retry_at,sweep_done,sweep_error FROM issue_wakeup_definition WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, rule).Scan(&attempts, &retry, &done, &e); err != nil {
		t.Fatal(err)
	}
	if e != nil {
		errText = *e
	}
	return attempts, retry != nil && retry.After(time.Now()), done, errText
}

func TestCustomSweepRetriesDatabaseErrorsWithBackoffAndParksThem(t *testing.T) {
	k := newCustomEnv(t)
	heal := k.failInstanceWrites(t)
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	if err := k.s.SweepWakeupDefinitions(context.Background(), parseTestUUID(t, k.f.WorkspaceID)); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("a real database error must be reported, got %v", err)
	}
	attempts, delayed, done, text := k.sweepState(t, view.RuleKey)
	if attempts != 1 || !delayed || done || !strings.Contains(text, "injected failure") {
		t.Fatalf("after one failure: attempts=%d delayed=%v done=%v error=%q, want a delayed retry with the error visible", attempts, delayed, done, text)
	}
	// Until the delay passes the sweep is left alone.
	k.sweep(t)
	if a, _, _, _ := k.sweepState(t, view.RuleKey); a != 1 {
		t.Fatalf("a delayed sweep was retried at once: %d attempts", a)
	}
	// The error clears: the sweep resumes and finishes.
	heal()
	k.f.Exec(t, `UPDATE issue_wakeup_definition SET sweep_retry_at=now()-interval '1 second' WHERE workspace_id=$1 AND rule_key=$2`, k.f.WorkspaceID, view.RuleKey)
	k.sweepAll(t)
	if n := len(k.customInstances(t, view.RuleKey)); n != 1 {
		t.Fatalf("%d instances after the retry, want 1", n)
	}
	if a, _, _, text := k.sweepState(t, view.RuleKey); a != 0 || text != "" {
		t.Fatalf("a finished sweep keeps attempts=%d error=%q", a, text)
	}

	// A sweep that keeps failing is parked, visibly, for this revision.
	k.f.Exec(t, `DELETE FROM issue_wakeup WHERE workspace_id=$1 AND default_rule_key=$2`, k.f.WorkspaceID, view.RuleKey)
	k.failInstanceWrites(t)
	k.mustSave(t, WakeupScopeProject, view.RuleKey, k.conditionRule(statusIsTodo)+`,"name":"again"`)
	for range wakeupSweepMaxAttempts {
		_ = k.s.SweepWakeupDefinitions(context.Background(), parseTestUUID(t, k.f.WorkspaceID))
		k.f.Exec(t, `UPDATE issue_wakeup_definition SET sweep_retry_at=now()-interval '1 second' WHERE workspace_id=$1 AND rule_key=$2 AND NOT sweep_done`, k.f.WorkspaceID, view.RuleKey)
	}
	attempts, _, done, text = k.sweepState(t, view.RuleKey)
	if attempts != wakeupSweepMaxAttempts || !done || !strings.Contains(text, "injected failure") {
		t.Fatalf("after %d failures: attempts=%d done=%v error=%q, want it parked with the error visible", wakeupSweepMaxAttempts, attempts, done, text)
	}
	k.sweep(t) // parked: nothing is retried
	if a, _, _, _ := k.sweepState(t, view.RuleKey); a != wakeupSweepMaxAttempts {
		t.Fatalf("a parked sweep was retried: %d attempts", a)
	}
}

func TestCustomSweepDoesNotSkipAnIssueAnotherWriterHolds(t *testing.T) {
	k := newCustomEnv(t)
	k.bulkIssues(t, 9, k.project, "todo")
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	ctx := context.Background()
	// Another writer holds one issue's row for longer than the sweep waits.
	held, err := k.f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(ctx)
	if _, err := held.Exec(ctx, `SELECT id FROM issue WHERE id=$1 FOR UPDATE`, k.issue); err != nil {
		t.Fatal(err)
	}
	k.sweep(t)
	if n := len(k.customInstances(t, view.RuleKey)); n == 10 {
		t.Fatalf("the held issue was activated through its lock")
	}
	if done := k.f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1 AND sweep_done`, k.f.WorkspaceID); done != 0 {
		t.Fatal("a sweep that could not reach an issue finished without it")
	}
	if err := held.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	k.sweepAll(t)
	if got := len(k.customInstances(t, view.RuleKey)); got != 10 {
		t.Fatalf("%d instances after the lock was released, want all 10", got)
	}
}

// An issue write pays for the activation signal with the same indexed probe as
// every other capture: nothing where no definition asks for it, and a bounded
// lookup at the ceiling of definitions in the issue's three scopes. Rounds
// alternate so a loaded host skews both alike; the budget only catches a gross
// regression such as a scan, and the logged numbers are the measurement.
func TestCustomActivationCaptureOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("timing measurement")
	}
	k := newCustomEnv(t)
	ctx := context.Background()
	const writes, rounds = 300, 7
	update := func() time.Duration {
		start := time.Now()
		for i := range writes {
			status := []string{"todo", "in_progress"}[i%2]
			if _, err := k.f.Pool.Exec(ctx, `UPDATE issue SET status=$2 WHERE id=$1`, k.issue, status); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(start) / writes
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
	update() // warm the plans
	var none, atCeiling []time.Duration
	for range rounds {
		ceiling(false)
		none = append(none, update())
		ceiling(true)
		atCeiling = append(atCeiling, update())
	}
	// Server-side cost of the activation probe alone, plan cached, no round trips:
	// the worst case is a probe that matches nothing in 96 candidate rows.
	ceiling(true)
	conn, err := k.f.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const calls = 5000
	if _, err := conn.Exec(ctx, `DO $$ DECLARE s timestamptz; b boolean; BEGIN s:=clock_timestamp();
 FOR i IN 1..`+strconv.Itoa(calls)+` LOOP b:=wakeup_scoped_event_wanted('`+k.issue+`','issue.activate'); END LOOP;
 PERFORM set_config('che1200.us',(extract(epoch FROM clock_timestamp()-s)*1000000)::text,false); END $$`); err != nil {
		t.Fatal(err)
	}
	var micros float64
	if err := conn.QueryRow(ctx, `SELECT current_setting('che1200.us')::float8`).Scan(&micros); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	t.Logf("activation probe at the ceiling of 96 non-matching definitions: %.1fµs per call", micros/calls)
	if micros/calls > 1000 {
		t.Fatalf("the activation probe costs %.0fµs per call", micros/calls)
	}
	ceiling(false)
	k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	matching := update()
	k.f.Exec(t, `DELETE FROM wakeup_scoped_event WHERE workspace_id=$1`, k.f.WorkspaceID)
	noneMedian, ceilingMedian := median(none), median(atCeiling)
	t.Logf("issue status update, median of %d rounds of %d writes: no definition %v, 96 non-matching definitions %v; one matching condition rule (one outbox row per write) %v",
		rounds, writes, noneMedian, ceilingMedian, matching)
	if ceilingMedian > noneMedian+time.Millisecond && ceilingMedian > 3*noneMedian {
		t.Fatalf("a write with %d non-matching definitions costs %v against %v with none", 96, ceilingMedian, noneMedian)
	}
}

func TestCustomSweepSkipsAnIssueMovedAwayMidSweep(t *testing.T) {
	k := newCustomEnv(t)
	k.bulkIssues(t, 119, k.project, "todo")
	other := k.f.Project(t, "moved to")
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweep(t)
	done := map[string]bool{}
	for _, i := range k.customInstances(t, view.RuleKey) {
		done[i.Issue] = true
	}
	// An issue the sweep has not reached yet leaves the project between passes.
	var moved string
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT id::text FROM issue WHERE project_id=$1 AND id>(SELECT sweep_cursor FROM issue_wakeup_definition WHERE workspace_id=$2 AND rule_key=$3) ORDER BY id LIMIT 1`,
		k.project, k.f.WorkspaceID, view.RuleKey).Scan(&moved); err != nil {
		t.Fatal(err)
	}
	k.f.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, moved, other)
	k.sweepAll(t)
	for _, i := range k.customInstances(t, view.RuleKey) {
		if i.Issue == moved {
			t.Fatal("the sweep activated an issue that had left the project")
		}
	}
	if n := len(k.customInstances(t, view.RuleKey)); n != 119 {
		t.Fatalf("%d instances, want the 119 issues that stayed", n)
	}
}

// A held-back instance whose issue closed meanwhile rests with the issue; the
// capacity that frees afterwards must not rearm it.
func TestCustomCapacityRetryNeverRearmsAnInstanceOfAClosedIssue(t *testing.T) {
	k := newCustomEnv(t)
	k.f.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,default_rule_key,default_scope_kind,default_scope_id)
 SELECT gen_random_uuid(),$1,$2,$3,'default','event','continuous',gen_random_uuid()::text,'workspace',$1 FROM generate_series(1,32)`, k.f.WorkspaceID, k.issue, k.agent)
	view := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(statusIsTodo))
	k.sweepAll(t)
	if got := k.customInstances(t, view.RuleKey); len(got) != 1 || got[0].CapacityReason == nil {
		t.Fatalf("instances = %+v, want one held back by the full pool", got)
	}
	wakeSetStatus(t, k.f, parseTestUUID(t, k.issue), "done")
	k.f.Exec(t, `DELETE FROM issue_wakeup WHERE issue_id=$1 AND default_rule_key<>$2`, k.issue, view.RuleKey)
	k.sweep(t)
	k.mustSave(t, WakeupScopeProject, view.RuleKey, k.conditionRule(statusIsTodo)+`,"name":"edited"`)
	k.sweepAll(t)
	if w := k.instanceRow(t, view.RuleKey); w.Enabled || w.CapacityReason.Valid {
		t.Fatalf("capacity freed: enabled=%v capacity reason %v, want the closed issue's instance left at rest and out of the retry list", w.Enabled, w.CapacityReason)
	}
}
