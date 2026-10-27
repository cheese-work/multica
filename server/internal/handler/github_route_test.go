package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const routeGitHubID int64 = 9100211

type routeFixture struct {
	userID  string
	project string
	issue   db.Issue
	agent   db.Agent
}

// newRouteFixture builds a mapped, active member with NO grant: the deny
// default every other case starts from.
func newRouteFixture(t *testing.T) routeFixture {
	t.Helper()
	userID := dbfx.User(t, "Route Member", fmt.Sprintf("route-%s@multica.ai", strings.ToLower(t.Name())))
	dbfx.Member(t, testWorkspaceID, userID, "member")
	workspace, err := testHandler.Queries.GetWorkspace(context.Background(), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings=$1 WHERE id=$2`, workspace.Settings, testWorkspaceID)
	})
	settings, _ := json.Marshal(map[string]any{"github_review_members": map[string]string{fmt.Sprint(routeGitHubID): userID}})
	dbfx.Exec(t, `UPDATE workspace SET settings=$1 WHERE id=$2`, settings, testWorkspaceID)

	project := dbfx.Project(t, "Route project")
	issueID := dbfx.Issue(t, "Route issue", testutil.Cols{"project_id": project})
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM github_route_grant WHERE project_id=$1`, project) })
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(createHandlerTestAgent(t, "route-target", nil)))
	if err != nil {
		t.Fatal(err)
	}
	return routeFixture{userID: userID, project: project, issue: issue, agent: agent}
}

func (f routeFixture) grant(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO github_route_grant (project_id, user_id) VALUES ($1,$2)`, f.project, f.userID)
}

func (f routeFixture) want(t *testing.T, h *Handler, want routeVerdict) {
	t.Helper()
	if got := h.canRoute(context.Background(), routeGitHubID, f.issue, f.agent); got.Verdict != want {
		t.Fatalf("canRoute = %s (%s), want %s", got.Verdict, got.Reason, want)
	}
}

func TestCanRouteAllowsWhenEveryConditionHolds(t *testing.T) {
	f := newRouteFixture(t)
	f.grant(t)
	f.want(t, testHandler, routeAllowed)
}

// T18
func TestCanRouteDeniesMappedMemberWithoutGrant(t *testing.T) {
	f := newRouteFixture(t)
	f.want(t, testHandler, routeDenied)
}

// T19 (predicate half): a grant removed after intake is gone at dispatch.
func TestCanRouteDeniesAfterGrantRevoked(t *testing.T) {
	f := newRouteFixture(t)
	f.grant(t)
	f.want(t, testHandler, routeAllowed)
	dbfx.Exec(t, `DELETE FROM github_route_grant WHERE project_id=$1`, f.project)
	f.want(t, testHandler, routeDenied)
}

// T21 (predicate half): one PR linked to two issues; the grant is on one project.
func TestCanRouteEvaluatesEachIssueOnItsOwnProject(t *testing.T) {
	f := newRouteFixture(t)
	f.grant(t)
	otherProject := dbfx.Project(t, "Route other project")
	otherIssue, err := testHandler.Queries.GetIssue(context.Background(),
		parseUUID(dbfx.Issue(t, "Route other issue", testutil.Cols{"project_id": otherProject})))
	if err != nil {
		t.Fatal(err)
	}
	if got := testHandler.canRoute(context.Background(), routeGitHubID, otherIssue, f.agent); got.Verdict != routeDenied {
		t.Fatalf("ungranted project = %s (%s), want denied", got.Verdict, got.Reason)
	}
	f.want(t, testHandler, routeAllowed)
}

func TestCanRouteUsesTheIssuesCurrentProject(t *testing.T) {
	f := newRouteFixture(t)
	f.grant(t)
	otherProject := dbfx.Project(t, "Route moved-to project")
	dbfx.Exec(t, `UPDATE issue SET project_id=$1 WHERE id=$2`, otherProject, f.issue.ID)
	issue, err := testHandler.Queries.GetIssue(context.Background(), f.issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.issue = issue
	f.want(t, testHandler, routeDenied)
}

func TestCanRouteDeniesEveryOtherFailedCondition(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(t *testing.T, f *routeFixture)
	}{
		{"unmapped GitHub ID", func(t *testing.T, f *routeFixture) {
			dbfx.Exec(t, `UPDATE workspace SET settings='{}' WHERE id=$1`, testWorkspaceID)
		}},
		{"mapped user is not a workspace member", func(t *testing.T, f *routeFixture) {
			dbfx.Exec(t, `DELETE FROM member WHERE workspace_id=$1 AND user_id=$2`, testWorkspaceID, f.userID)
		}},
		{"issue has no project", func(t *testing.T, f *routeFixture) {
			f.issue.ProjectID.Valid = false
		}},
		{"project is inactive", func(t *testing.T, f *routeFixture) {
			dbfx.Exec(t, `UPDATE project SET status='cancelled' WHERE id=$1`, f.project)
		}},
		{"user may not invoke the target agent", func(t *testing.T, f *routeFixture) {
			dbfx.Exec(t, `UPDATE agent SET permission_mode='private' WHERE id=$1`, f.agent.ID)
			f.agent.PermissionMode = "private"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRouteFixture(t)
			f.grant(t)
			tc.break_(t, &f)
			f.want(t, testHandler, routeDenied)
		})
	}
}

// failingDB errors on any statement containing match, so each lookup in the
// predicate can be failed on its own.
type failingDB struct {
	db.DBTX
	match string
	skip  *int // successful matches to let through before failing; nil fails the first
}

type routeErrRow struct{}

func (routeErrRow) Scan(...any) error { return errors.New("injected lookup failure") }

func (f failingDB) fails(sql string) bool {
	if !strings.Contains(sql, f.match) {
		return false
	}
	if f.skip != nil && *f.skip > 0 {
		*f.skip--
		return false
	}
	return true
}

func (f failingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.fails(sql) {
		return pgconn.CommandTag{}, errors.New("injected lookup failure")
	}
	return f.DBTX.Exec(ctx, sql, args...)
}

func (f failingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.fails(sql) {
		return nil, errors.New("injected lookup failure")
	}
	return f.DBTX.Query(ctx, sql, args...)
}

func (f failingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.fails(sql) {
		return routeErrRow{}
	}
	return f.DBTX.QueryRow(ctx, sql, args...)
}

// T20: every lookup error is `deferred`, never authorised and never a denial.
func TestCanRouteDefersOnAnyLookupError(t *testing.T) {
	for _, match := range []string{
		"FROM workspace",               // GitHub ID mapping
		"FROM member",                  // active-member check
		"FROM github_route_grant",      // grant lookup
		"FROM project",                 // project state
		"FROM agent_invocation_target", // canInvokeAgent allow-list
	} {
		t.Run(match, func(t *testing.T) {
			f := newRouteFixture(t)
			f.grant(t)
			h := *testHandler
			h.Queries = db.New(failingDB{DBTX: testPool, match: match})
			f.want(t, &h, routeDeferred)
		})
	}
}

// A member-target match must not hide a failed membership lookup inside the
// invocation check: canRoute defers, while canInvokeAgent keeps its old verdict.
func TestCanRouteDefersWhenInvocationMembershipLookupFails(t *testing.T) {
	f := newRouteFixture(t)
	f.grant(t)
	dbfx.Exec(t, `DELETE FROM agent_invocation_target WHERE agent_id=$1`, uuidToString(f.agent.ID))
	dbfx.Exec(t, `INSERT INTO agent_invocation_target (agent_id, target_type, target_id) VALUES ($1,'member',$2)`, uuidToString(f.agent.ID), f.userID)
	f.want(t, testHandler, routeAllowed)

	skip := 1 // canRoute's own member check passes; the invocation check's fails
	h := *testHandler
	h.Queries = db.New(failingDB{DBTX: testPool, match: "FROM member", skip: &skip})
	f.want(t, &h, routeDeferred)

	skip = 1
	if !h.canInvokeAgent(context.Background(), f.agent, "member", f.userID, "", testWorkspaceID) {
		t.Fatal("canInvokeAgent must keep admitting a matching member target despite the lookup error")
	}
}

func TestCanRouteDefersOnCanceledContext(t *testing.T) {
	f := newRouteFixture(t)
	f.grant(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := testHandler.canRoute(ctx, routeGitHubID, f.issue, f.agent); got.Verdict != routeDeferred {
		t.Fatalf("canceled ctx = %s (%s), want deferred", got.Verdict, got.Reason)
	}
}

func routeGrantRequest(userID, method, project, target string) *http.Request {
	req := newRequestAs(userID, method, "/api/projects/"+project+"/github-route-grants/"+target, nil)
	return withURLParams(req, "id", project, "userId", target)
}

func TestRouteGrantWritesAreOwnerOrAdminOnly(t *testing.T) {
	f := newRouteFixture(t)
	for _, tc := range []struct {
		method  string
		handler http.HandlerFunc
	}{
		{http.MethodPut, testHandler.SetGitHubRouteGrant},
		{http.MethodDelete, testHandler.DeleteGitHubRouteGrant},
	} {
		w := httptest.NewRecorder()
		tc.handler(w, routeGrantRequest(f.userID, tc.method, f.project, f.userID))
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s as plain member = %d %s, want 403", tc.method, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	testHandler.ListGitHubRouteGrants(w, withURLParam(newRequestAs(f.userID, http.MethodGet, "/api/projects/"+f.project+"/github-route-grants", nil), "id", f.project))
	if w.Code != http.StatusForbidden {
		t.Fatalf("list as plain member = %d, want 403", w.Code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_route_grant WHERE project_id=$1`, f.project); n != 0 {
		t.Fatalf("grants after forbidden writes = %d, want 0", n)
	}
}

func TestRouteGrantAdminSetListDelete(t *testing.T) {
	f := newRouteFixture(t)
	w := httptest.NewRecorder()
	testHandler.SetGitHubRouteGrant(w, routeGrantRequest(testUserID, http.MethodPut, f.project, f.userID))
	if w.Code != http.StatusOK {
		t.Fatalf("set as owner = %d %s", w.Code, w.Body.String())
	}
	f.want(t, testHandler, routeAllowed)

	w = httptest.NewRecorder()
	testHandler.ListGitHubRouteGrants(w, withURLParam(newRequestAs(testUserID, http.MethodGet, "/x", nil), "id", f.project))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), f.userID) {
		t.Fatalf("list as owner = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	testHandler.DeleteGitHubRouteGrant(w, routeGrantRequest(testUserID, http.MethodDelete, f.project, f.userID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete as owner = %d %s", w.Code, w.Body.String())
	}
	f.want(t, testHandler, routeDenied)
}

func TestRouteGrantRejectsNonMemberTarget(t *testing.T) {
	f := newRouteFixture(t)
	stranger := dbfx.User(t, "Route Stranger", "route-stranger@multica.ai")
	w := httptest.NewRecorder()
	testHandler.SetGitHubRouteGrant(w, routeGrantRequest(testUserID, http.MethodPut, f.project, stranger))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("grant to non-member = %d %s, want 400", w.Code, w.Body.String())
	}
}

func TestDeleteProjectRemovesItsRouteGrants(t *testing.T) {
	f := newRouteFixture(t)
	f.grant(t)
	w := httptest.NewRecorder()
	testHandler.DeleteProject(w, withURLParam(newRequest(http.MethodDelete, "/api/projects/"+f.project+"?workspace_id="+testWorkspaceID, nil), "id", f.project))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete project = %d %s", w.Code, w.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_route_grant WHERE project_id=$1`, f.project); n != 0 {
		t.Fatalf("grants left after project delete = %d, want 0", n)
	}
}

// Binding chain: installation -> workspace -> PR row -> linked issues, each with
// its current project. Anything off the chain yields no target.
func TestRouteTargetIssuesFollowsTheBindingChain(t *testing.T) {
	f := newRouteFixture(t)
	ctx := context.Background()
	const installation, prNumber = int64(9100211), int32(9100211)
	dbfx.Exec(t, `INSERT INTO github_installation (workspace_id, installation_id, account_login, account_type) VALUES ($1,$2,'route-org','Organization')`, testWorkspaceID, installation)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM github_installation WHERE installation_id=$1`, installation) })
	var prID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO github_pull_request (workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha)
		VALUES ($1,$2,'route-org','route-repo',$3,'t','open','https://example.test/pr',now(),now(),'abc') RETURNING id`,
		testWorkspaceID, installation, prNumber).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM issue_pull_request WHERE pull_request_id=$1`, prID)
		dbfx.Exec(t, `DELETE FROM github_pull_request WHERE id=$1`, prID)
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1,$2)`, uuidToString(f.issue.ID), prID)

	got, err := testHandler.routeTargetIssues(ctx, installation, "route-org", "route-repo", prNumber)
	if err != nil || len(got) != 1 || got[0].ID != f.issue.ID || uuidToString(got[0].ProjectID) != f.project {
		t.Fatalf("chain = %v, %v; want the linked issue in project %s", got, err, f.project)
	}

	other := dbfx.Project(t, "Route moved project")
	dbfx.Exec(t, `UPDATE issue SET project_id=$1 WHERE id=$2`, other, uuidToString(f.issue.ID))
	if got, _ = testHandler.routeTargetIssues(ctx, installation, "route-org", "route-repo", prNumber); len(got) != 1 || uuidToString(got[0].ProjectID) != other {
		t.Fatalf("chain after move = %v; want current project %s", got, other)
	}

	for name, call := range map[string]func() ([]db.Issue, error){
		"unknown installation": func() ([]db.Issue, error) {
			return testHandler.routeTargetIssues(ctx, installation+1, "route-org", "route-repo", prNumber)
		},
		"other repo": func() ([]db.Issue, error) {
			return testHandler.routeTargetIssues(ctx, installation, "route-org", "other", prNumber)
		},
		"other PR number": func() ([]db.Issue, error) {
			return testHandler.routeTargetIssues(ctx, installation, "route-org", "route-repo", prNumber+1)
		},
	} {
		if got, err := call(); err != nil || len(got) != 0 {
			t.Errorf("%s: got %v, %v; want no targets", name, got, err)
		}
	}

	dbfx.Exec(t, `UPDATE github_pull_request SET installation_id=$1 WHERE id=$2`, installation+2, prID)
	if got, err := testHandler.routeTargetIssues(ctx, installation, "route-org", "route-repo", prNumber); err != nil || len(got) != 0 {
		t.Errorf("PR row on another installation: got %v, %v; want no targets", got, err)
	}
}

// Grants have no workspace_id, so workspace teardown must reach them through
// the workspace's projects.
func TestWorkspaceTeardownRemovesRouteGrants(t *testing.T) {
	ctx := context.Background()
	workspace := dbfx.Workspace(t, "Route teardown", "route-teardown")
	user := dbfx.User(t, "Route Teardown", "route-teardown@multica.ai")
	var project string
	if err := testPool.QueryRow(ctx, `INSERT INTO project (workspace_id, title) VALUES ($1,'p') RETURNING id`, workspace).Scan(&project); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `INSERT INTO github_route_grant (project_id, user_id) VALUES ($1,$2)`, project, user)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM github_route_grant WHERE project_id=$1`, project) })
	if err := testHandler.Queries.DeleteWorkspaceLeafData(ctx, parseUUID(workspace)); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_route_grant WHERE project_id=$1`, project); n != 0 {
		t.Fatalf("grants left after workspace teardown = %d, want 0", n)
	}
}
