package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Custom event and condition definitions (CHE-1082 L10): validation, the
// activation sweep, per-issue instances and their execution. Fixtures write
// through the service, as an owner who may invoke the fixture's private agent.

type customEnv struct {
	drainKit
}

func newCustomEnv(t *testing.T) customEnv {
	t.Helper()
	k := newDrainKit(t)
	k.f.Cleanup(t, "DELETE FROM activity_log WHERE issue_id=$1", k.issue)
	return customEnv{k}
}

func (k customEnv) scopeRef(t *testing.T, scope WakeupScope) WakeupScopeRef {
	t.Helper()
	ws := parseTestUUID(t, k.f.WorkspaceID)
	id := map[WakeupScope]pgtype.UUID{WakeupScopeWorkspace: ws, WakeupScopeProject: parseTestUUID(t, k.project), WakeupScopeIssue: parseTestUUID(t, k.issue)}[scope]
	return WakeupScopeRef{Kind: scope, ID: id, WorkspaceID: ws}
}

// save writes one definition through the service. A custom root is created with
// an empty key; any other write reads the stored revision first.
func (k customEnv) save(scope WakeupScope, key, fields string, t *testing.T) (WakeupDefinitionView, error) {
	t.Helper()
	ref := k.scopeRef(t, scope)
	var revision int64
	if key != "" {
		if row, err := k.f.q.GetWakeupDefinition(context.Background(), wakeupDefinitionParams(ref, key)); err == nil {
			revision = row.Revision
		}
	}
	return k.s.SaveWakeupDefinition(context.Background(), ref, parseTestUUID(t, k.owner), WakeupDefinitionWrite{RuleKey: key, Revision: revision, Patch: patchOf(t, fields)})
}

func (k customEnv) mustSave(t *testing.T, scope WakeupScope, key, fields string) WakeupDefinitionView {
	t.Helper()
	view, err := k.save(scope, key, fields, t)
	if err != nil {
		t.Fatalf("save %s %s: %v", scope, fields, err)
	}
	return view
}

// target is the named-agent target every fixture rule uses.
func (k customEnv) target() string { return `"target":{"type":"agent","id":"` + k.agent + `"}` }

func (k customEnv) eventRule(events string) string {
	return `"enabled":true,"trigger":{"kind":"event","events":` + events + `},` + k.target() + `,"instruction":"look at it"`
}

func (k customEnv) conditionRule(condition string) string {
	return `"enabled":true,"trigger":{"kind":"condition","condition":` + condition + `},` + k.target() + `,"instruction":"it is ready"`
}

func (k customEnv) storedEventTypes(t *testing.T, key string) []string {
	t.Helper()
	var types []string
	if err := k.f.Pool.QueryRow(context.Background(), `SELECT event_types FROM issue_wakeup_definition WHERE workspace_id=$1 AND rule_key=$2 ORDER BY created_at LIMIT 1`, k.f.WorkspaceID, key).Scan(&types); err != nil {
		t.Fatal(err)
	}
	return types
}

func TestCustomDefinitionAcceptsEventAndConditionTriggers(t *testing.T) {
	k := newCustomEnv(t)
	event := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["task.failed","comment.created","comment.created"]`))
	if event.RuleKey == "" || !event.Root {
		t.Fatalf("event rule = %+v, want a new custom root", event)
	}
	if got := k.storedEventTypes(t, event.RuleKey); !slices.Equal(got, []string{"comment.created", "task.failed"}) {
		t.Fatalf("event selector = %v", got)
	}
	cond := k.mustSave(t, WakeupScopeProject, "", k.conditionRule(`{"type":"issue_field","field":"status","value":"in_review"}`))
	got := k.storedEventTypes(t, cond.RuleKey)
	// The condition's own hint event wakes its evaluation early; the activation
	// signal brings issues that newly become eligible into the rule.
	if !slices.Contains(got, "issue.status_changed") || !slices.Contains(got, wakeupActivateEvent) {
		t.Fatalf("condition selector = %v, want the status hint and the activation signal", got)
	}
}

func TestCustomDefinitionRefusesWhatNothingRunsYet(t *testing.T) {
	k := newCustomEnv(t)
	foreign := newCustomEnv(t)
	label := foreign.f.Insert(t, "issue_label", testutil.Cols{"workspace_id": foreign.f.WorkspaceID, "name": "foreign", "color": "#444444"})
	for name, fields := range map[string]string{
		"schedule":              k.eventRule(`["comment.created"]`) + `,"schedule":{"kind":"every","seconds":3600}`,
		"cron trigger":          `"enabled":true,"trigger":{"kind":"cron"},` + k.target() + `,"instruction":"x"`,
		"at trigger":            `"enabled":true,"trigger":{"kind":"at"},` + k.target() + `,"instruction":"x"`,
		"event without events":  `"enabled":true,"trigger":{"kind":"event"},` + k.target() + `,"instruction":"x"`,
		"empty events":          k.eventRule(`[]`),
		"unknown event":         k.eventRule(`["not.an.event"]`),
		"events not a list":     k.eventRule(`"comment.created"`),
		"unknown condition":     k.conditionRule(`{"type":"nonsense"}`),
		"missing condition":     `"enabled":true,"trigger":{"kind":"condition"},` + k.target() + `,"instruction":"x"`,
		"each_stage reserved":   k.conditionRule(`{"type":"children_done","each_stage":true}`),
		"unknown status":        k.conditionRule(`{"type":"issue_field","field":"status","value":"no_such_status"}`),
		"foreign label":         k.conditionRule(`{"type":"issue_field","field":"label","label_id":"` + label + `"}`),
		"other_issue no issue":  k.conditionRule(`{"type":"other_issue","state":"done"}`),
		"unreadable condition":  k.conditionRule(`{"type":"issue_field","field":"status","value":"todo","bogus":1}`),
		"event with a filter":   k.eventRule(`["comment.created"]`) + `,"filters":{"base_branch":"main"}`,
		"condition on built-in": `"trigger":{"kind":"condition","condition":{"type":"children_done"}}`,
	} {
		key := ""
		if name == "condition on built-in" {
			key = SystemRuleChildDone
		}
		if _, err := k.save(WakeupScopeWorkspace, key, fields, t); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !errors.Is(err, ErrWakeupInput) && !errors.Is(err, ErrWakeupForbidden) {
			t.Errorf("%s: error %v is neither an input nor a permission refusal", name, err)
		}
	}
	if n := k.f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1`, k.f.WorkspaceID); n != 0 {
		t.Fatalf("a refused definition was stored: %d rows", n)
	}
}

// A preview resolves without persisting, so it validates the same way.
func TestCustomDefinitionPreviewValidatesConditions(t *testing.T) {
	k := newCustomEnv(t)
	ref := k.scopeRef(t, WakeupScopeWorkspace)
	owner := parseTestUUID(t, k.owner)
	if _, err := k.s.EffectiveWakeupRule(context.Background(), ref, owner, "", &WakeupDefinitionWrite{Patch: patchOf(t, k.conditionRule(`{"type":"nonsense"}`))}); !errors.Is(err, ErrWakeupInput) {
		t.Fatalf("preview of a bad condition: %v, want an input refusal", err)
	}
	eff, err := k.s.EffectiveWakeupRule(context.Background(), ref, owner, "", &WakeupDefinitionWrite{Patch: patchOf(t, k.conditionRule(`{"type":"issue_field","field":"status","value":"done"}`))})
	if err != nil || !strings.Contains(string(eff.Config.Trigger.Value), "condition") {
		t.Fatalf("preview = %+v, %v", eff, err)
	}
}

// bulkIssues adds n issues to a project in one statement; ids are random, so the
// sweep's keyset order is unrelated to creation order.
func (k customEnv) bulkIssues(t *testing.T, n int, project, status string) {
	t.Helper()
	k.f.Exec(t, `INSERT INTO issue(workspace_id,title,status,priority,creator_type,creator_id,position,number,project_id)
 SELECT $1,'bulk '||g,$5,'none','member',$2,0,(SELECT COALESCE(MAX(number),0) FROM issue WHERE workspace_id=$1)+g,$3 FROM generate_series(1,$4) g`,
		k.f.WorkspaceID, k.f.UserID, project, n, status)
}

func (k customEnv) sweep(t *testing.T) {
	t.Helper()
	if err := k.s.SweepWakeupDefinitions(context.Background(), parseTestUUID(t, k.f.WorkspaceID)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

// sweepAll runs passes until none is pending and returns how many it took.
func (k customEnv) sweepAll(t *testing.T) int {
	t.Helper()
	for pass := 1; pass <= 40; pass++ {
		k.sweep(t)
		if k.f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1 AND (NOT sweep_done OR sweep_revision<>revision)`, k.f.WorkspaceID) == 0 {
			return pass
		}
	}
	t.Fatal("the sweep never finished")
	return 0
}

type customInstance struct {
	ID, Issue, State, Fingerprint string
	Condition                     []byte
	Enabled                       bool
	CapacityReason                *string
	Revision                      int64
}

func (k customEnv) customInstances(t *testing.T, rule string) []customInstance {
	t.Helper()
	rows, err := k.f.Pool.Query(context.Background(), `SELECT id::text,issue_id::text,condition_state,COALESCE(config_fingerprint,''),condition,enabled,capacity_reason,revision
 FROM issue_wakeup WHERE workspace_id=$1 AND default_rule_key=$2 ORDER BY issue_id`, k.f.WorkspaceID, rule)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []customInstance
	for rows.Next() {
		var i customInstance
		if err := rows.Scan(&i.ID, &i.Issue, &i.State, &i.Fingerprint, &i.Condition, &i.Enabled, &i.CapacityReason, &i.Revision); err != nil {
			t.Fatal(err)
		}
		out = append(out, i)
	}
	return out
}

func (k customEnv) taskCount(t *testing.T) int {
	t.Helper()
	return k.f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE workspace_id=$1)`, k.f.WorkspaceID)
}

const statusIsTodo = `{"type":"issue_field","field":"status","value":"todo"}`

func (k customEnv) tick(t *testing.T) {
	t.Helper()
	if err := k.s.TickWorkspaces(context.Background(), parseTestUUID(t, k.f.WorkspaceID)); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

// ruleTasks are the runs a rule's instances queued, oldest first.
func (k customEnv) ruleTasks(t *testing.T, rule string) []db.AgentTaskQueue {
	t.Helper()
	rows, err := k.f.Pool.Query(context.Background(), `SELECT t.id FROM agent_task_queue t JOIN issue_wakeup w ON w.id::text=t.context->>'wakeup_id'
 WHERE w.workspace_id=$1 AND w.default_rule_key=$2 ORDER BY t.created_at,t.id`, k.f.WorkspaceID, rule)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []db.AgentTaskQueue
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		task, err := k.f.q.GetAgentTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, task)
	}
	return out
}

func (k customEnv) wantRuns(t *testing.T, rule string, want int) []db.AgentTaskQueue {
	t.Helper()
	got := k.ruleTasks(t, rule)
	if len(got) != want {
		t.Fatalf("rule %s queued %d runs, want %d", rule, len(got), want)
	}
	return got
}

// finish ends the runs so a rule is not held back by an unfinished one.
func (k customEnv) finish(t *testing.T) {
	t.Helper()
	k.f.Exec(t, `UPDATE agent_task_queue SET status='completed',started_at=COALESCE(started_at,clock_timestamp()),completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, k.issue)
}

func (k customEnv) instanceRow(t *testing.T, rule string) db.IssueWakeup {
	t.Helper()
	w, err := k.f.q.GetDefaultWakeupInstance(context.Background(), db.GetDefaultWakeupInstanceParams{IssueID: parseTestUUID(t, k.issue), RuleKey: rule})
	if err != nil {
		t.Fatalf("no instance of %s: %v", rule, err)
	}
	return w
}

func TestCustomPreviewCountsTheIssuesAlreadySatisfied(t *testing.T) {
	k := newCustomEnv(t)
	k.bulkIssues(t, 3, k.project, "todo")      // with the kit's issue: 4 satisfy it
	k.bulkIssues(t, 2, k.project, "in_review") // open, but not todo
	k.bulkIssues(t, 2, k.project, "backlog")   // dormant: not eligible, not counted
	k.bulkIssues(t, 2, k.project, "done")      // closed: not eligible, not counted
	ref := k.scopeRef(t, WakeupScopeProject)
	eff, err := k.s.EffectiveWakeupRule(context.Background(), ref, parseTestUUID(t, k.owner), "", &WakeupDefinitionWrite{Patch: patchOf(t, k.conditionRule(statusIsTodo))})
	if err != nil {
		t.Fatal(err)
	}
	got := eff.AlreadySatisfied
	if got == nil || got.Satisfied != 4 || got.Examined != 6 || got.Truncated {
		t.Fatalf("already satisfied = %+v, want 4 of 6 eligible issues, not truncated", got)
	}
	// An event rule has no predicate to count.
	event, err := k.s.EffectiveWakeupRule(context.Background(), ref, parseTestUUID(t, k.owner), "", &WakeupDefinitionWrite{Patch: patchOf(t, k.eventRule(`["comment.created"]`))})
	if err != nil || event.AlreadySatisfied != nil {
		t.Fatalf("event preview = %+v, %v, want no count", event.AlreadySatisfied, err)
	}
}

func TestCustomPreviewCountIsBounded(t *testing.T) {
	k := newCustomEnv(t)
	k.bulkIssues(t, wakeupPreviewExamineLimit+20, k.project, "todo")
	eff, err := k.s.EffectiveWakeupRule(context.Background(), k.scopeRef(t, WakeupScopeProject), parseTestUUID(t, k.owner), "", &WakeupDefinitionWrite{Patch: patchOf(t, k.conditionRule(statusIsTodo))})
	if err != nil {
		t.Fatal(err)
	}
	if got := eff.AlreadySatisfied; got == nil || got.Examined != wakeupPreviewExamineLimit || !got.Truncated || got.Satisfied != wakeupPreviewExamineLimit {
		t.Fatalf("already satisfied = %+v, want a count over the first %d issues, flagged truncated", got, wakeupPreviewExamineLimit)
	}
}

func TestCustomRootFireCapDefaultsOnlyOnCreationOfARepeatingRule(t *testing.T) {
	k := newCustomEnv(t)
	repeating := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`))
	if got := repeating.Patch.MaxFires; !got.Set || got.Value != wakeupCustomFireDefault {
		t.Fatalf("a new repeating rule's max_fires = %+v, want the default %d", got, wakeupCustomFireDefault)
	}
	once := k.mustSave(t, WakeupScopeWorkspace, "", k.eventRule(`["comment.created"]`)+`,"mode":"once"`)
	if once.Patch.MaxFires.Set {
		t.Fatalf("a once rule gained max_fires %+v", once.Patch.MaxFires)
	}
	// An edit that names none keeps none: the default is for creation only.
	edited := k.mustSave(t, WakeupScopeWorkspace, once.RuleKey, k.eventRule(`["comment.created"]`)+`,"mode":"continuous"`)
	if edited.Patch.MaxFires.Set {
		t.Fatalf("an edit inserted max_fires %+v", edited.Patch.MaxFires)
	}
	// An instruction-only override of a built-in inserts nothing either.
	override := k.mustSave(t, WakeupScopeProject, SystemRulePRMerged, `"instruction":"project text"`)
	if override.Patch.MaxFires.Set || override.Patch.AggregateLimit.Set {
		t.Fatalf("a built-in override gained limits: %+v", override.Patch)
	}
}
