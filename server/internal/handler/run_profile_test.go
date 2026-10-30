package handler

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Profile resolver at claim (CHE-2216 plan 3.1, slice A1b; T9 part).

func TestResolveRunProfile(t *testing.T) {
	grant := func(scopes ...string) db.RunCapabilityGrant { return db.RunCapabilityGrant{Scopes: scopes} }
	cases := []struct {
		name         string
		grants       []db.RunCapabilityGrant
		taint        []string
		issueTainted bool
		fenced       bool
		want         runProfile
	}{
		{"no grant", nil, nil, false, false, runProfile{Profile: "restricted", Scopes: []string{}, TaintIDs: []string{}}},
		{"grant elevates with sorted scope union", []db.RunCapabilityGrant{grant("docker", "device:a"), grant("device:a", "multica_cross_issue")}, nil, false, false,
			runProfile{Profile: "elevated", Scopes: []string{"device:a", "docker", "multica_cross_issue"}, TaintIDs: []string{}}},
		{"tainted task ignores grant", []db.RunCapabilityGrant{grant("docker")}, []string{"t1"}, false, false, runProfile{Profile: "restricted", Scopes: []string{}, TaintIDs: []string{"t1"}}},
		{"tainted issue ignores grant", []db.RunCapabilityGrant{grant("docker")}, nil, true, false, runProfile{Profile: "restricted", Scopes: []string{}, TaintIDs: []string{}}},
		{"fenced task never re-elevates", []db.RunCapabilityGrant{grant("docker")}, nil, false, true, runProfile{Profile: "restricted", Scopes: []string{}, TaintIDs: []string{}}},
	}
	for _, c := range cases {
		if got := resolveRunProfile(c.grants, c.taint, c.issueTainted, c.fenced); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func insertGrant(t *testing.T, issueID, workspaceID string, scopes []string, createdAgo, expiresIn string, revoked bool) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO run_capability_grant (workspace_id, issue_id, scopes, granted_by_member_id, created_at, expires_at, reason, revoked_at)
		VALUES ($1, $2, $3, gen_random_uuid(), now() - $4::interval, now() + $5::interval, 'test', CASE WHEN $6::boolean THEN now() ELSE NULL END)`,
		workspaceID, issueID, scopes, createdAgo, expiresIn, revoked)
}

func TestRunProfileForTaskReadsOnlyLiveGrantsOfThisIssue(t *testing.T) {
	ctx := context.Background()
	live, expired, revoked, other := dbfx.Issue(t, "profile live"), dbfx.Issue(t, "profile expired"), dbfx.Issue(t, "profile revoked"), dbfx.Issue(t, "profile other")
	insertGrant(t, live, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
	insertGrant(t, expired, testWorkspaceID, []string{"docker"}, "2 hours", "-1 hour", false)
	insertGrant(t, revoked, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", true)
	insertGrant(t, other, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
	foreignWS := dbfx.Workspace(t, "Profile Foreign", "profile-foreign-ws")
	foreign := dbfx.Issue(t, "profile foreign", testutil.Cols{"workspace_id": foreignWS})
	insertGrant(t, foreign, foreignWS, []string{"docker"}, "1 minute", "1 hour", false)

	want := map[string]string{live: "elevated", expired: "restricted", revoked: "restricted"}
	for issue, profile := range want {
		if got := testHandler.runProfileForTask(ctx, parseUUID(testWorkspaceID), parseUUID(issue), nil); got.Profile != profile {
			t.Errorf("issue %s: profile = %q, want %q", issue, got.Profile, profile)
		}
	}
	// The grant of another workspace's issue never covers this workspace.
	if got := testHandler.runProfileForTask(ctx, parseUUID(testWorkspaceID), parseUUID(foreign), nil); got.Profile != "restricted" {
		t.Errorf("cross-workspace grant elevated: %+v", got)
	}
}

func TestClaimCarriesProfileFields(t *testing.T) {
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Profile claim runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Profile claim agent")
	insertGrant(t, issueID, testWorkspaceID, []string{"docker"}, "1 minute", "1 hour", false)
	createDispatchedClaimFixtureTask(t, ctx, agentID, runtimeID, issueID, "120 seconds", false)

	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
	var resp struct {
		Task *struct {
			Profile  string   `json:"profile"`
			Scopes   []string `json:"scopes"`
			TaintIDs []string `json:"taint_ids"`
		} `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&resp)
	if resp.Task == nil || resp.Task.Profile != "elevated" || !reflect.DeepEqual(resp.Task.Scopes, []string{"docker"}) {
		t.Fatalf("claim = %+v, want elevated with scope docker", resp.Task)
	}
}

// Protected-path policy record (plan 3.2): admin-written, never agent-written.

func putPolicy(t *testing.T, name string, body map[string]any, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParams(newRequest("PUT", "/api/workspaces/"+testWorkspaceID+"/protected-path-policies/policy", body), "id", testWorkspaceID, "name", name)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, testHandler.SetProtectedPathPolicy, req)
}

func TestProtectedPathPolicyAdminWritesAndUpserts(t *testing.T) {
	name := "t" + dbfx.Issue(t, "policy name")[:8]
	putPolicy(t, name, map[string]any{"patterns": []string{".github/**"}}, nil).Want(http.StatusOK)
	putPolicy(t, name, map[string]any{"patterns": []string{"**/AGENTS.md", "skills/**"}}, nil).Want(http.StatusOK)
	var patterns []string
	dbfx.QueryRow(t, `SELECT patterns FROM protected_path_policy WHERE workspace_id = $1 AND name = $2`, testWorkspaceID, name).Scan(&patterns)
	if !reflect.DeepEqual(patterns, []string{"**/AGENTS.md", "skills/**"}) {
		t.Fatalf("stored patterns = %v", patterns)
	}
}

func TestProtectedPathPolicyRefusesAgentsAndPlainMembers(t *testing.T) {
	name := "x" + dbfx.Issue(t, "policy refuse")[:8]
	agentID := seededReadyAgentID(t)
	taskID := createHandlerTestTaskForAgent(t, agentID)
	for label, tweak := range map[string]func(*http.Request){
		"member-owned agent": func(r *http.Request) { r.Header.Set("X-Agent-ID", agentID); r.Header.Set("X-Task-ID", taskID) },
		"task token": func(r *http.Request) {
			r.Header.Set("X-Actor-Source", "task_token")
			r.Header.Set("X-Agent-ID", agentID)
		},
	} {
		putPolicy(t, name, map[string]any{"patterns": []string{"a/**"}}, tweak).Want(http.StatusForbidden)
		if n := dbfx.Count(t, `SELECT count(*) FROM protected_path_policy WHERE name = $1`, name); n != 0 {
			t.Fatalf("%s: refused write left %d rows", label, n)
		}
	}
	userID := dbfx.User(t, "Policy Plain", "policy-plain@example.com")
	dbfx.Member(t, testWorkspaceID, userID, "member")
	putPolicy(t, name, map[string]any{"patterns": []string{"a/**"}}, func(r *http.Request) { r.Header.Set("X-User-ID", userID) }).Want(http.StatusForbidden)
}

func TestProtectedPathPolicyValidation(t *testing.T) {
	for label, c := range map[string]struct {
		name string
		body map[string]any
	}{
		"no patterns":    {"p1", map[string]any{"patterns": []string{}}},
		"empty pattern":  {"p1", map[string]any{"patterns": []string{""}}},
		"absolute":       {"p1", map[string]any{"patterns": []string{"/etc/**"}}},
		"parent segment": {"p1", map[string]any{"patterns": []string{"../x"}}},
		"nul byte":       {"p1", map[string]any{"patterns": []string{"a\x00b"}}},
		"too many":       {"p1", map[string]any{"patterns": make([]string, 65)}},
		"unknown field":  {"p1", map[string]any{"patterns": []string{"a"}, "updated_by_member_id": "x"}},
		"bad name":       {"a b", map[string]any{"patterns": []string{"a"}}},
		"traversal name": {"..", map[string]any{"patterns": []string{"a"}}},
	} {
		putPolicy(t, c.name, c.body, nil).Want(http.StatusBadRequest)
		_ = label
	}
}

func TestProtectedPathPatternsFallsBackToInitialPolicy(t *testing.T) {
	got, err := testHandler.protectedPathPatterns(context.Background(), parseUUID(testWorkspaceID), "never-written")
	if err != nil || !reflect.DeepEqual(got, initialProtectedPaths) || len(got) != 10 {
		t.Fatalf("patterns = %v, err = %v", got, err)
	}
}
