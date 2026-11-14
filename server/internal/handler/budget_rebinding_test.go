package handler

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// P8 / E17 of the CHE-2798 design: a non-interactive credential cannot widen
// an issue's run budget by rebinding the workflow or project it follows.

func TestBudgetWidens(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	budget := func(cap int, start time.Time) budgetBinding {
		return budgetBinding{budgeted: true, cap: cap, start: start}
	}
	cases := []struct {
		name          string
		before, after budgetBinding
		want          bool
	}{
		{"unbudgeted before never widens", budgetBinding{}, budget(50, t0), false},
		{"budgeted to unbudgeted", budget(5, t0), budgetBinding{}, true},
		{"higher cap", budget(5, t0), budget(6, t0), true},
		{"lower cap", budget(5, t0), budget(4, t0), false},
		{"same cap and start", budget(5, t0), budget(5, t0), false},
		{"later start", budget(5, t0), budget(5, t0.Add(time.Second)), true},
		{"earlier start", budget(5, t0), budget(5, t0.Add(-time.Second)), false},
		{"lower cap but later start", budget(5, t0), budget(4, t0.Add(time.Second)), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := budgetWidens(c.before, c.after); got != c.want {
				t.Fatalf("budgetWidens(%+v, %+v) = %v, want %v", c.before, c.after, got, c.want)
			}
		})
	}
}

type rebindFixture struct {
	c            edgeCase
	narrow, wide string // workflow ids; narrow is the exhausted issue's own
	narrowProj   string
	wideProj     string
}

// newRebindFixture exhausts an issue (cap 2, 2 runs) in a project on the
// narrow workflow, next to a wide workflow (cap 50) with its own project.
func newRebindFixture(t *testing.T, installationID int64) rebindFixture {
	t.Helper()
	c := newEdgeCase(t, installationID, mergeEdgeSteps)
	spendBudget(t, c, 2, "in_progress")
	wide := createTestWorkflowSteps(t, "Wide "+workflowTestSuffix(), mergeEdgeSteps)
	dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue = 50, run_budget_since = now() - interval '1 hour' WHERE id = $1`, wide.ID)
	f := rebindFixture{c: c, narrow: c.workflowID, wide: wide.ID}
	dbfx.QueryRow(t, `SELECT project_id::text FROM issue WHERE id = $1`, c.issueID).Scan(&f.narrowProj)
	f.wideProj = createWorkflowTestProject(t, "Rebind wide project")
	setProjectWorkflow(t, f.wideProj, map[string]any{"workflow_id": wide.ID}).Want(http.StatusOK)
	return f
}

var nonInteractive = []string{"pat", "cloud_pat", "cli_jwt", "task_token"}

type rebindPath struct {
	name string
	// setup puts the fixture where the path widens; call returns the response.
	setup func(t *testing.T, f rebindFixture)
	call  func(t *testing.T, f rebindFixture, source string) *testutil.Response
	// wantInteractive is the status interactive_jwt gets on the widening call.
	wantInteractive int
	// state reads what the call changes.
	state func(t *testing.T, f rebindFixture) string
	// taskTokenCard: a task token's widening ask opens an approval card (202)
	// and changes nothing, instead of the 403 refusal (CHE-5505).
	taskTokenCard bool
}

func withActorSource(req *http.Request, source string) *http.Request {
	req.Header.Set("X-Actor-Source", source)
	return req
}

func rebindPaths() []rebindPath {
	issuePut := func(f rebindFixture, body map[string]any, source string, t *testing.T) *testutil.Response {
		req := withURLParam(withActorSource(newRequest("PATCH", "/api/issues/"+f.c.issueID, body), source), "id", f.c.issueID)
		return testutil.Call(t, testHandler.UpdateIssue, req)
	}
	issueState := func(t *testing.T, f rebindFixture) string {
		var s string
		dbfx.QueryRow(t, `SELECT COALESCE(project_id::text,'-') || '/' || COALESCE(workflow_id::text,'-') FROM issue WHERE id = $1`, f.c.issueID).Scan(&s)
		return s
	}
	projectState := func(t *testing.T, f rebindFixture) string {
		var s string
		dbfx.QueryRow(t, `SELECT COALESCE(workflow_id::text,'-') FROM project WHERE id = $1`, f.narrowProj).Scan(&s)
		return s
	}
	return []rebindPath{
		{
			name: "issue project move",
			call: func(t *testing.T, f rebindFixture, s string) *testutil.Response {
				return issuePut(f, map[string]any{"project_id": f.wideProj}, s, t)
			},
			wantInteractive: http.StatusOK, state: issueState,
		},
		{
			name: "issue move endpoint",
			call: func(t *testing.T, f rebindFixture, s string) *testutil.Response {
				body := map[string]any{"project_id": f.wideProj, "before_id": nil, "after_id": nil}
				req := withURLParam(withActorSource(newRequest("POST", "/api/issues/"+f.c.issueID+"/move", body), s), "id", f.c.issueID)
				return testutil.Call(t, testHandler.MoveIssue, req)
			},
			wantInteractive: http.StatusOK, state: issueState,
		},
		{
			name: "issue batch project move",
			call: func(t *testing.T, f rebindFixture, s string) *testutil.Response {
				req := withActorSource(newRequest("POST", "/api/issues/batch?workspace_id="+testWorkspaceID, map[string]any{
					"issue_ids": []string{f.c.issueID}, "updates": map[string]any{"project_id": f.wideProj},
				}), s)
				return testutil.Call(t, testHandler.BatchUpdateIssues, req)
			},
			wantInteractive: http.StatusOK, state: issueState,
		},
		{
			name: "issue workflow set",
			call: func(t *testing.T, f rebindFixture, s string) *testutil.Response {
				req := withURLParam(withActorSource(newRequest("PUT", "/api/issues/"+f.c.issueID+"/workflow", map[string]any{"workflow_id": f.wide}), s), "id", f.c.issueID)
				return testutil.Call(t, testHandler.SetIssueWorkflow, req)
			},
			wantInteractive: http.StatusOK, state: issueState, taskTokenCard: true,
		},
		{
			name: "issue workflow clear",
			setup: func(t *testing.T, f rebindFixture) {
				// The exhausted issue sits in the wide project on its own narrow override.
				dbfx.Exec(t, `UPDATE issue SET project_id = $2, workflow_id = $3 WHERE id = $1`, f.c.issueID, f.wideProj, f.narrow)
			},
			call: func(t *testing.T, f rebindFixture, s string) *testutil.Response {
				req := withURLParam(withActorSource(newRequest("PUT", "/api/issues/"+f.c.issueID+"/workflow", map[string]any{"workflow_id": nil}), s), "id", f.c.issueID)
				return testutil.Call(t, testHandler.SetIssueWorkflow, req)
			},
			wantInteractive: http.StatusOK, state: issueState, taskTokenCard: true,
		},
		{
			name: "project workflow switch",
			call: func(t *testing.T, f rebindFixture, s string) *testutil.Response {
				req := withURLParam(withActorSource(workflowRequest("POST", "/api/projects/"+f.narrowProj+"/workflow", map[string]any{"workflow_id": f.wide}), s), "id", f.narrowProj)
				return testutil.Call(t, testHandler.SetProjectWorkflow, req)
			},
			wantInteractive: http.StatusOK, state: projectState,
		},
		{
			name: "project workflow clear",
			call: func(t *testing.T, f rebindFixture, s string) *testutil.Response {
				req := withURLParam(withActorSource(workflowRequest("POST", "/api/projects/"+f.narrowProj+"/workflow", map[string]any{"workflow_id": nil}), s), "id", f.narrowProj)
				return testutil.Call(t, testHandler.SetProjectWorkflow, req)
			},
			wantInteractive: http.StatusOK, state: projectState,
		},
		{
			name: "workflow delete",
			call: func(t *testing.T, f rebindFixture, s string) *testutil.Response {
				req := withURLParam(withActorSource(workflowRequest("DELETE", "/api/issue-workflows/"+f.narrow, nil), s), "id", f.narrow)
				return testutil.Call(t, testHandler.DeleteIssueWorkflow, req)
			},
			// A referenced workflow cannot be deleted by anyone.
			wantInteractive: http.StatusConflict,
			state: func(t *testing.T, f rebindFixture) string {
				return fmt.Sprint(dbfx.Count(t, `SELECT count(*) FROM issue_workflow WHERE id = $1`, f.narrow))
			},
		},
	}
}

func wantRebindRefused(t *testing.T, res *testutil.Response, path, source string) {
	t.Helper()
	res.Want(http.StatusForbidden)
	if !strings.Contains(res.Text(), "budget_rebinding_requires_interactive") {
		t.Fatalf("%s as %s: 403 without the budget_rebinding_requires_interactive code: %s", path, source, res.Text())
	}
}

// Matrix row 1 (R3-1): an exhausted issue, each rebinding path that widens,
// each non-interactive credential.
func TestBudgetRebindingWideningRefusedForNonInteractive(t *testing.T) {
	for i, p := range rebindPaths() {
		t.Run(p.name, func(t *testing.T) {
			f := newRebindFixture(t, int64(2830100+i))
			if p.setup != nil {
				p.setup(t, f)
			}
			before := p.state(t, f)
			for _, source := range nonInteractive {
				res := p.call(t, f, source)
				if p.taskTokenCard && source == "task_token" {
					res.Want(http.StatusAccepted)
				} else if p.name == "issue batch project move" {
					// The batch reports a refused item; nothing else changes.
					if !strings.Contains(res.Text(), "budget_rebinding_requires_interactive") {
						t.Fatalf("batch as %s: refusal missing: %s", source, res.Text())
					}
				} else {
					wantRebindRefused(t, res, p.name, source)
				}
				if got := p.state(t, f); got != before {
					t.Fatalf("%s as %s changed state: %q -> %q", p.name, source, before, got)
				}
			}
			p.call(t, f, "interactive_jwt").Want(p.wantInteractive)
		})
	}
}

// Narrowing and neutral changes pass for every credential.
func TestBudgetRebindingNarrowingAllowedForNonInteractive(t *testing.T) {
	f := newRebindFixture(t, 2830200)
	// The issue moves from the wide project to the narrow one; the project
	// switches wide -> narrow.
	dbfx.Exec(t, `UPDATE issue SET project_id = $2 WHERE id = $1`, f.c.issueID, f.wideProj)
	req := withURLParam(withActorSource(newRequest("PATCH", "/api/issues/"+f.c.issueID, map[string]any{"project_id": f.narrowProj}), "pat"), "id", f.c.issueID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)

	req = withURLParam(withActorSource(workflowRequest("POST", "/api/projects/"+f.wideProj+"/workflow", map[string]any{"workflow_id": f.narrow}), "cli_jwt"), "id", f.wideProj)
	testutil.Call(t, testHandler.SetProjectWorkflow, req).Want(http.StatusOK)

	// Neutral: the same workflow again.
	req = withURLParam(withActorSource(newRequest("PUT", "/api/issues/"+f.c.issueID+"/workflow", map[string]any{"workflow_id": nil}), "task_token"), "id", f.c.issueID)
	testutil.Call(t, testHandler.SetIssueWorkflow, req).Want(http.StatusOK)
}

// sqlc queries that rewrite the budget binding, and the guarded path or
// exemption each one is held to. A new query fails this test until it is
// mapped here.
var budgetBindingQueries = map[string]string{
	"UpdateIssue":                          "guarded: UpdateIssue project change (single, batch, move)",
	"SetIssueProjectFromDecision":          "guarded: issue.project.set widens only through a spend-class owner approval",
	"SetIssueWorkflow":                     "guarded: SetIssueWorkflow",
	"SetProjectWorkflow":                   "guarded: SetProjectWorkflow",
	"DeleteIssueWorkflow":                  "guarded: DeleteIssueWorkflow",
	"UpdateIssueWorkflow":                  "guarded: workflow budget fields (S1b)",
	"RemoveStatusFromDefaultIssueWorkflow": "exempt: writes steps only, no budget column",
	"DeleteIssueWorkflowsForWorkspace":     "exempt: workspace teardown",
}

func TestBudgetBindingQueriesAreAllMapped(t *testing.T) {
	files, err := filepath.Glob("../../pkg/db/queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("queries not found: %v", err)
	}
	header := regexp.MustCompile(`(?m)^-- name: (\w+)`)
	comment := regexp.MustCompile(`(?m)--[^\n]*`)
	writes := regexp.MustCompile(`(?is)\bUPDATE\s+(issue_workflow|issue|project)\b(.*?)(?:;|$)|\bDELETE\s+FROM\s+issue_workflow\b`)
	where := regexp.MustCompile(`(?i)\bWHERE\b`)
	binds := regexp.MustCompile(`(?i)\b(project_id|workflow_id)\s*=`)
	var found []string
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		heads := header.FindAllStringSubmatchIndex(string(raw), -1)
		for i, h := range heads {
			end := len(raw)
			if i+1 < len(heads) {
				end = heads[i+1][0]
			}
			name := string(raw[h[2]:h[3]])
			body := comment.ReplaceAllString(string(raw[h[1]:end]), "")
			for _, m := range writes.FindAllStringSubmatch(body, -1) {
				switch {
				case m[1] == "" || strings.EqualFold(m[1], "issue_workflow"): // delete, or update of issue_workflow
					found = append(found, name)
				case binds.MatchString(where.Split(m[2], 2)[0]):
					found = append(found, name)
				}
			}
		}
	}
	sort.Strings(found)
	seen := map[string]bool{}
	for _, name := range found {
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := budgetBindingQueries[name]; !ok {
			t.Errorf("query %s rewrites issue.project_id, issue.workflow_id, project.workflow_id or issue_workflow and is not mapped to a guarded path or exemption", name)
		}
	}
	for name := range budgetBindingQueries {
		if !seen[name] {
			t.Errorf("mapped query %s no longer rewrites a budget binding; drop it from the map", name)
		}
	}
}

// Saving a handoff as a project copy creates a fresh workflow, which starts a
// new budget window, and switches the project to it.
// Cloud PATs and task tokens are stopped earlier, by the route's own gates.
func TestBudgetRebindingHandoffCopyRefusedForNonInteractive(t *testing.T) {
	for i, source := range []string{"pat", "cli_jwt"} {
		t.Run(source, func(t *testing.T) {
			f := newRebindFixture(t, int64(2830900+i))
			body := handToAgent(seededReadyAgentID(t), "Keep this handoff")
			body["copy_for_project_id"] = f.narrowProj
			req := withURLParams(withActorSource(workflowRequest("PUT", "/api/issue-workflows/"+f.narrow+"/handoffs/in_progress", body), source), "id", f.narrow, "status", "in_progress")
			res := testutil.Call(t, RequireHumanOrTaskToken(http.HandlerFunc(testHandler.SetIssueWorkflowHandoff)).ServeHTTP, req)
			res.Want(http.StatusForbidden)
			if !strings.Contains(res.Text(), budgetRebindingCode) {
				t.Fatalf("body %q does not carry %s", res.Text(), budgetRebindingCode)
			}
			var workflow string
			dbfx.QueryRow(t, `SELECT workflow_id::text FROM project WHERE id=$1`, f.narrowProj).Scan(&workflow)
			if workflow != f.narrow {
				t.Fatalf("project workflow changed to %s despite the refusal", workflow)
			}
		})
	}
}

// An issue override above both caps keeps its effective cap, so the switch
// does not widen that issue.
func TestBudgetRebindingNeutralProjectSwitchWithIssueOverride(t *testing.T) {
	f := newRebindFixture(t, 2830910)
	dbfx.Exec(t, `UPDATE issue SET max_runs_override=100 WHERE id=$1`, f.c.issueID)
	dbfx.Exec(t, `UPDATE issue_workflow SET run_budget_since=(SELECT run_budget_since FROM issue_workflow WHERE id=$2) WHERE id=$1`, f.wide, f.narrow)
	req := withURLParam(withActorSource(workflowRequest("POST", "/api/projects/"+f.narrowProj+"/workflow", map[string]any{"workflow_id": f.wide}), "pat"), "id", f.narrowProj)
	testutil.Call(t, testHandler.SetProjectWorkflow, req).Want(http.StatusOK)
}

// budgetGuardFor lists, per rewriting query, the guard its caller must invoke.
// The query inventory above cannot see a second caller of the same query.
var budgetGuardFor = map[string][]string{
	"SetProjectWorkflow":          {"projectSwitchWidensBudget"},
	"SetIssueWorkflow":            {"issueBinding"},
	"DeleteIssueWorkflow":         {"workflowDeleteWidensBudget"},
	"UpdateIssue":                 {"guardIssueProjectRebinding", "PreserveProject"},
	"SetIssueProjectFromDecision": {"loadIssueProjectMove"},
}

// budgetGuardExempt names callers that cannot change a binding.
var budgetGuardExempt = map[string]string{
	"MoveIssue": "delegates to the UpdateIssue handler, which sets the actor for the guard",
}

func TestBudgetBindingCallersAreGuarded(t *testing.T) {
	fset := token.NewFileSet()
	var seen int
	for _, dir := range []string{".", "../service"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(fset, file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range parsed.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				calls := map[string]bool{}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok {
						calls[id.Name] = true
					}
					if call, ok := n.(*ast.CallExpr); ok {
						switch f := call.Fun.(type) {
						case *ast.SelectorExpr:
							calls[f.Sel.Name] = true
						case *ast.Ident:
							calls[f.Name] = true
						}
					}
					return true
				})
				for query, guards := range budgetGuardFor {
					if !calls[query] {
						continue
					}
					seen++
					if _, ok := budgetGuardExempt[fn.Name.Name]; ok {
						continue
					}
					guarded := false
					for _, g := range guards {
						guarded = guarded || calls[g]
					}
					if !guarded {
						t.Errorf("%s: %s calls %s without %v", file, fn.Name.Name, query, guards)
					}
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no caller of a budget-binding query; the scan is broken")
	}
}
