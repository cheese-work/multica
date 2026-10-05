package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Built-in execution (CHE-1082 L5): the three platform rules run the
// configuration their scoped definitions resolve to.

type builtinEnv struct {
	f       principalFixture
	s       *IssueWakeupService
	issue   pgtype.UUID
	agent   string
	project pgtype.UUID
	owner   pgtype.UUID
}

func newBuiltinEnv(t *testing.T) builtinEnv {
	t.Helper()
	f, s, issue, agent := conditionFixture(t)
	assignPRWakeupIssue(t, f, issue, agent)
	project := f.Project(t, "builtin exec project")
	f.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, issue, project)
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
	return builtinEnv{f: f, s: s, issue: issue, agent: agent, project: parseTestUUID(t, project), owner: parseTestUUID(t, f.UserID)}
}

func patchOf(t *testing.T, fields string) WakeupConfigPatch {
	t.Helper()
	p, err := DecodeWakeupConfigPatch([]byte(`{"v":1,` + fields + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// define writes (or replaces) a definition through the service, as an owner who
// may invoke every agent of the fixture.
func (e builtinEnv) define(t *testing.T, scope WakeupScope, key, fields string) WakeupDefinitionView {
	t.Helper()
	id := map[WakeupScope]pgtype.UUID{
		WakeupScopeWorkspace: parseTestUUID(t, e.f.WorkspaceID), WakeupScopeProject: e.project, WakeupScopeIssue: e.issue,
	}[scope]
	ref := WakeupScopeRef{Kind: scope, ID: id, WorkspaceID: parseTestUUID(t, e.f.WorkspaceID)}
	var revision int64
	if row, err := e.f.q.GetWakeupDefinition(context.Background(), wakeupDefinitionParams(ref, key)); err == nil {
		revision = row.Revision
	}
	view, err := e.s.SaveWakeupDefinition(context.Background(), ref, e.owner, WakeupDefinitionWrite{RuleKey: key, Revision: revision, Patch: patchOf(t, fields)})
	if err != nil {
		t.Fatalf("define %s/%s %s: %v", scope, key, fields, err)
	}
	return view
}

func (e builtinEnv) mergedPR(t *testing.T, number int32, mutate ...func(*PullRequestWakeupInput)) {
	t.Helper()
	in := PullRequestWakeupInput{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: number,
		URL: fmt.Sprintf("https://github.com/acme/widget/pull/%d", number), MergeCommit: fmt.Sprintf("merge-%d", number), BaseBranch: "main", HeadBranch: "feature"}
	for _, m := range mutate {
		m(&in)
	}
	if err := e.s.TriggerPullRequestWakeup(context.Background(), e.issue, in); err != nil {
		t.Fatal(err)
	}
}

func (e builtinEnv) failedChecks(t *testing.T, head, conclusion string) {
	t.Helper()
	err := e.s.TriggerPullRequestWakeup(context.Background(), e.issue, PullRequestWakeupInput{Rule: SystemRulePRChecksFailed,
		RepoOwner: "acme", RepoName: "widget", Number: 300, URL: "https://github.com/acme/widget/pull/300", HeadSHA: head, Conclusion: conclusion})
	if err != nil {
		t.Fatal(err)
	}
}

func (e builtinEnv) rule(t *testing.T, key string) db.IssueWakeup {
	t.Helper()
	w, err := e.f.q.GetSystemWakeup(context.Background(), db.GetSystemWakeupParams{IssueID: e.issue, SystemRule: systemRuleText(key)})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (e builtinEnv) tasks(t *testing.T, key string) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, e.issue, key)
}

func (e builtinEnv) lastTask(t *testing.T, key string) db.AgentTaskQueue {
	t.Helper()
	var id pgtype.UUID
	if err := e.f.Pool.QueryRow(context.Background(), `SELECT id FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2 ORDER BY created_at DESC,id DESC LIMIT 1`, e.issue, key).Scan(&id); err != nil {
		t.Fatalf("no %s task: %v", key, err)
	}
	task, err := e.f.q.GetAgentTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (e builtinEnv) finishTasks(t *testing.T) {
	t.Helper()
	e.f.Exec(t, `UPDATE agent_task_queue SET status='completed',started_at=COALESCE(started_at,clock_timestamp()),completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, e.issue)
}

func (e builtinEnv) pending(t *testing.T, key string) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule=$2 AND r.processed_at IS NULL`, e.issue, key)
}

func (e builtinEnv) outcomes(t *testing.T, key string) []string {
	t.Helper()
	rows, err := e.f.Pool.Query(context.Background(), `SELECT COALESCE(details->>'outcome','') FROM activity_log WHERE issue_id=$1 AND details->>'rule'=$2 ORDER BY created_at,id`, e.issue, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

func (e builtinEnv) loadBuiltin(t *testing.T, key string) *builtinWakeup {
	t.Helper()
	issue, err := e.f.q.GetIssue(context.Background(), e.issue)
	if err != nil {
		t.Fatal(err)
	}
	var legacy *db.IssueWakeup
	if w, err := e.f.q.GetSystemWakeup(context.Background(), db.GetSystemWakeupParams{IssueID: e.issue, SystemRule: systemRuleText(key)}); err == nil {
		legacy = &w
	}
	b, err := loadBuiltinWakeup(context.Background(), e.f.q, issue, key, legacy)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An instruction-only override changes the text and nothing else: the platform
// keeps appending the PR facts, and no mode, expiry, fire cap or throttle
// appears that the override did not set.
func TestBuiltinInstructionOnlyOverrideInsertsNoDefaults(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"Project way"`)
	b := e.loadBuiltin(t, SystemRulePRMerged)
	c := b.Eff.Config
	if c.Mode.Set || c.MaxFires.Set || c.Expiry.Set || c.RateLimit.Set || c.AggregateLimit.Set || len(b.Eff.AggregateCaps) != 0 {
		t.Fatalf("an instruction-only override resolved extra controls: %+v", c)
	}
	for number := int32(201); number <= 205; number++ {
		e.mergedPR(t, number)
		e.finishTasks(t)
	}
	task := e.lastTask(t, SystemRulePRMerged)
	note := task.HandoffNote.String
	if !strings.Contains(note, "Project way") || strings.Contains(note, "A linked pull request has merged") {
		t.Fatalf("the run does not carry the project instruction: %q", note)
	}
	if !strings.Contains(note, `"pr_number":205`) || !strings.Contains(note, "merge-205") {
		t.Fatalf("the platform's PR facts are no longer appended: %q", note)
	}
	w := e.rule(t, SystemRulePRMerged)
	if e.tasks(t, SystemRulePRMerged) != 5 || w.FireCount != 5 || w.PausedReason.Valid || w.MaxFires.Valid {
		t.Fatalf("five merges ran %d times, fire_count=%d, pause=%q, max_fires=%v; want 5 uncapped runs", e.tasks(t, SystemRulePRMerged), w.FireCount, w.PausedReason.String, w.MaxFires)
	}
}

// With no stored definition the legacy path runs untouched: workspace settings
// decide, and no configuration is resolved or fingerprinted.
func TestBuiltinLegacyDefaultsUnchangedWithoutDefinitions(t *testing.T) {
	e := newBuiltinEnv(t)
	e.mergedPR(t, 210)
	w := e.rule(t, SystemRulePRMerged)
	if got := e.lastTask(t, SystemRulePRMerged).HandoffNote.String; !strings.Contains(got, "A linked pull request has merged") {
		t.Fatalf("legacy PR text changed: %q", got)
	}
	if w.ConfigFingerprint.Valid {
		t.Fatalf("legacy instance was fingerprinted: %q", w.ConfigFingerprint.String)
	}
	if e.loadBuiltin(t, SystemRulePRMerged) != nil {
		t.Fatal("a configuration was resolved without any stored definition")
	}
	e.finishTasks(t)
	e.f.Exec(t, `UPDATE workspace SET settings='{"github_wake_on_pr_merge":false}'::jsonb WHERE id=$1`, e.f.WorkspaceID)
	e.mergedPR(t, 211)
	if e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatalf("the workspace opt-out no longer holds: %d runs", e.tasks(t, SystemRulePRMerged))
	}
}

// A project override that re-enables a rule the workspace turned off runs, and
// one that disables a rule the workspace left on does not; the GitHub master
// switch stays a veto at every scope.
func TestBuiltinProjectOverrideEnablesAndDisables(t *testing.T) {
	e := newBuiltinEnv(t)
	e.f.Exec(t, `UPDATE workspace SET settings='{"github_wake_on_pr_merge":false}'::jsonb WHERE id=$1`, e.f.WorkspaceID)
	e.mergedPR(t, 220)
	if e.tasks(t, SystemRulePRMerged) != 0 {
		t.Fatal("workspace opt-out ran")
	}
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"enabled":true`)
	e.mergedPR(t, 221)
	if e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatalf("the project re-enable ran %d times, want 1", e.tasks(t, SystemRulePRMerged))
	}
	e.finishTasks(t)
	e.f.Exec(t, `UPDATE workspace SET settings='{"github_enabled":false,"github_wake_on_pr_merge":false}'::jsonb WHERE id=$1`, e.f.WorkspaceID)
	e.mergedPR(t, 222)
	if e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatal("the GitHub master switch did not veto a project re-enable")
	}
	e.f.Exec(t, `UPDATE workspace SET settings='{}'::jsonb WHERE id=$1`, e.f.WorkspaceID)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"enabled":false`)
	e.mergedPR(t, 223)
	if e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatal("a project disable still ran")
	}
}

// Moving the issue to a project without the override reverts to the inherited
// configuration, and a run queued under the old project can no longer be
// claimed or started.
func TestBuiltinProjectMoveInvalidatesUnstartedRunsAndRevertsConfig(t *testing.T) {
	e := newBuiltinEnv(t)
	ctx := context.Background()
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"Project way"`)
	e.mergedPR(t, 230)
	first := e.lastTask(t, SystemRulePRMerged)
	before := e.rule(t, SystemRulePRMerged)
	if !before.ConfigFingerprint.Valid {
		t.Fatal("the instance did not record the configuration it captured under")
	}
	if err := e.s.CheckClaim(ctx, first); err != nil {
		t.Fatalf("claim before the move: %v", err)
	}
	other := e.f.Project(t, "other project")
	e.f.Exec(t, `UPDATE issue SET project_id=$2 WHERE id=$1`, e.issue, other)
	if err := e.s.CheckClaim(ctx, first); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("claim after the move = %v, want ErrWakeupForbidden", err)
	}
	if err := e.s.CheckStart(ctx, first); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("start after the move = %v, want ErrWakeupForbidden", err)
	}
	e.mergedPR(t, 231)
	after := e.rule(t, SystemRulePRMerged)
	if after.ID != before.ID || after.FireCount != before.FireCount+1 || after.Revision == before.Revision || after.ConfigFingerprint.Valid {
		t.Fatalf("instance identity/counters after the move: before %+v after %+v", before, after)
	}
	cancelled := e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='cancelled'`, first.ID)
	if cancelled != 1 {
		t.Fatalf("the unstarted run captured under the old project was not withdrawn")
	}
	if note := e.lastTask(t, SystemRulePRMerged).HandoffNote.String; strings.Contains(note, "Project way") || !strings.Contains(note, "A linked pull request has merged") {
		t.Fatalf("the moved issue still runs the old project's instruction: %q", note)
	}
}

// Editing a definition invalidates what has not started; a run that is already
// running keeps the prompt it has.
func TestBuiltinConfigEditInvalidatesUnstartedButNotRunning(t *testing.T) {
	e := newBuiltinEnv(t)
	ctx := context.Background()
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"One"`)
	e.mergedPR(t, 240)
	running := e.lastTask(t, SystemRulePRMerged)
	wakeStart(t, e.f, util.UUIDToString(running.ID))
	running, _ = e.f.q.GetAgentTask(ctx, running.ID)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"Two"`)
	if err := e.s.CheckStart(ctx, running); err != nil {
		t.Fatalf("a running prompt must stay valid: %v", err)
	}
	e.finishTasks(t)

	e.mergedPR(t, 241)
	queued := e.lastTask(t, SystemRulePRMerged)
	if !strings.Contains(queued.HandoffNote.String, "Two") {
		t.Fatalf("the capture does not use the edited instruction: %q", queued.HandoffNote.String)
	}
	if err := e.s.CheckClaim(ctx, queued); err != nil {
		t.Fatalf("claim before the next edit: %v", err)
	}
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"Three"`)
	if err := e.s.CheckClaim(ctx, queued); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("claim of a run captured under the old text = %v, want ErrWakeupForbidden", err)
	}
	if err := e.s.CheckStart(ctx, queued); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("start of a run captured under the old text = %v, want ErrWakeupForbidden", err)
	}
	e.mergedPR(t, 242)
	next := e.lastTask(t, SystemRulePRMerged)
	if next.ID == queued.ID || !strings.Contains(next.HandoffNote.String, "Three") {
		t.Fatalf("the next capture does not use the edited instruction: %q", next.HandoffNote.String)
	}
	if e.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='cancelled'`, queued.ID) != 1 {
		t.Fatal("the stale unstarted run was not withdrawn")
	}
}

// A waiting run that claims after a definition edit does not take the stale
// capture along; the rule keeps its instance and counters.
func TestBuiltinConfigEditStopsAJoinOfStaleInputs(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"One"`)
	waiting := wakeWaitingRun(t, e.f, e.issue, e.agent, e.f.UserID)
	e.mergedPR(t, 250)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"Two"`)
	if notes := wakeClaim(t, e.f, e.s, waiting); strings.Contains(notes, "One") {
		t.Fatalf("a stale capture joined the claimed run: %q", notes)
	}
	// Without an edit the same capture joins, carrying the project instruction.
	e2 := newBuiltinEnv(t)
	e2.define(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"One"`)
	waiting2 := wakeWaitingRun(t, e2.f, e2.issue, e2.agent, e2.f.UserID)
	e2.mergedPR(t, 251)
	if notes := wakeClaim(t, e2.f, e2.s, waiting2); !strings.Contains(notes, "One") {
		t.Fatalf("the project instruction did not reach the joined run: %q", notes)
	}
}

// A named agent target runs on that agent; the member who wrote the target is
// asked again at enqueue, join and claim.
func TestBuiltinNamedAgentTargetAuthorizationFailsClosedVisibly(t *testing.T) {
	e := newBuiltinEnv(t)
	// Created before the agent, so cleanup removes the agent first.
	newOwner := e.f.member(t, "new-owner")
	named := e.f.privateAgentOwnedBy(t, e.f.UserID, "named")
	e.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", e.issue)
	e.define(t, WakeupScopeWorkspace, SystemRulePRMerged, fmt.Sprintf(`"target":{"type":"agent","id":%q}`, named))
	e.mergedPR(t, 260)
	task := e.lastTask(t, SystemRulePRMerged)
	if util.UUIDToString(task.AgentID) != named {
		t.Fatalf("run on agent %s, want the named %s", util.UUIDToString(task.AgentID), named)
	}
	if err := e.s.CheckClaim(context.Background(), task); err != nil {
		t.Fatalf("claim of an authorized target: %v", err)
	}
	e.finishTasks(t)

	// The writer loses access to the named agent.
	e.f.Exec(t, `UPDATE agent SET owner_id=$2 WHERE id=$1`, named, newOwner)
	if err := e.s.CheckClaim(context.Background(), task); !errors.Is(err, ErrWakeupForbidden) {
		t.Fatalf("claim after revocation = %v, want ErrWakeupForbidden", err)
	}
	e.mergedPR(t, 261)
	if e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatalf("a revoked target still ran: %d runs", e.tasks(t, SystemRulePRMerged))
	}
	if got := e.outcomes(t, SystemRulePRMerged); len(got) == 0 || got[len(got)-1] != "target_unauthorized" {
		t.Fatalf("the refusal is not visible on the timeline: %v", got)
	}
	if e.pending(t, SystemRulePRMerged) != 0 {
		t.Fatal("the refused input stayed pending")
	}
	// An archived target fails the same way.
	e.f.Exec(t, `UPDATE agent SET owner_id=$2,archived_at=now() WHERE id=$1`, named, e.f.UserID)
	e.mergedPR(t, 262)
	if e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatal("an archived target still ran")
	}
}

func TestBuiltinRevokedTargetDoesNotJoinARun(t *testing.T) {
	e := newBuiltinEnv(t)
	newOwner := e.f.member(t, "join-new-owner")
	named := e.f.privateAgentOwnedBy(t, e.f.UserID, "named-join")
	invocableByWorkspace(t, e.f, named)
	e.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", e.issue)
	e.define(t, WakeupScopeWorkspace, SystemRulePRMerged, fmt.Sprintf(`"target":{"type":"agent","id":%q}`, named))
	waiting := wakeWaitingRun(t, e.f, e.issue, named, e.f.UserID)
	// The named agent already has a waiting run, so the capture joins it.
	e.mergedPR(t, 265)
	e.f.Exec(t, `UPDATE agent SET permission_mode='private',owner_id=$2 WHERE id=$1`, named, newOwner)
	if notes := wakeClaim(t, e.f, e.s, waiting); notes != "" {
		t.Fatalf("a revoked target joined the claimed run: %q", notes)
	}
}

// A named squad runs its leader as a leader task with the squad's identity.
func TestBuiltinNamedSquadTargetBriefsTheLeader(t *testing.T) {
	e := newBuiltinEnv(t)
	leader := e.f.privateAgentOwnedBy(t, e.f.UserID, "leader")
	squad := e.f.Squad(t, "builtin squad", leader)
	e.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", e.issue)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, fmt.Sprintf(`"target":{"type":"squad","id":%q}`, squad))
	e.mergedPR(t, 270)
	task := e.lastTask(t, SystemRulePRMerged)
	if util.UUIDToString(task.AgentID) != leader || !task.IsLeaderTask || util.UUIDToString(task.SquadID) != squad {
		t.Fatalf("squad run = agent %s leader=%v squad=%s; want leader %s of squad %s", util.UUIDToString(task.AgentID), task.IsLeaderTask, util.UUIDToString(task.SquadID), leader, squad)
	}
	// A dynamic target on the same rule keeps resolving the assignee.
	e.finishTasks(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"target":{"type":"assignee"}`)
	e.mergedPR(t, 271)
	if got := e.lastTask(t, SystemRulePRMerged); util.UUIDToString(got.AgentID) != e.agent || got.IsLeaderTask {
		t.Fatalf("assignee target ran agent %s leader=%v, want %s", util.UUIDToString(got.AgentID), got.IsLeaderTask, e.agent)
	}
}

func TestBuiltinFiltersDecideEligibility(t *testing.T) {
	t.Run("branches match exactly and missing facts do not match", func(t *testing.T) {
		e := newBuiltinEnv(t)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"filters":{"base_branch":"main","head_branch":"feature"}`)
		e.mergedPR(t, 280, func(in *PullRequestWakeupInput) { in.BaseBranch = "develop" })
		e.mergedPR(t, 281, func(in *PullRequestWakeupInput) { in.HeadBranch = "Feature" })
		e.mergedPR(t, 282, func(in *PullRequestWakeupInput) { in.BaseBranch = "" })
		if e.tasks(t, SystemRulePRMerged) != 0 {
			t.Fatalf("non-matching PRs ran %d times", e.tasks(t, SystemRulePRMerged))
		}
		e.mergedPR(t, 283)
		if e.tasks(t, SystemRulePRMerged) != 1 {
			t.Fatalf("the matching PR ran %d times, want 1", e.tasks(t, SystemRulePRMerged))
		}
		// The ledger keeps a filtered event's identity: redelivery never fires.
		e.finishTasks(t)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"filters":null`)
		e.mergedPR(t, 280, func(in *PullRequestWakeupInput) { in.BaseBranch = "develop" })
		if e.tasks(t, SystemRulePRMerged) != 1 {
			t.Fatal("a filtered event fired when it was redelivered after the filter was removed")
		}
	})
	t.Run("ci conclusion", func(t *testing.T) {
		e := newBuiltinEnv(t)
		e.define(t, WakeupScopeWorkspace, SystemRulePRChecksFailed, `"filters":{"ci":"failure"}`)
		e.failedChecks(t, "head-a", "ERROR")
		if e.tasks(t, SystemRulePRChecksFailed) != 0 {
			t.Fatal("an ERROR conclusion ran a failure-only rule")
		}
		e.failedChecks(t, "head-b", "FAILURE")
		if e.tasks(t, SystemRulePRChecksFailed) != 1 {
			t.Fatalf("a FAILURE conclusion ran %d times, want 1", e.tasks(t, SystemRulePRChecksFailed))
		}
	})
	t.Run("labels need all and priorities need any", func(t *testing.T) {
		e := newBuiltinEnv(t)
		l1 := e.f.Insert(t, "issue_label", testutil.Cols{"workspace_id": e.f.WorkspaceID, "name": "wk-one", "color": "#111111"})
		l2 := e.f.Insert(t, "issue_label", testutil.Cols{"workspace_id": e.f.WorkspaceID, "name": "wk-two", "color": "#222222"})
		e.f.Exec(t, `INSERT INTO issue_to_label(issue_id,label_id) VALUES($1,$2)`, e.issue, l1)
		e.f.Cleanup(t, `DELETE FROM issue_to_label WHERE issue_id=$1`, e.issue)
		e.f.Exec(t, `UPDATE issue SET priority='high' WHERE id=$1`, e.issue)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, fmt.Sprintf(`"filters":{"labels":[%q,%q]}`, l1, l2))
		e.mergedPR(t, 290)
		if e.tasks(t, SystemRulePRMerged) != 0 {
			t.Fatal("an issue missing one required label ran")
		}
		e.f.Exec(t, `INSERT INTO issue_to_label(issue_id,label_id) VALUES($1,$2)`, e.issue, l2)
		e.mergedPR(t, 291)
		if e.tasks(t, SystemRulePRMerged) != 1 {
			t.Fatalf("an issue with every label ran %d times, want 1", e.tasks(t, SystemRulePRMerged))
		}
		e.finishTasks(t)
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"filters":{"priorities":["urgent","low"]}`)
		e.mergedPR(t, 292)
		if e.tasks(t, SystemRulePRMerged) != 1 {
			t.Fatal("an issue outside the selected priorities ran")
		}
		e.define(t, WakeupScopeProject, SystemRulePRMerged, `"filters":{"priorities":["urgent","high"]}`)
		e.mergedPR(t, 293)
		if e.tasks(t, SystemRulePRMerged) != 2 {
			t.Fatalf("an issue in the selected priorities ran %d times total, want 2", e.tasks(t, SystemRulePRMerged))
		}
	})
}

// Once and max_fires end an instance for good: an ancestor edit never rearms it
// and never clears a safety pause.
func TestBuiltinModeAndMaxFiresEndTheInstanceAndStayEnded(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"mode":"once"`)
	e.mergedPR(t, 300)
	e.finishTasks(t)
	e.mergedPR(t, 301)
	w := e.rule(t, SystemRulePRMerged)
	if e.tasks(t, SystemRulePRMerged) != 1 || !w.PausedReason.Valid || w.PausedReason.String != wakeupPausedMaxFires {
		t.Fatalf("once rule: %d runs, pause %q; want one run then an ended instance", e.tasks(t, SystemRulePRMerged), w.PausedReason.String)
	}
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"mode":"continuous"`)
	e.mergedPR(t, 302)
	if e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatal("an ancestor edit rearmed a consumed instance")
	}
	if e.pending(t, SystemRulePRMerged) != 0 {
		t.Fatal("inputs of an ended instance stayed pending")
	}

	e2 := newBuiltinEnv(t)
	e2.define(t, WakeupScopeProject, SystemRulePRMerged, `"max_fires":2`)
	for number := int32(310); number < 314; number++ {
		e2.mergedPR(t, number)
		e2.finishTasks(t)
	}
	if got := e2.tasks(t, SystemRulePRMerged); got != 2 {
		t.Fatalf("max_fires 2 ran %d times", got)
	}
	e2.define(t, WakeupScopeProject, SystemRulePRMerged, `"max_fires":10`)
	e2.mergedPR(t, 320)
	if got := e2.tasks(t, SystemRulePRMerged); got != 2 {
		t.Fatalf("raising the cap rearmed a consumed instance: %d runs", got)
	}

	// A loop pause is a safety pause: editing never lifts it.
	e3 := newBuiltinEnv(t)
	e3.mergedPR(t, 330)
	e3.finishTasks(t)
	w3 := e3.rule(t, SystemRulePRMerged)
	e3.f.Exec(t, `UPDATE issue_wakeup SET enabled=false,paused_reason='loop',disabled_at=now() WHERE id=$1`, w3.ID)
	e3.define(t, WakeupScopeProject, SystemRulePRMerged, `"enabled":true,"instruction":"edit"`)
	e3.mergedPR(t, 331)
	if got := e3.tasks(t, SystemRulePRMerged); got != 1 {
		t.Fatalf("a definition edit cleared a loop pause: %d runs", got)
	}
	if after := e3.rule(t, SystemRulePRMerged); after.ID != w3.ID || !after.PausedReason.Valid || after.PausedReason.String != wakeupPausedLoop {
		t.Fatalf("pause after the edit = %+v", after)
	}
}

func TestBuiltinExpiryEndsNewInputs(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRMerged, `"expiry":{"after_seconds":3600}`)
	e.mergedPR(t, 340)
	if e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatal("the first input within the window did not run")
	}
	e.finishTasks(t)
	e.f.Exec(t, `UPDATE issue_wakeup SET created_at=now()-interval '2 hours' WHERE issue_id=$1 AND system_rule=$2`, e.issue, SystemRulePRMerged)
	e.mergedPR(t, 341)
	if e.tasks(t, SystemRulePRMerged) != 1 {
		t.Fatal("a relative expiry measured from the instance's first activation did not end the rule")
	}
	if e.pending(t, SystemRulePRMerged) != 0 {
		t.Fatal("expired input stayed pending")
	}

	// An absolute end time in the past ends the rule, including a new instance.
	e2 := newBuiltinEnv(t)
	e2.define(t, WakeupScopeProject, SystemRulePRMerged, `"expiry":{"at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	e2.f.Exec(t, `UPDATE issue_wakeup_definition SET config=jsonb_set(config,'{expiry,at}',to_jsonb($4::text)),revision=revision+1 WHERE workspace_id=$1 AND scope_kind='project' AND scope_id=$2 AND rule_key=$3`,
		e2.f.WorkspaceID, e2.project, SystemRulePRMerged, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	e2.mergedPR(t, 342)
	if e2.tasks(t, SystemRulePRMerged) != 0 {
		t.Fatal("an elapsed absolute expiry still created a run")
	}
	if e2.f.Count(t, `SELECT count(*) FROM issue_wakeup WHERE issue_id=$1 AND system_rule=$2`, e2.issue, SystemRulePRMerged) != 0 {
		t.Fatal("an elapsed scope-level expiry created a new instance")
	}
}

func TestBuiltinRateLimitLowersTheHourlyCap(t *testing.T) {
	e := newBuiltinEnv(t)
	e.define(t, WakeupScopeProject, SystemRulePRChecksFailed, `"rate_limit":2`)
	for i := range 3 {
		e.failedChecks(t, fmt.Sprintf("rate-head-%d", i), "FAILURE")
		e.finishTasks(t)
	}
	w := e.rule(t, SystemRulePRChecksFailed)
	if e.tasks(t, SystemRulePRChecksFailed) != 2 || w.Enabled || w.PausedReason.String != wakeupPausedRate {
		t.Fatalf("rate_limit 2: %d runs, enabled=%v pause=%q; want 2 runs then a rate pause", e.tasks(t, SystemRulePRChecksFailed), w.Enabled, w.PausedReason.String)
	}
}

// A stored definition this build cannot execute holds the rule: nothing runs,
// the captured fact waits, and the definition is left as stored.
func TestBuiltinUnexecutableDefinitionsHoldTheRule(t *testing.T) {
	for _, tc := range []struct{ name, fields string }{
		{"unknown active run", `"active_run":"queue"`},
		{"schedule", `"schedule":{"cron":"* * * * *"}`},
		{"other trigger kind", `"trigger":{"kind":"cron"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newBuiltinEnv(t)
			if err := insertDefinition(t, e.f, "project", util.UUIDToString(e.project), SystemRulePRMerged, false, `{"v":1,`+tc.fields+`}`); err != nil {
				t.Fatal(err)
			}
			e.mergedPR(t, 350)
			if e.tasks(t, SystemRulePRMerged) != 0 || e.pending(t, SystemRulePRMerged) != 1 {
				t.Fatalf("held rule: %d runs, %d pending; want 0 and 1", e.tasks(t, SystemRulePRMerged), e.pending(t, SystemRulePRMerged))
			}
			e.f.Exec(t, `DELETE FROM issue_wakeup_definition WHERE workspace_id=$1`, e.f.WorkspaceID)
			if err := e.s.dispatchSystem(context.Background(), e.rule(t, SystemRulePRMerged)); err != nil {
				t.Fatal(err)
			}
			if e.tasks(t, SystemRulePRMerged) != 1 {
				t.Fatal("the held fact did not run once the definition was gone")
			}
		})
	}
}

func TestBuiltinExecutionClassifiesEveryPatchField(t *testing.T) {
	executed := map[string]bool{}
	for _, f := range builtinExecutedFields {
		executed[f] = true
	}
	for _, f := range builtinHeldFields {
		if executed[f] {
			t.Fatalf("field %q is both executed and held", f)
		}
		executed[f] = true
	}
	for _, f := range setWakeupFields(allFieldsPatch(t)) {
		if !executed[f] {
			t.Fatalf("patch field %q is neither executed nor held by built-in execution; classify it", f)
		}
		delete(executed, f)
	}
	if len(executed) != 0 {
		t.Fatalf("classified fields that are not patch fields: %v", executed)
	}
}

func allFieldsPatch(t *testing.T) WakeupConfigPatch {
	t.Helper()
	return patchOf(t, `"enabled":true,"name":"n","trigger":{},"target":{},"instruction":"x","mode":"once","max_fires":1,"expiry":{},"schedule":{},"rate_limit":1,"aggregate_limit":1,"filters":{},"active_run":"defer"`)
}

// child_done: a project instruction runs, a project disable keeps it quiet, and
// a project re-enable of a workspace-disabled rule does not replay a stage that
// closed while it was off.
func TestBuiltinChildDoneUsesScopedConfiguration(t *testing.T) {
	setup := func(t *testing.T) (builtinEnv, string) {
		e := newBuiltinEnv(t)
		e.f.Cleanup(t, "DELETE FROM issue_child_event WHERE parent_id=$1", e.issue)
		child := e.f.Issue(t, "child", testutil.Cols{"parent_issue_id": e.issue, "status": "in_progress"})
		if err := e.s.ProcessChildEvents(context.Background(), e.issue); err != nil {
			t.Fatal(err)
		}
		return e, child
	}
	closeChild := func(t *testing.T, e builtinEnv, child string) {
		t.Helper()
		e.f.Exec(t, "UPDATE issue SET status='done' WHERE id=$1", child)
		if err := e.s.ProcessChildEvents(context.Background(), e.issue); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("instruction", func(t *testing.T) {
		e, child := setup(t)
		e.define(t, WakeupScopeProject, SystemRuleChildDone, `"instruction":"Close out the project"`)
		closeChild(t, e, child)
		if e.tasks(t, SystemRuleChildDone) != 1 || !strings.Contains(e.lastTask(t, SystemRuleChildDone).HandoffNote.String, "Close out the project") {
			t.Fatalf("project instruction missing from the child_done run (%d runs)", e.tasks(t, SystemRuleChildDone))
		}
	})
	t.Run("disabled for the project", func(t *testing.T) {
		e, child := setup(t)
		e.define(t, WakeupScopeProject, SystemRuleChildDone, `"enabled":false`)
		closeChild(t, e, child)
		if e.tasks(t, SystemRuleChildDone) != 0 {
			t.Fatal("a project disable still woke the assignee")
		}
	})
	t.Run("re-enabled over a workspace opt-out", func(t *testing.T) {
		e, child := setup(t)
		off := false
		if _, err := e.s.SetChildDoneDefault(context.Background(), parseTestUUID(t, e.f.WorkspaceID), &off, nil); err != nil {
			t.Fatal(err)
		}
		closeChild(t, e, child)
		if e.tasks(t, SystemRuleChildDone) != 0 {
			t.Fatal("workspace opt-out ran")
		}
		e.define(t, WakeupScopeProject, SystemRuleChildDone, `"enabled":true`)
		if err := e.s.ProcessChildEvents(context.Background(), e.issue); err != nil {
			t.Fatal(err)
		}
		if e.tasks(t, SystemRuleChildDone) != 0 {
			t.Fatal("re-enabling replayed a stage that closed while the rule was off")
		}
		// A change that happens after re-enabling does fire.
		second := e.f.Issue(t, "second child", testutil.Cols{"parent_issue_id": e.issue, "status": "in_progress"})
		if err := e.s.ProcessChildEvents(context.Background(), e.issue); err != nil {
			t.Fatal(err)
		}
		closeChild(t, e, second)
		if e.tasks(t, SystemRuleChildDone) != 1 {
			t.Fatalf("a stage closing after the re-enable ran %d times, want 1", e.tasks(t, SystemRuleChildDone))
		}
	})
	t.Run("squad target", func(t *testing.T) {
		e, child := setup(t)
		leader := e.f.privateAgentOwnedBy(t, e.f.UserID, "cd-leader")
		squad := e.f.Squad(t, "child done squad", leader)
		e.f.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id=$1", e.issue)
		e.define(t, WakeupScopeWorkspace, SystemRuleChildDone, fmt.Sprintf(`"target":{"type":"squad","id":%q}`, squad))
		closeChild(t, e, child)
		task := e.lastTask(t, SystemRuleChildDone)
		if util.UUIDToString(task.AgentID) != leader || !task.IsLeaderTask || util.UUIDToString(task.SquadID) != squad {
			t.Fatalf("child_done squad run = agent %s leader=%v squad %s", util.UUIDToString(task.AgentID), task.IsLeaderTask, util.UUIDToString(task.SquadID))
		}
	})
}
