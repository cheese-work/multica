package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Browser permission model and admin API (CHE-4737, W3-8; plan CHE-4695 W1 r3, F3). Default
// deny: a profile with no rows grants nothing. There is no egress exception field anywhere.

type browserPermissionResponse struct {
	ProfileID string   `json:"profile_id"`
	Members   []string `json:"members"`
	Agents    []string `json:"agents"`
}

type browserPermissionFixture struct {
	profile string
	agent   string
}

func newBrowserPermissionFixture(t *testing.T) browserPermissionFixture {
	t.Helper()
	runtimeID := dbfx.Runtime(t, "browser-permission runtime")
	agentID := dbfx.Agent(t, "browser-permission agent "+uuid.NewString(), runtimeID)
	prof, err := db.New(testPool).CreateBrowserProfile(context.Background(), db.CreateBrowserProfileParams{
		WorkspaceID: parseUUID(testWorkspaceID), Name: "perm-" + uuid.NewString(), Class: "S2",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"browser_profile_member", "browser_profile_agent", "browser_profile"} {
			testPool.Exec(context.Background(), "DELETE FROM "+tbl+" WHERE workspace_id = $1", testWorkspaceID)
		}
	})
	return browserPermissionFixture{profile: uuid.UUID(prof.ID.Bytes).String(), agent: agentID}
}

func (f browserPermissionFixture) req(method, tail string, kv ...string) *http.Request {
	path := "/api/workspaces/" + testWorkspaceID + "/browser-profiles/" + f.profile + tail
	return withURLParams(newRequest(method, path, nil), append([]string{"id", testWorkspaceID, "profileId", f.profile}, kv...)...)
}

func (f browserPermissionFixture) list(t *testing.T) browserPermissionResponse {
	t.Helper()
	var out browserPermissionResponse
	testutil.Call(t, testHandler.GetBrowserProfilePermissions, f.req(http.MethodGet, "/permissions")).Want(http.StatusOK).JSON(&out)
	return out
}

func TestBrowserPermissionDefaultDeny(t *testing.T) {
	f := newBrowserPermissionFixture(t)
	got := f.list(t)
	if got.ProfileID != f.profile || len(got.Members) != 0 || len(got.Agents) != 0 {
		t.Fatalf("new profile = %+v, want no member and no agent", got)
	}
}

func TestBrowserPermissionGrantListRevokeMember(t *testing.T) {
	f := newBrowserPermissionFixture(t)
	grant := func() {
		t.Helper()
		testutil.Call(t, testHandler.GrantBrowserProfileMember, f.req(http.MethodPut, "/members/"+testUserID, "userId", testUserID)).Want(http.StatusNoContent)
	}
	grant()
	grant() // idempotent
	if got := f.list(t); len(got.Members) != 1 || got.Members[0] != testUserID || len(got.Agents) != 0 {
		t.Fatalf("after grant = %+v, want the one member and no agent", got)
	}
	revoke := func() {
		t.Helper()
		testutil.Call(t, testHandler.RevokeBrowserProfileMember, f.req(http.MethodDelete, "/members/"+testUserID, "userId", testUserID)).Want(http.StatusNoContent)
	}
	revoke()
	revoke() // idempotent
	if got := f.list(t); len(got.Members) != 0 {
		t.Fatalf("after revoke = %+v, want no member", got)
	}
}

func TestBrowserPermissionGrantListRevokeAgent(t *testing.T) {
	f := newBrowserPermissionFixture(t)
	call := func(fn http.HandlerFunc, method string) {
		t.Helper()
		testutil.Call(t, fn, f.req(method, "/agents/"+f.agent, "agentId", f.agent)).Want(http.StatusNoContent)
	}
	call(testHandler.GrantBrowserProfileAgent, http.MethodPut)
	call(testHandler.GrantBrowserProfileAgent, http.MethodPut)
	if got := f.list(t); len(got.Agents) != 1 || got.Agents[0] != f.agent || len(got.Members) != 0 {
		t.Fatalf("after grant = %+v, want the one agent and no member", got)
	}
	call(testHandler.RevokeBrowserProfileAgent, http.MethodDelete)
	call(testHandler.RevokeBrowserProfileAgent, http.MethodDelete)
	if got := f.list(t); len(got.Agents) != 0 {
		t.Fatalf("after revoke = %+v, want no agent", got)
	}
}

// A grant names a real profile, a workspace member and a workspace agent; anything else is 404
// and writes nothing.
func TestBrowserPermissionRefusesUnknownTargets(t *testing.T) {
	f := newBrowserPermissionFixture(t)
	stranger := uuid.NewString()
	testutil.Call(t, testHandler.GrantBrowserProfileMember, f.req(http.MethodPut, "/members/"+stranger, "userId", stranger)).Want(http.StatusNotFound)
	testutil.Call(t, testHandler.GrantBrowserProfileAgent, f.req(http.MethodPut, "/agents/"+stranger, "agentId", stranger)).Want(http.StatusNotFound)
	other := browserPermissionFixture{profile: uuid.NewString(), agent: f.agent}
	testutil.Call(t, testHandler.GrantBrowserProfileAgent, other.req(http.MethodPut, "/agents/"+f.agent, "agentId", f.agent)).Want(http.StatusNotFound)
	testutil.Call(t, testHandler.GetBrowserProfilePermissions, other.req(http.MethodGet, "/permissions")).Want(http.StatusNotFound)
	testutil.Call(t, testHandler.GrantBrowserProfileMember, f.req(http.MethodPut, "/members/not-a-uuid", "userId", "not-a-uuid")).Want(http.StatusBadRequest)
	if got := f.list(t); len(got.Members) != 0 || len(got.Agents) != 0 {
		t.Fatalf("a refused grant wrote a row: %+v", got)
	}
}

// Cheese administers both lists: an agent credential, even on an owner's user id, is refused.
func TestBrowserPermissionRefusesAgentActor(t *testing.T) {
	f := newBrowserPermissionFixture(t)
	req := f.req(http.MethodPut, "/agents/"+f.agent, "agentId", f.agent)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", f.agent)
	testutil.Call(t, testHandler.GrantBrowserProfileAgent, req).Want(http.StatusForbidden)
	req = f.req(http.MethodPut, "/members/"+testUserID, "userId", testUserID)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", f.agent)
	testutil.Call(t, testHandler.GrantBrowserProfileMember, req).Want(http.StatusForbidden)
	if got := f.list(t); len(got.Members) != 0 || len(got.Agents) != 0 {
		t.Fatalf("an agent wrote a permission: %+v", got)
	}
}

// Plan: no exceptions. Neither table nor the response carries an egress, host or allow field.
func TestBrowserPermissionHasNoEgressExceptionField(t *testing.T) {
	f := newBrowserPermissionFixture(t)
	w := httptest.NewRecorder()
	testHandler.GetBrowserProfilePermissions(w, f.req(http.MethodGet, "/permissions"))
	for _, banned := range []string{"egress", "exception", "allow_host", "domain", "cidr", "bypass"} {
		if strings.Contains(strings.ToLower(w.Body.String()), banned) {
			t.Errorf("response mentions %q: %s", banned, w.Body.String())
		}
	}
	rows, err := testPool.Query(context.Background(), `
		SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name IN ('browser_profile_member', 'browser_profile_agent')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			t.Fatal(err)
		}
		seen++
		for _, banned := range []string{"egress", "exception", "allow_host", "domain", "cidr", "bypass"} {
			if strings.Contains(col, banned) {
				t.Errorf("%s.%s: egress exception field", table, col)
			}
		}
	}
	if seen == 0 {
		t.Fatal("permission tables do not exist")
	}
}
