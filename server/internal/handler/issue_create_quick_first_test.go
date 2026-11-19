package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-7680 (slice 15 of CHE-7658): the quick_first arm at issue create. A root
// issue of a listed type whose arm-D route is bound draws by number parity:
// even = arm Q (the Quick workflow as the issue's override), odd = arm D (the
// route it had). The setting is unset by default; unset, the create path is the
// one on base cf71fbcdad.

type quickFirstFixture struct {
	quick, mapped          string // workflow ids: the Quick workflow, and the arm-D route
	agentQ, agentD, agentN string
	projectID              string
	originalSettings       string
}

func newQuickFirstFixture(t *testing.T) quickFirstFixture {
	t.Helper()
	seedTestCatalog(t)
	withFeatureFlag(t, testHandler, featureflags.ProjectWorkflowsV1, true)
	runtimeID := ""
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, seededReadyAgentID(t)).Scan(&runtimeID)
	f := quickFirstFixture{
		agentQ: dbfx.Agent(t, "Quick author "+workflowTestSuffix(), runtimeID),
		agentD: dbfx.Agent(t, "Quick arm D "+workflowTestSuffix(), runtimeID),
		agentN: dbfx.Agent(t, "Quick named author "+workflowTestSuffix(), runtimeID),
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = ANY($1)`, []string{f.agentQ, f.agentD, f.agentN})
	})
	f.quick = createTestWorkflow(t, "quick-first-q-"+workflowTestSuffix(), "todo", "in_progress", "in_review").ID
	f.mapped = createTestWorkflow(t, "quick-first-d-"+workflowTestSuffix(), "todo", "in_progress", "in_review").ID
	setWorkflowHandoff(t, f.quick, "todo", handToAgent(f.agentQ, "author")).Want(http.StatusOK)
	setWorkflowHandoff(t, f.mapped, "todo", handToAgent(f.agentD, "arm D")).Want(http.StatusOK)
	f.projectID = createWorkflowTestProject(t, "Quick first project")
	setProjectWorkflow(t, f.projectID, map[string]any{"workflow_id": f.mapped}).Want(http.StatusOK)
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&f.originalSettings)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, f.originalSettings)
	})
	return f
}

// setting stores the quick_first value (nil removes the key) and the
// workflow_by_type map (nil removes it), leaving the other keys alone.
func (f quickFirstFixture) setting(t *testing.T, quickFirst map[string]any, byType map[string]string) {
	t.Helper()
	put := func(key string, value any, present bool) {
		if !present {
			dbfx.Exec(t, `UPDATE workspace SET settings = COALESCE(settings, '{}'::jsonb) - $2 WHERE id = $1`, testWorkspaceID, key)
			return
		}
		raw, _ := json.Marshal(value)
		dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'::jsonb), ARRAY[$2::text], $3::jsonb) WHERE id = $1`, testWorkspaceID, key, string(raw))
	}
	put("quick_first", quickFirst, quickFirst != nil)
	put("workflow_by_type", byType, byType != nil)
}

// on is the quick_first value of an open window over the feat and fix types.
func (f quickFirstFixture) on(over map[string]any) map[string]any {
	qf := map[string]any{
		"workflow_id": f.quick, "types": []string{"feat", "fix"},
		"starts_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"ends_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	for k, v := range over {
		qf[k] = v
	}
	return qf
}

func (f quickFirstFixture) byType() map[string]string { return map[string]string{"feat": f.mapped} }

type quickCreated struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
}

// create makes a root issue with the next number even or odd. A "project_id"
// of "" in over means no project; a nil value removes any other key.
func (f quickFirstFixture) create(t *testing.T, even bool, over map[string]any) quickCreated {
	t.Helper()
	nextNumber(t, even)
	body := map[string]any{"title": "feat(x): quick first " + workflowTestSuffix(), "project_id": f.projectID}
	for k, v := range over {
		if v == nil || v == "" {
			delete(body, k)
			continue
		}
		body[k] = v
	}
	var created quickCreated
	testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)).Want(http.StatusCreated).JSON(&created)
	if created.Number%2 == 1 == even {
		t.Fatalf("number %d is not %s", created.Number, map[bool]string{true: "even", false: "odd"}[even])
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, util.MustParseUUID(created.ID))
	})
	return created
}

func (f quickFirstFixture) wantQuick(t *testing.T, issueID string) {
	t.Helper()
	if got := issueOverride(t, issueID); got != f.quick {
		t.Fatalf("workflow override = %q, want the Quick workflow %s", got, f.quick)
	}
	status, _, assignee := issueStatusAndAssignee(t, issueID)
	if status != "todo" || assignee != f.agentQ {
		t.Fatalf("Quick issue = status %q assignee %q, want todo and %s", status, assignee, f.agentQ)
	}
	if steps := pendingStepRuns(t, issueID, f.agentQ); len(steps) != 1 || steps[0] != "todo" {
		t.Fatalf("pending runs for the author = %v, want one for todo", steps)
	}
}

// wantMapped is arm D with a title-mapped route: the override is the mapped
// workflow, as on base.
func (f quickFirstFixture) wantMapped(t *testing.T, issueID string) {
	t.Helper()
	if got := issueOverride(t, issueID); got != f.mapped {
		t.Fatalf("workflow override = %q, want the mapped workflow %s", got, f.mapped)
	}
	if steps := pendingStepRuns(t, issueID, f.agentD); len(steps) != 1 || steps[0] != "todo" {
		t.Fatalf("pending runs for arm D = %v, want one for todo", steps)
	}
	if steps := pendingStepRuns(t, issueID, f.agentQ); len(steps) != 0 {
		t.Fatalf("arm D issue holds %v runs for the Quick author", steps)
	}
}

func (f quickFirstFixture) wantNoOverride(t *testing.T, issueID string) {
	t.Helper()
	if got := issueOverride(t, issueID); got != "" {
		t.Fatalf("workflow override = %q, want none", got)
	}
}

func TestQuickFirstDrawsByParityForAMappedType(t *testing.T) {
	f := newQuickFirstFixture(t)
	f.setting(t, f.on(nil), f.byType())
	logged := captureHoldLog(t)

	// Mapped type with no override: Q enters Quick todo with the author
	// handoff, D keeps the mapped route.
	f.wantQuick(t, f.create(t, true, nil).ID)
	f.wantMapped(t, f.create(t, false, nil).ID)

	t.Run("mapped type outside any project", func(t *testing.T) {
		f.wantQuick(t, f.create(t, true, map[string]any{"project_id": ""}).ID)
		f.wantMapped(t, f.create(t, false, map[string]any{"project_id": ""}).ID)
	})
	if !strings.Contains(logged.String(), "quick_first") {
		t.Fatalf("the draw logged no quick_first line: %q", logged.String())
	}
}

func TestQuickFirstDrawsByParityForAProjectRoute(t *testing.T) {
	f := newQuickFirstFixture(t)
	f.setting(t, f.on(nil), nil) // no title map: the route is the project's workflow
	f.wantQuick(t, f.create(t, true, nil).ID)
	odd := f.create(t, false, nil)
	f.wantNoOverride(t, odd.ID)
	if steps := pendingStepRuns(t, odd.ID, f.agentD); len(steps) != 1 {
		t.Fatalf("arm D issue pending runs for the project handler = %v, want one", steps)
	}
}

func TestQuickFirstLeavesTypesOutsideTheListAlone(t *testing.T) {
	f := newQuickFirstFixture(t)
	f.setting(t, f.on(nil), map[string]string{"plan": f.mapped})
	got := f.create(t, true, map[string]any{"title": "plan(x): not a code type " + workflowTestSuffix()})
	f.wantMapped(t, got.ID)
	if o := issueOverride(t, f.create(t, true, map[string]any{"title": "no type here " + workflowTestSuffix()}).ID); o != "" {
		t.Fatalf("an untyped title got override %q", o)
	}
}

func TestQuickFirstOffIsTheBasePath(t *testing.T) {
	f := newQuickFirstFixture(t)
	sf := newCreateSplitFixture(t) // an active project split, arm B on even numbers
	f.setting(t, nil, f.byType())
	for _, on := range []bool{false, true} {
		name := map[bool]string{false: "quick_first off", true: "quick_first on, project split active"}[on]
		t.Run(name, func(t *testing.T) {
			if on {
				f.setting(t, f.on(nil), f.byType())
			}
			// Mapped title: the mapped workflow is explicit to the split, which
			// leaves the issue alone; no split activity.
			mapped := sf.create(t, true, map[string]any{"title": "feat(x): mapped " + workflowTestSuffix()})
			if got := issueOverride(t, mapped.ID); got != f.mapped {
				t.Fatalf("mapped override = %q, want %s", got, f.mapped)
			}
			if status, _, _ := issueStatusAndAssignee(t, mapped.ID); status != "in_progress" {
				t.Fatalf("mapped status = %q, want in_progress", status)
			}
			if n := activityCount(t, mapped.ID, "workflow_split"); n != 0 {
				t.Fatalf("%d split activity rows on a mapped issue, want 0", n)
			}
			// Unmapped title: the split draws as on base.
			f.setting(t, map[bool]map[string]any{false: nil, true: f.on(nil)}[on], nil)
			split := sf.create(t, true, map[string]any{"title": "feat(x): split " + workflowTestSuffix()})
			sf.wantArm(t, split.ID, true)
			if n := activityCount(t, split.ID, "workflow_split"); n != 1 {
				t.Fatalf("%d split activity rows, want 1", n)
			}
		})
	}
}

func TestQuickFirstUnprojectedWithoutAMappedRouteIsIneligible(t *testing.T) {
	f := newQuickFirstFixture(t)
	f.setting(t, f.on(nil), map[string]string{"design": f.mapped}) // feat is not mapped
	logged := captureHoldLog(t)
	for _, even := range []bool{true, false} {
		got := f.create(t, even, map[string]any{"project_id": ""})
		f.wantNoOverride(t, got.ID)
	}
	if !strings.Contains(logged.String(), "reason=no_full_workflow") {
		t.Fatalf("no no_full_workflow line in %q", logged.String())
	}
}

func TestQuickFirstCallerOverrideStaysOut(t *testing.T) {
	f := newQuickFirstFixture(t)
	f.setting(t, f.on(nil), f.byType())
	logged := captureHoldLog(t)
	explicit := createTestWorkflow(t, "quick-first-explicit-"+workflowTestSuffix(), "todo", "in_progress")
	nextNumber(t, true)
	body := map[string]any{"title": "feat(x): explicit " + workflowTestSuffix(), "project_id": f.projectID, "workflow_id": explicit.ID}
	var got quickCreated
	req := withActorSource(newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body), "interactive_jwt")
	testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated).JSON(&got)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, util.MustParseUUID(got.ID))
	})
	if o := issueOverride(t, got.ID); o != explicit.ID {
		t.Fatalf("override = %q, want the caller's %s", o, explicit.ID)
	}
	if !strings.Contains(logged.String(), "reason=caller_override") {
		t.Fatalf("no caller_override line in %q", logged.String())
	}
}

func TestQuickFirstStatusAndAssigneeEligibility(t *testing.T) {
	f := newQuickFirstFixture(t)
	f.setting(t, f.on(nil), f.byType())

	t.Run("named in_progress is ineligible", func(t *testing.T) {
		got := f.create(t, true, map[string]any{"status": "in_progress"})
		if o := issueOverride(t, got.ID); o != f.mapped {
			t.Fatalf("override = %q, want the mapped route %s", o, f.mapped)
		}
	})
	t.Run("named todo is eligible", func(t *testing.T) {
		f.wantQuick(t, f.create(t, true, map[string]any{"status": "todo"}).ID)
	})
	t.Run("named backlog stays in backlog in Quick", func(t *testing.T) {
		got := f.create(t, true, map[string]any{"status": "backlog"})
		if o := issueOverride(t, got.ID); o != f.quick {
			t.Fatalf("override = %q, want Quick", o)
		}
		if status, _, _ := issueStatusAndAssignee(t, got.ID); status != "backlog" {
			t.Fatalf("status = %q, want backlog", status)
		}
	})
	t.Run("a member or squad assignee is ineligible", func(t *testing.T) {
		for name, assignee := range map[string]map[string]any{
			"member": {"assignee_type": "member", "assignee_id": testUserID},
			"squad":  {"assignee_type": "squad", "assignee_id": dbfx.Squad(t, "Quick squad "+workflowTestSuffix(), f.agentN)},
		} {
			got := f.create(t, true, assignee)
			if o := issueOverride(t, got.ID); o != f.mapped {
				t.Fatalf("%s: override = %q, want the mapped route", name, o)
			}
		}
	})
	t.Run("a named agent becomes the sticky author", func(t *testing.T) {
		got := f.create(t, true, map[string]any{"assignee_type": "agent", "assignee_id": f.agentN})
		if o := issueOverride(t, got.ID); o != f.quick {
			t.Fatalf("override = %q, want Quick", o)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM issue_step_assignee WHERE issue_id = $1 AND status_key = '_author' AND assignee_type = 'agent' AND assignee_id = $2`, got.ID, f.agentN); n != 1 {
			t.Fatalf("%d sticky author rows, want 1", n)
		}
		odd := f.create(t, false, map[string]any{"assignee_type": "agent", "assignee_id": f.agentN})
		if n := dbfx.Count(t, `SELECT count(*) FROM issue_step_assignee WHERE issue_id = $1`, odd.ID); n != 0 {
			t.Fatalf("an arm D issue has %d sticky rows, want 0", n)
		}
	})
}

func TestQuickFirstHoldLandsInBacklogInQuick(t *testing.T) {
	f := newQuickFirstFixture(t)
	hold := newHoldFixture(t, nil)
	f.setting(t, f.on(nil), nil)
	response := holdCreateAs(t, map[string]any{"title": "feat(x): held " + workflowTestSuffix(), "project_id": hold.projectID}, true)
	response.Want(http.StatusCreated)
	issueID := response.Map()["id"].(string)
	if status, _, _ := issueStatusAndAssignee(t, issueID); status != "backlog" {
		t.Fatalf("held issue status = %q, want backlog", status)
	}
	if holdMarker(t, issueID) == "" {
		t.Fatal("the held issue carries no hold marker")
	}
	// Even and odd numbers differ only by the draw; the hold applies to both,
	// so only an even number reaches Quick.
	if o := issueOverride(t, issueID); o != f.quick && o != "" {
		t.Fatalf("override = %q, want Quick or none", o)
	}
}

func TestQuickFirstChildFollowsItsParent(t *testing.T) {
	f := newQuickFirstFixture(t)
	f.setting(t, f.on(nil), nil)
	quickParent := f.create(t, true, nil)
	mappedParent := f.create(t, false, nil)
	for _, even := range []bool{true, false} {
		child := f.create(t, even, map[string]any{"parent_issue_id": quickParent.ID, "title": "feat(y): child " + workflowTestSuffix()})
		if o := issueOverride(t, child.ID); o != f.quick {
			t.Fatalf("child of a Quick parent (even=%v): override %q, want Quick", even, o)
		}
		other := f.create(t, even, map[string]any{"parent_issue_id": mappedParent.ID, "title": "feat(y): child " + workflowTestSuffix()})
		f.wantNoOverride(t, other.ID)
	}
}

func TestQuickFirstWindowEdges(t *testing.T) {
	f := newQuickFirstFixture(t)
	now := time.Now().UTC()
	for name, window := range map[string]map[string]any{
		"not started": {"starts_at": now.Add(time.Hour).Format(time.RFC3339), "ends_at": now.Add(2 * time.Hour).Format(time.RFC3339)},
		"ended":       {"starts_at": now.Add(-2 * time.Hour).Format(time.RFC3339), "ends_at": now.Add(-time.Hour).Format(time.RFC3339)},
	} {
		t.Run(name, func(t *testing.T) {
			f.setting(t, f.on(window), f.byType())
			f.wantMapped(t, f.create(t, true, nil).ID)
		})
	}
	t.Run("open", func(t *testing.T) {
		f.setting(t, f.on(nil), f.byType())
		f.wantQuick(t, f.create(t, true, nil).ID)
	})
}

func TestQuickFirstWithAMissingQuickWorkflowIsIneligible(t *testing.T) {
	f := newQuickFirstFixture(t)
	f.setting(t, f.on(map[string]any{"workflow_id": "00000000-0000-4000-8000-000000000000"}), f.byType())
	f.wantMapped(t, f.create(t, true, nil).ID)
}

// The setting is on the decision-action allow table and checked on both write
// paths with one validator.

const quickFirstWindow = `"starts_at":"2026-11-20T00:00:00Z","ends_at":"2026-12-11T00:00:00Z"`

func quickFirstValue(extra string) string {
	return `{"workflow_id":"7328545d-f16f-4bdb-9deb-27f5d70b1210","types":["feat","fix"],` + quickFirstWindow + extra + `}`
}

var badQuickFirst = map[string]string{
	"not an object":          `["feat"]`,
	"unknown field":          quickFirstValue(`,"script":"rm"`),
	"workflow not a uuid":    `{"workflow_id":"quick","types":["feat"],` + quickFirstWindow + `}`,
	"no types":               `{"workflow_id":"7328545d-f16f-4bdb-9deb-27f5d70b1210","types":[],` + quickFirstWindow + `}`,
	"uppercase type":         `{"workflow_id":"7328545d-f16f-4bdb-9deb-27f5d70b1210","types":["Feat"],` + quickFirstWindow + `}`,
	"type with a space":      `{"workflow_id":"7328545d-f16f-4bdb-9deb-27f5d70b1210","types":["a b"],` + quickFirstWindow + `}`,
	"ends before starts":     `{"workflow_id":"7328545d-f16f-4bdb-9deb-27f5d70b1210","types":["feat"],"starts_at":"2026-12-11T00:00:00Z","ends_at":"2026-11-20T00:00:00Z"}`,
	"window not a timestamp": `{"workflow_id":"7328545d-f16f-4bdb-9deb-27f5d70b1210","types":["feat"],"starts_at":"soon","ends_at":"later"}`,
	"missing window":         `{"workflow_id":"7328545d-f16f-4bdb-9deb-27f5d70b1210","types":["feat"]}`,
}

func TestQuickFirstSettingValidation(t *testing.T) {
	if err := validateSettingsPatchValue("quick_first", json.RawMessage(quickFirstValue(``))); err != nil {
		t.Fatalf("valid value refused: %v", err)
	}
	for name, raw := range badQuickFirst {
		if err := validateSettingsPatchValue("quick_first", json.RawMessage(raw)); err == nil {
			t.Errorf("patch: %s accepted", name)
		}
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err)
		}
		if err := validateQuickFirstSetting(map[string]any{"quick_first": value}); err == nil {
			t.Errorf("ordinary save: %s accepted", name)
		}
	}
	if err := validateQuickFirstSetting(map[string]any{"other": 1}); err != nil {
		t.Errorf("settings without the key refused: %v", err)
	}
	if err := validateQuickFirstSetting(map[string]any{"quick_first": nil}); err != nil {
		t.Errorf("null refused: %v", err)
	}
}

func TestQuickFirstIsAnAllowTableKey(t *testing.T) {
	if err := decisionaction.CheckConfigChange(decisionaction.SettingsField, []string{"quick_first"}); err != nil {
		t.Fatalf("quick_first is not patchable: %v", err)
	}
}
