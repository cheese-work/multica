package service

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Aggregate admission (CHE-1082 L7): durable shared starts/hour budgets for
// scoped default rules.

// issueIn adds one more issue the fixture's agent owns, in the given project
// (invalid = none), so several instances of one rule can share a counter.
func (e builtinEnv) issueIn(t *testing.T, project pgtype.UUID) pgtype.UUID {
	t.Helper()
	issue := parseTestUUID(t, e.f.Issue(t, "aggregate issue"))
	e.f.Exec(t, `UPDATE issue SET status='in_progress',assignee_type='agent',assignee_id=$2,project_id=$3 WHERE id=$1`, issue, e.agent, project)
	e.f.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id=$1)", issue)
	e.f.Cleanup(t, "DELETE FROM issue_wakeup_pr_event WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id=$1)", issue)
	e.f.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
	e.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", issue)
	return issue
}

// bareTask is a queued task of the fixture's agent that no wakeup made, for
// tests that drive admission directly.
func (e builtinEnv) bareTask(t *testing.T) pgtype.UUID {
	t.Helper()
	issue := e.f.Issue(t, "bare task issue")
	var runtime pgtype.UUID
	if err := e.f.Pool.QueryRow(context.Background(), `SELECT runtime_id FROM agent WHERE id=$1`, e.agent).Scan(&runtime); err != nil {
		t.Fatal(err)
	}
	return parseTestUUID(t, e.f.Task(t, e.agent, testutil.Cols{"issue_id": issue, "runtime_id": util.UUIDToString(runtime)}))
}

func (e builtinEnv) newProject(t *testing.T, name string) pgtype.UUID {
	t.Helper()
	return parseTestUUID(t, e.f.Project(t, name))
}

func (e builtinEnv) mergedOn(t *testing.T, issue pgtype.UUID, number int32) {
	t.Helper()
	in := PullRequestWakeupInput{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: number,
		URL: "https://github.com/acme/widget/pull/" + util.UUIDToString(issue), MergeCommit: "merge", BaseBranch: "main", HeadBranch: "feature"}
	if err := e.s.TriggerPullRequestWakeup(context.Background(), issue, in); err != nil {
		t.Fatal(err)
	}
}

func (e builtinEnv) defineAt(t *testing.T, kind WakeupScope, scopeID pgtype.UUID, key, fields string) {
	t.Helper()
	ref := WakeupScopeRef{Kind: kind, ID: scopeID, WorkspaceID: parseTestUUID(t, e.f.WorkspaceID)}
	var revision int64
	if row, err := e.f.q.GetWakeupDefinition(context.Background(), wakeupDefinitionParams(ref, key)); err == nil {
		revision = row.Revision
	}
	if _, err := e.s.SaveWakeupDefinition(context.Background(), ref, e.owner, WakeupDefinitionWrite{RuleKey: key, Revision: revision, Patch: patchOf(t, fields)}); err != nil {
		t.Fatalf("define %s/%s %s: %v", kind, key, fields, err)
	}
}

func (e builtinEnv) runs(t *testing.T, issue pgtype.UUID) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'='pr_merged'`, issue)
}

func (e builtinEnv) pendingOn(t *testing.T, issue pgtype.UUID) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND r.processed_at IS NULL`, issue)
}

// redispatch makes the instance's retry gate due and dispatches it, as the
// scheduler does once the gate has passed.
func (e builtinEnv) redispatch(t *testing.T, issue pgtype.UUID) {
	t.Helper()
	e.f.Exec(t, `UPDATE issue_wakeup SET aggregate_retry_at=aggregate_retry_at-interval '1 day' WHERE issue_id=$1 AND aggregate_retry_at IS NOT NULL`, issue)
	w, err := e.f.q.GetSystemWakeup(context.Background(), db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRMerged)})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.dispatchSystem(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}

func (e builtinEnv) cancelRuns(t *testing.T, issue pgtype.UUID) {
	t.Helper()
	e.f.Exec(t, `UPDATE agent_task_queue SET status='cancelled',completed_at=now(),error='cancelled' WHERE issue_id=$1 AND status='queued' AND started_at IS NULL`, issue)
}

func (e builtinEnv) budgets(t *testing.T) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM wakeup_aggregate_budget WHERE workspace_id=$1`, e.f.WorkspaceID)
}

func (e builtinEnv) cleanAggregate(t *testing.T) {
	t.Helper()
	e.f.Cleanup(t, "DELETE FROM wakeup_aggregate_reservation WHERE workspace_id=$1", e.f.WorkspaceID)
	e.f.Cleanup(t, "DELETE FROM wakeup_aggregate_budget WHERE workspace_id=$1", e.f.WorkspaceID)
}

// fire merges one PR on each of the issues in turn and reports how many runs
// each now holds.
func (e builtinEnv) fire(t *testing.T, issues []pgtype.UUID) []int {
	t.Helper()
	out := make([]int, len(issues))
	for i, issue := range issues {
		e.mergedOn(t, issue, 500)
		out[i] = e.runs(t, issue)
	}
	return out
}

func sum(xs []int) (n int) {
	for _, x := range xs {
		n += x
	}
	return n
}

// The 13th start of a 12/hour cap is delayed, not dropped: its fact stays
// pending, the instance reports when to retry, and the start happens once a
// slot frees.
func TestAggregateThirteenthStartIsDelayed(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"aggregate_limit":12`)
	issues := make([]pgtype.UUID, 13)
	for i := range issues {
		issues[i] = e.issueIn(t, e.project)
	}
	got := e.fire(t, issues)
	if sum(got[:12]) != 12 || got[12] != 0 {
		t.Fatalf("starts per issue = %v, want twelve admitted and the 13th delayed", got)
	}
	if e.pendingOn(t, issues[12]) != 1 {
		t.Fatalf("the delayed fact was not kept pending")
	}
	w, err := e.f.q.GetSystemWakeup(context.Background(), db.GetSystemWakeupParams{IssueID: issues[12], SystemRule: systemRuleText(SystemRulePRMerged)})
	if err != nil {
		t.Fatal(err)
	}
	if !w.Enabled || w.PausedReason.Valid || !w.AggregateRetryAt.Valid || w.AggregateBlockedScopeKind.String != "project" || w.AggregateBlockedScopeID != e.project {
		t.Fatalf("the delayed instance must stay enabled, unpaused and name its blocking counter: %+v", w)
	}
	// Still full on a retry: nothing starts, nothing is lost.
	e.redispatch(t, issues[12])
	if e.runs(t, issues[12]) != 0 || e.pendingOn(t, issues[12]) != 1 {
		t.Fatalf("a retry over a full counter must change nothing")
	}
	e.cancelRuns(t, issues[0])
	e.redispatch(t, issues[12])
	if e.runs(t, issues[12]) != 1 || e.pendingOn(t, issues[12]) != 0 {
		t.Fatalf("the delayed start did not run after a slot freed")
	}
	if w, _ = e.f.q.GetSystemWakeup(context.Background(), db.GetSystemWakeupParams{IssueID: issues[12], SystemRule: systemRuleText(SystemRulePRMerged)}); w.AggregateRetryAt.Valid || w.AggregateBlockedScopeID.Valid {
		t.Fatalf("an admitted instance still carries its delay marker")
	}
}

// One counter covers every instance below its scope; another project's
// instances use their own and an unrelated rule key has none.
func TestAggregateCounterIsSharedAcrossInstancesAndIndependentPerKey(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"aggregate_limit":2`)
	a := []pgtype.UUID{e.issueIn(t, e.project), e.issueIn(t, e.project), e.issueIn(t, e.project)}
	if got := e.fire(t, a); got[0] != 1 || got[1] != 1 || got[2] != 0 {
		t.Fatalf("project cap 2 admitted %v across three instances", got)
	}
	// Another project has no cap of its own.
	other := e.newProject(t, "uncapped project")
	b := []pgtype.UUID{e.issueIn(t, other), e.issueIn(t, other), e.issueIn(t, other)}
	if got := e.fire(t, b); sum(got) != 3 {
		t.Fatalf("an uncapped project was throttled by a sibling's counter: %v", got)
	}
	// The cap belongs to pr_merged only.
	c := e.issueIn(t, e.project)
	err := e.s.TriggerPullRequestWakeup(context.Background(), c, PullRequestWakeupInput{Rule: SystemRulePRChecksFailed,
		RepoOwner: "acme", RepoName: "widget", Number: 7, URL: "https://github.com/acme/widget/pull/7", HeadSHA: "abc", Conclusion: "FAILURE"})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'='pr_checks_failed'`, c); n != 1 {
		t.Fatalf("a different rule key was throttled by pr_merged's counter: %d runs", n)
	}
}

// An instruction-only override adds no throttle: with no ancestor cap there is
// no counter at all, with one it inherits that counter and creates none.
func TestAggregateInstructionOnlyOverrideInheritsOrAddsNothing(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"Project way"`)
	issues := make([]pgtype.UUID, 14)
	for i := range issues {
		issues[i] = e.issueIn(t, e.project)
	}
	if got := e.fire(t, issues); sum(got) != 14 {
		t.Fatalf("legacy defaults must stay uncapped across issues: %v", got)
	}
	if e.budgets(t) != 0 {
		t.Fatalf("no cap was installed, yet %d budget rows exist", e.budgets(t))
	}

	e.define(t, WakeupScopeWorkspace, SystemRulePRMerged, `"aggregate_limit":2`)
	more := []pgtype.UUID{e.issueIn(t, e.project), e.issueIn(t, e.project), e.issueIn(t, e.project)}
	if got := e.fire(t, more); got[0] != 1 || got[1] != 1 || got[2] != 0 {
		t.Fatalf("the override must inherit the workspace cap of 2: %v", got)
	}
	if n := e.f.Count(t, `SELECT count(*) FROM wakeup_aggregate_budget WHERE workspace_id=$1 AND scope_kind='project'`, e.f.WorkspaceID); n != 0 {
		t.Fatalf("an override acquired its own counter")
	}
}

// Every applicable cap is enforced: a project may be stricter than the
// workspace but never looser, and clearing the local cap leaves the ancestor's.
func TestAggregateAncestorCapsAreAllEnforced(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	batch := func(n int) []pgtype.UUID {
		out := make([]pgtype.UUID, n)
		for i := range out {
			out[i] = e.issueIn(t, e.project)
		}
		return out
	}
	e.define(t, WakeupScopeWorkspace, SystemRulePRMerged, `"aggregate_limit":3`)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"aggregate_limit":10`)
	first := batch(5)
	if got := e.fire(t, first); sum(got) != 3 {
		t.Fatalf("a project cap of 10 lifted the workspace cap of 3: %v", got)
	}
	// Free the workspace counter and drop the facts it delayed (they would
	// rightly be first in line), then make the project stricter: only its cap binds.
	for _, issue := range first {
		e.cancelRuns(t, issue)
	}
	e.f.Exec(t, `UPDATE issue_wakeup_receipt SET processed_at=now() WHERE processed_at IS NULL AND wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id=ANY($1))`, first)
	e.defineAt(t, WakeupScopeProject, e.project, SystemRulePRMerged, `"aggregate_limit":1`)
	if got := e.fire(t, batch(3)); sum(got) != 1 {
		t.Fatalf("a stricter project cap of 1 admitted %v", got)
	}
	// Clearing the project's own cap leaves the workspace's: 1 of 3 is used.
	e.defineAt(t, WakeupScopeProject, e.project, SystemRulePRMerged, `"aggregate_limit":null`)
	if got := e.fire(t, batch(3)); sum(got) != 2 {
		t.Fatalf("with the project cap cleared the workspace cap of 3 leaves room for 2 starts: %v", got)
	}
}

// A run cancelled before it started returns its slot; counting it again, or
// cancelling it twice, never frees or takes a second one.
func TestAggregateCancellationReleasesAndIsIdempotent(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"aggregate_limit":1`)
	first, second := e.issueIn(t, e.project), e.issueIn(t, e.project)
	e.mergedOn(t, first, 1)
	e.mergedOn(t, second, 2)
	if e.runs(t, first) != 1 || e.runs(t, second) != 0 {
		t.Fatal("cap of 1 must admit exactly the first start")
	}
	e.cancelRuns(t, first)
	e.cancelRuns(t, first)
	e.redispatch(t, second)
	e.redispatch(t, second)
	if e.runs(t, first) != 1 || e.runs(t, second) != 1 {
		t.Fatalf("after a queued cancel the slot must pass to the waiting instance exactly once (first=%d second=%d)", e.runs(t, first), e.runs(t, second))
	}
	// A run that really started keeps its slot for the hour.
	e.f.Exec(t, `UPDATE agent_task_queue SET status='running',started_at=now() WHERE issue_id=$1`, second)
	third := e.issueIn(t, e.project)
	e.mergedOn(t, third, 3)
	if e.runs(t, third) != 0 {
		t.Fatal("a started run must keep counting")
	}
}

// Reserving the same task again (a retried dispatch) never takes a second slot.
func TestAggregateReservationIsIdempotent(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	ctx := context.Background()
	ws := parseTestUUID(t, e.f.WorkspaceID)
	scope := WakeupAggregateCap{Scope: WakeupScopeProject, ScopeID: e.project, Limit: 1}
	task := e.bareTask(t)
	now := time.Now()
	for i := range 3 {
		tx, err := e.s.Tasks.TxStarter.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		adm, err := admitAggregateStart(ctx, e.s.Tasks.Queries.WithTx(tx), aggregateStart{WorkspaceID: ws, RuleKey: SystemRulePRMerged, WakeupID: task, TaskID: task}, []WakeupAggregateCap{scope}, now)
		if err != nil || !adm.Admitted {
			t.Fatalf("attempt %d: admitted=%v err=%v; a retry of the reserved task must be admitted again without a second slot", i, adm.Admitted, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.f.Count(t, `SELECT count(*) FROM wakeup_aggregate_reservation WHERE workspace_id=$1`, e.f.WorkspaceID); n != 1 {
		t.Fatalf("%d reservations for one task", n)
	}
}

// Many writers racing for the last slots admit exactly the cap.
func TestAggregateConcurrentReservationsAdmitExactlyTheCap(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	ctx := context.Background()
	ws := parseTestUUID(t, e.f.WorkspaceID)
	const limit, writers = 5, 30
	caps := []WakeupAggregateCap{{Scope: WakeupScopeWorkspace, ScopeID: ws, Limit: 50}, {Scope: WakeupScopeProject, ScopeID: e.project, Limit: limit}}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		task := e.bareTask(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := e.s.Tasks.TxStarter.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback(ctx)
			adm, err := admitAggregateStart(ctx, e.s.Tasks.Queries.WithTx(tx), aggregateStart{WorkspaceID: ws, RuleKey: SystemRulePRMerged, WakeupID: task, TaskID: task}, caps, time.Now())
			if err != nil {
				errs <- err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
				return
			}
			if adm.Admitted {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if admitted.Load() != limit {
		t.Fatalf("%d of %d racing starts admitted, want exactly %d", admitted.Load(), writers, limit)
	}
	if n := e.f.Count(t, `SELECT count(*) FROM wakeup_aggregate_reservation WHERE workspace_id=$1 AND scope_kind='project'`, e.f.WorkspaceID); n != limit {
		t.Fatalf("%d project reservations, want %d", n, limit)
	}
	// A refused start reserves nothing on the broader counter either.
	if n := e.f.Count(t, `SELECT count(*) FROM wakeup_aggregate_reservation WHERE workspace_id=$1 AND scope_kind='workspace'`, e.f.WorkspaceID); n != limit {
		t.Fatalf("%d workspace reservations, want %d", n, limit)
	}
}

// One blocked project neither starves another nor keeps occupying the
// scheduler's batch until its counter has room.
func TestAggregateBlockedProjectDoesNotStarveOthers(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"aggregate_limit":1`)
	blocked := []pgtype.UUID{e.issueIn(t, e.project), e.issueIn(t, e.project), e.issueIn(t, e.project)}
	e.fire(t, blocked)
	other := e.newProject(t, "fair project")
	e.defineAt(t, WakeupScopeProject, other, SystemRulePRMerged, `"aggregate_limit":1`)
	fair := e.issueIn(t, other)
	e.mergedOn(t, fair, 9)
	if e.runs(t, fair) != 1 {
		t.Fatal("the other project's own counter must admit its first start")
	}
	for _, issue := range blocked[1:] {
		if e.runs(t, issue) != 0 || e.pendingOn(t, issue) != 1 {
			t.Fatal("a blocked fact must stay pending")
		}
	}
	// Blocked instances leave the scheduler's candidate list until they are due.
	ready, err := e.f.q.ListReadyWakeups(context.Background(), []pgtype.UUID{parseTestUUID(t, e.f.WorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ready {
		if w.IssueID == blocked[1] || w.IssueID == blocked[2] {
			t.Fatalf("a blocked instance is a scheduler candidate before its retry time")
		}
	}
	e.f.Exec(t, `UPDATE issue_wakeup SET aggregate_retry_at=now()-interval '1 second' WHERE issue_id=ANY($1)`, []pgtype.UUID{blocked[1], blocked[2]})
	if ready, err = e.f.q.ListReadyWakeups(context.Background(), []pgtype.UUID{parseTestUUID(t, e.f.WorkspaceID)}); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, w := range ready {
		if w.IssueID == blocked[1] || w.IssueID == blocked[2] {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("due blocked instances are not scheduler candidates (%d of 2)", found)
	}
}

// The backlog and earliest retry of a counter are plain data for the UI layers.
func TestAggregateStatusReportsUsedBacklogAndEarliestRetry(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"aggregate_limit":1`)
	issues := []pgtype.UUID{e.issueIn(t, e.project), e.issueIn(t, e.project), e.issueIn(t, e.project)}
	e.fire(t, issues)
	st, err := WakeupAggregateStatusOf(context.Background(), e.f.q, parseTestUUID(t, e.f.WorkspaceID), WakeupAggregateCap{Scope: WakeupScopeProject, ScopeID: e.project, Limit: 1}, SystemRulePRMerged, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if st.Used != 1 || st.Limit != 1 || st.Backlog != 2 || st.EarliestRetry.IsZero() || st.EarliestRetry.After(time.Now().Add(time.Hour+time.Minute)) {
		t.Fatalf("status = %+v, want used 1/1, backlog 2 and a retry within the hour", st)
	}
	free, err := WakeupAggregateStatusOf(context.Background(), e.f.q, parseTestUUID(t, e.f.WorkspaceID), WakeupAggregateCap{Scope: WakeupScopeProject, ScopeID: e.project, Limit: 5}, "00000000-0000-4000-8000-000000000001", time.Now())
	if err != nil || free.Used != 0 || free.Backlog != 0 || !free.EarliestRetry.IsZero() {
		t.Fatalf("an untouched counter must read as empty: %+v err=%v", free, err)
	}
}

// Creating a custom root rule is the only write that inserts the 12/hour
// default; an explicit value wins, and an issue-scope cap is refused.
func TestAggregateCustomRootDefaultAndScopeRules(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	ctx := context.Background()
	ws := parseTestUUID(t, e.f.WorkspaceID)
	ref := WakeupScopeRef{Kind: WakeupScopeProject, ID: e.project, WorkspaceID: ws}
	fields := `"enabled":true,"trigger":{"kind":"pr_merged"},"instruction":"custom root"`
	created, err := e.s.SaveWakeupDefinition(ctx, ref, e.owner, WakeupDefinitionWrite{Patch: patchOf(t, fields)})
	if err != nil {
		t.Fatal(err)
	}
	if p := created.Patch.AggregateLimit; !p.Set || p.Null || p.Value != 12 {
		t.Fatalf("a new custom root has aggregate_limit %+v, want the 12/hour default", p)
	}
	rule, err := e.s.EffectiveWakeupRule(ctx, ref, e.owner, created.RuleKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if caps := rule.AggregateCaps; len(caps) != 1 || caps[0].Scope != WakeupScopeProject || caps[0].Limit != 12 {
		t.Fatalf("effective caps = %+v", rule.AggregateCaps)
	}
	preview, err := e.s.EffectiveWakeupRule(ctx, ref, e.owner, "", &WakeupDefinitionWrite{Patch: patchOf(t, fields)})
	if err != nil || len(preview.AggregateCaps) != 1 || preview.AggregateCaps[0].Limit != 12 {
		t.Fatalf("a preview of a new root must show the default cap: %+v err=%v", preview.AggregateCaps, err)
	}
	explicit, err := e.s.SaveWakeupDefinition(ctx, ref, e.owner, WakeupDefinitionWrite{Patch: patchOf(t, fields+`,"aggregate_limit":5`)})
	if err != nil || explicit.Patch.AggregateLimit.Value != 5 {
		t.Fatalf("an explicit cap must win over the default: %+v err=%v", explicit.Patch.AggregateLimit, err)
	}
	// Updating an existing definition never inserts a default.
	e.defineAt(t, WakeupScopeProject, e.project, SystemRulePRMerged, `"instruction":"only text"`)
	row, err := e.f.q.GetWakeupDefinition(ctx, wakeupDefinitionParams(ref, SystemRulePRMerged))
	if err != nil {
		t.Fatal(err)
	}
	if patch, err := DecodeWakeupConfigPatch(row.Config); err != nil || patch.AggregateLimit.Set {
		t.Fatalf("a built-in instruction-only override gained a cap: %+v err=%v", patch.AggregateLimit, err)
	}
	issueRef := WakeupScopeRef{Kind: WakeupScopeIssue, ID: e.issue, WorkspaceID: ws}
	if _, err := e.s.SaveWakeupDefinition(ctx, issueRef, e.owner, WakeupDefinitionWrite{RuleKey: SystemRulePRMerged, Patch: patchOf(t, `"aggregate_limit":2`)}); err == nil {
		t.Fatal("an issue-scope aggregate cap would only duplicate rate_limit and must be refused")
	}
	if _, err := e.s.SaveWakeupDefinition(ctx, ref, e.owner, WakeupDefinitionWrite{RuleKey: SystemRulePRChecksFailed, Patch: patchOf(t, `"aggregate_limit":0`)}); err == nil {
		t.Fatal("a cap below 1 must be refused")
	}
}

// With no scoped definition at all (today's production) nothing is counted:
// legacy defaults stay uncapped across issues and no budget row is ever made.
func TestAggregateLegacyDefaultsStayUncappedAcrossIssues(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	issues := make([]pgtype.UUID, 14)
	for i := range issues {
		issues[i] = e.issueIn(t, e.project)
	}
	if got := e.fire(t, issues); sum(got) != 14 {
		t.Fatalf("legacy defaults were throttled across issues: %v", got)
	}
	if e.budgets(t) != 0 || e.f.Count(t, `SELECT count(*) FROM wakeup_aggregate_reservation WHERE workspace_id=$1`, e.f.WorkspaceID) != 0 {
		t.Fatal("the legacy path touched the aggregate tables")
	}
}

// The scheduler itself, not only a direct dispatch, starts a delayed instance
// once its retry time has passed and a slot is free, and never before.
func TestAggregateSchedulerStartsDelayedInstanceWhenSlotFrees(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	ctx := context.Background()
	ws := parseTestUUID(t, e.f.WorkspaceID)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"aggregate_limit":1`)
	first, second := e.issueIn(t, e.project), e.issueIn(t, e.project)
	e.mergedOn(t, first, 1)
	e.mergedOn(t, second, 2)
	if e.runs(t, second) != 0 {
		t.Fatal("the second start must be delayed")
	}
	e.cancelRuns(t, first)
	if err := e.s.TickWorkspaces(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if e.runs(t, second) != 0 {
		t.Fatal("the scheduler looked at a delayed instance before its retry time")
	}
	e.f.Exec(t, `UPDATE issue_wakeup SET aggregate_retry_at=now()-interval '1 second' WHERE issue_id=$1`, second)
	if err := e.s.TickWorkspaces(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if e.runs(t, second) != 1 || e.pendingOn(t, second) != 0 {
		t.Fatalf("the scheduler did not start the delayed instance once its slot freed (runs=%d pending=%d)", e.runs(t, second), e.pendingOn(t, second))
	}
}

// Dispatches racing for one counter never pass its cap; what a busy counter
// turned away stays pending and is admitted by later passes, up to the cap.
func TestAggregateConcurrentDispatchNeverExceedsTheCap(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	const limit, writers = 3, 10
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"aggregate_limit":3`)
	issues := make([]pgtype.UUID, writers)
	for i := range issues {
		issues[i] = e.issueIn(t, e.project)
	}
	var wg sync.WaitGroup
	for _, issue := range issues {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.s.TriggerPullRequestWakeup(context.Background(), issue, PullRequestWakeupInput{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget",
				Number: 1, URL: "https://github.com/acme/widget/pull/1", MergeCommit: "m", BaseBranch: "main", HeadBranch: "f"})
		}()
	}
	wg.Wait()
	total := func() (n int) {
		for _, issue := range issues {
			n += e.runs(t, issue)
		}
		return n
	}
	if total() > limit {
		t.Fatalf("%d starts admitted by racing dispatches, cap is %d", total(), limit)
	}
	for _, issue := range issues {
		if e.runs(t, issue) == 0 && e.pendingOn(t, issue) == 1 {
			e.redispatch(t, issue)
		}
	}
	if total() != limit {
		t.Fatalf("%d starts after the delayed instances were retried, want exactly %d", total(), limit)
	}
}

// afterFirstReservationRead runs a callback right after the first statement
// that reads the reservation table, standing in for another session that
// commits between two statements of one admission (READ COMMITTED).
type afterFirstReservationRead struct {
	db.DBTX
	after func()
	once  sync.Once
}

func (a *afterFirstReservationRead) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := a.DBTX.QueryRow(ctx, sql, args...)
	if strings.Contains(sql, "wakeup_aggregate_reservation") {
		a.once.Do(a.after)
	}
	return row
}

// A queued counted run cancelled while an admission is reading its counter is
// the normal way a slot is released; it must delay or admit the start, never
// fail it.
func TestAggregateReleaseBetweenReadsIsNotAnError(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	ctx := context.Background()
	ws := parseTestUUID(t, e.f.WorkspaceID)
	caps := []WakeupAggregateCap{{Scope: WakeupScopeProject, ScopeID: e.project, Limit: 2}}
	reserve := func(q *db.Queries, task pgtype.UUID) (aggregateAdmission, error) {
		return admitAggregateStart(ctx, q, aggregateStart{WorkspaceID: ws, RuleKey: SystemRulePRMerged, WakeupID: task, TaskID: task}, caps, time.Now())
	}
	counted := []pgtype.UUID{e.bareTask(t), e.bareTask(t)}
	for _, task := range counted {
		if adm, err := reserve(e.f.q, task); err != nil || !adm.Admitted {
			t.Fatalf("setup: admitted=%v err=%v", adm.Admitted, err)
		}
	}
	tx, err := e.s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	racing := &afterFirstReservationRead{DBTX: tx, after: func() {
		for _, task := range counted {
			e.f.Exec(t, `UPDATE agent_task_queue SET status='cancelled',completed_at=now() WHERE id=$1`, task)
		}
	}}
	adm, err := reserve(db.New(racing), e.bareTask(t))
	if err != nil {
		t.Fatalf("a slot released between the counter's reads failed the admission: %v", err)
	}
	if adm.Admitted || adm.RetryAt.IsZero() {
		t.Fatalf("the read that saw the counter full must delay the start: %+v", adm)
	}
}

// Slots a delayed fact is waiting for go to the oldest waiters first: a fresh
// event, which dispatches immediately and not through the scheduler's order,
// must not take a slot ahead of facts that were delayed earlier.
func TestAggregateFreedSlotGoesToOlderBlockedFactsFirst(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"aggregate_limit":1`)
	holder, waiting, fresh := e.issueIn(t, e.project), e.issueIn(t, e.project), e.issueIn(t, e.project)
	e.mergedOn(t, holder, 1)
	e.mergedOn(t, waiting, 2)
	if e.runs(t, holder) != 1 || e.runs(t, waiting) != 0 {
		t.Fatal("setup: the cap of 1 must delay the second start")
	}
	e.cancelRuns(t, holder)
	e.mergedOn(t, fresh, 3)
	if e.runs(t, fresh) != 0 || e.pendingOn(t, fresh) != 1 {
		t.Fatalf("a fresh event took the freed slot ahead of an older delayed fact (runs=%d)", e.runs(t, fresh))
	}
	e.redispatch(t, waiting)
	if e.runs(t, waiting) != 1 {
		t.Fatal("the oldest delayed fact did not get the freed slot")
	}
	// The fresh fact keeps its place in line behind it and starts once room returns.
	e.redispatch(t, fresh)
	if e.runs(t, fresh) != 0 {
		t.Fatal("the cap of 1 was passed")
	}
	e.cancelRuns(t, waiting)
	e.redispatch(t, fresh)
	if e.runs(t, fresh) != 1 {
		t.Fatal("the delayed fresh fact never started once its turn came")
	}
}

// Reservations older than the retention window are pruned a bounded batch per
// scheduler pass; recent ones, which still count, are kept.
func TestAggregateReservationRetentionIsBounded(t *testing.T) {
	e := newBuiltinEnv(t)
	e.cleanAggregate(t)
	ctx := context.Background()
	ws := parseTestUUID(t, e.f.WorkspaceID)
	extra := 3
	e.f.Exec(t, `INSERT INTO wakeup_aggregate_reservation(workspace_id,scope_kind,scope_id,rule_key,task_id,wakeup_id,reserved_at)
		SELECT $1,'project',$2,'pr_merged',gen_random_uuid(),gen_random_uuid(),now()-interval '25 hours' FROM generate_series(1,$3::int)`, ws, e.project, wakeupAggregatePruneBatch+extra)
	e.f.Exec(t, `INSERT INTO wakeup_aggregate_reservation(workspace_id,scope_kind,scope_id,rule_key,task_id,wakeup_id,reserved_at)
		SELECT $1,'project',$2,'pr_merged',gen_random_uuid(),gen_random_uuid(),now()-interval '30 minutes' FROM generate_series(1,2)`, ws, e.project)
	old := func() int {
		return e.f.Count(t, `SELECT count(*) FROM wakeup_aggregate_reservation WHERE workspace_id=$1 AND reserved_at < now()-interval '24 hours'`, e.f.WorkspaceID)
	}
	if err := e.s.TickWorkspaces(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if got := old(); got != extra {
		t.Fatalf("after one pass %d expired reservations remain, want %d (one bounded batch of %d pruned)", got, extra, wakeupAggregatePruneBatch)
	}
	if err := e.s.TickWorkspaces(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if got := old(); got != 0 {
		t.Fatalf("%d expired reservations survived two passes", got)
	}
	if n := e.f.Count(t, `SELECT count(*) FROM wakeup_aggregate_reservation WHERE workspace_id=$1`, e.f.WorkspaceID); n != 2 {
		t.Fatalf("%d reservations remain, want the 2 recent ones that still count", n)
	}
}

// An explicit null on a new custom root is the caller clearing the optional
// cap: it opts out of the 12/hour default instead of being overwritten by it.
func TestAggregateExplicitNullOnNewRootOptsOutOfTheDefault(t *testing.T) {
	e := newBuiltinEnv(t)
	ref := WakeupScopeRef{Kind: WakeupScopeProject, ID: e.project, WorkspaceID: parseTestUUID(t, e.f.WorkspaceID)}
	created, err := e.s.SaveWakeupDefinition(context.Background(), ref, e.owner, WakeupDefinitionWrite{
		Patch: patchOf(t, `"enabled":true,"trigger":{"kind":"pr_merged"},"instruction":"uncapped root","aggregate_limit":null`)})
	if err != nil {
		t.Fatal(err)
	}
	if p := created.Patch.AggregateLimit; !p.Set || !p.Null {
		t.Fatalf("an explicit null was replaced by %+v", p)
	}
	rule, err := e.s.EffectiveWakeupRule(context.Background(), ref, e.owner, created.RuleKey, nil)
	if err != nil || len(rule.AggregateCaps) != 0 {
		t.Fatalf("an opted-out root resolved caps %+v (err %v)", rule.AggregateCaps, err)
	}
}
