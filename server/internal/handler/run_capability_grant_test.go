package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// run_capability_grant (CHE-2216 plan 3.1, slice A1a): only a person who owns
// or administers the workspace writes a grant. Every agent credential gets 403,
// including the token of an agent a member owns.

const sha40 = "0123456789abcdef0123456789abcdef01234567"

func postGrant(t *testing.T, issueID string, body map[string]any, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("POST", "/api/issues/"+issueID+"/capability-grants", body), "id", issueID)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, testHandler.CreateRunCapabilityGrant, req)
}

func validGrantBody() map[string]any {
	return map[string]any{
		"scopes":     []string{"protected_paths:default", "repo_write:129714106", "multica_cross_issue", "device:pixel-8", "docker"},
		"expires_at": time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"reason":     "CHE-2489 test grant",
	}
}

func grantRows(t *testing.T, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM run_capability_grant WHERE issue_id = $1`, issueID)
}

func TestRunCapabilityGrantMemberWritesOne(t *testing.T) {
	issueID := dbfx.Issue(t, "grant target")
	body := validGrantBody()
	body["pinned_base_sha"] = sha40
	var got struct {
		ID        string   `json:"id"`
		IssueID   string   `json:"issue_id"`
		Scopes    []string `json:"scopes"`
		GrantedBy string   `json:"granted_by_member_id"`
	}
	postGrant(t, issueID, body, nil).Want(http.StatusCreated).JSON(&got)
	if got.ID == "" || got.IssueID != issueID || len(got.Scopes) != 5 {
		t.Fatalf("response = %+v", got)
	}
	var memberID string
	dbfx.QueryRow(t, `SELECT id FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, testUserID).Scan(&memberID)
	if got.GrantedBy != memberID {
		t.Fatalf("granted_by_member_id = %q, want the calling member %q", got.GrantedBy, memberID)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM run_capability_grant WHERE id = $1 AND workspace_id = $2 AND granted_by_member_id = $3 AND pinned_base_sha = $4 AND revoked_at IS NULL`,
		got.ID, testWorkspaceID, memberID, sha40); n != 1 {
		t.Fatalf("stored grant rows = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'run_capability_granted' AND actor_type = 'member' AND actor_id = $2`, issueID, testUserID); n != 1 {
		t.Fatalf("run_capability_granted activity rows = %d, want 1", n)
	}
}

func TestRunCapabilityGrantRefusesEveryAgentCredential(t *testing.T) {
	issueID := dbfx.Issue(t, "grant target agent")
	agentID := seededReadyAgentID(t)
	taskID := createHandlerTestTaskForAgent(t, agentID)
	cases := map[string]func(*http.Request){
		"member-owned agent with task headers": func(r *http.Request) {
			r.Header.Set("X-Agent-ID", agentID)
			r.Header.Set("X-Task-ID", taskID)
		},
		"task token": func(r *http.Request) {
			r.Header.Set("X-Actor-Source", "task_token")
			r.Header.Set("X-Agent-ID", agentID)
		},
		"cloud pat": func(r *http.Request) { r.Header.Set("X-Actor-Source", "cloud_pat") },
	}
	for name, tweak := range cases {
		postGrant(t, issueID, validGrantBody(), tweak).Want(http.StatusForbidden)
		if n := grantRows(t, issueID); n != 0 {
			t.Fatalf("%s: refused write left %d rows", name, n)
		}
	}
}

func TestRunCapabilityGrantRefusesPlainMember(t *testing.T) {
	issueID := dbfx.Issue(t, "grant target plain member")
	userID := dbfx.User(t, "Plain Member", "plain-member-grant@example.com")
	dbfx.Member(t, testWorkspaceID, userID, "member")
	postGrant(t, issueID, validGrantBody(), func(r *http.Request) { r.Header.Set("X-User-ID", userID) }).Want(http.StatusForbidden)
	if n := grantRows(t, issueID); n != 0 {
		t.Fatalf("refused write left %d rows", n)
	}
}

func TestRunCapabilityGrantValidation(t *testing.T) {
	issueID := dbfx.Issue(t, "grant target validation")
	mutate := func(k string, v any) map[string]any { b := validGrantBody(); b[k] = v; return b }
	cases := map[string]map[string]any{
		"no scopes":           mutate("scopes", []string{}),
		"unknown scope":       mutate("scopes", []string{"shell"}),
		"free text scope":     mutate("scopes", []string{"docker please"}),
		"non-numeric repo":    mutate("scopes", []string{"repo_write:multica"}),
		"leading-zero repo":   mutate("scopes", []string{"repo_write:012"}),
		"empty device name":   mutate("scopes", []string{"device:"}),
		"path in policy name": mutate("scopes", []string{"protected_paths:../x"}),
		"too many scopes": mutate("scopes", func() []string {
			s := make([]string, 17)
			for i := range s {
				s[i] = "device:d" + string(rune('a'+i))
			}
			return s
		}()),
		"expiry in the past":   mutate("expires_at", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)),
		"expiry over 24 hours": mutate("expires_at", time.Now().Add(25*time.Hour).UTC().Format(time.RFC3339)),
		"expiry missing":       mutate("expires_at", nil),
		"expiry malformed":     mutate("expires_at", "tomorrow"),
		"empty reason":         mutate("reason", " "),
		"reason too long":      mutate("reason", string(make([]byte, 1001))),
		"short pinned base":    mutate("pinned_base_sha", "abc123"),
		"upper pinned head":    mutate("pinned_head_sha", "0123456789ABCDEF0123456789ABCDEF01234567"),
		"unknown field":        mutate("granted_by_member_id", "00000000-0000-0000-0000-000000000000"),
	}
	for name, body := range cases {
		postGrant(t, issueID, body, nil).Want(http.StatusBadRequest)
		if n := grantRows(t, issueID); n != 0 {
			t.Fatalf("%s: rejected write left %d rows", name, n)
		}
	}
}

func TestRunCapabilityGrantIssueFromAnotherWorkspaceIs404(t *testing.T) {
	otherWS := dbfx.Workspace(t, "Grant Other", "grant-other-ws")
	otherIssue := dbfx.Issue(t, "foreign issue", testutil.Cols{"workspace_id": otherWS})
	postGrant(t, otherIssue, validGrantBody(), nil).Want(http.StatusNotFound)
	if n := grantRows(t, otherIssue); n != 0 {
		t.Fatalf("cross-workspace write left %d rows", n)
	}
}
