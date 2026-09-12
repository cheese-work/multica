package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestGitHubMultipleInstallationsRemainIndependent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}

	ctx := context.Background()
	const (
		secret             = "multi-installation-regression-secret"
		firstInstallation  = int64(82680001)
		secondInstallation = int64(82680002)
	)
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	t.Setenv("GITHUB_APP_CLIENT_ID", "multi-installation-client")
	t.Setenv("GITHUB_APP_CLIENT_SECRET", "multi-installation-client-secret")
	t.Setenv("GITHUB_APP_SLUG", "multica-test")
	pemBytes, _ := generateTestRSAKeyPEM(t)
	t.Setenv("GITHUB_APP_ID", "268")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pemBytes))
	t.Setenv("FRONTEND_ORIGIN", "https://app.example.test")
	githubAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/login/oauth/access_token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			code := r.Form.Get("code")
			if code != "first" && code != "second" {
				http.Error(w, "bad code", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token-" + code})
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/app/installations/%d", firstInstallation):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      firstInstallation,
				"account": map[string]any{"id": 1001, "login": "che-268-first", "type": "User"},
			})
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/app/installations/%d", secondInstallation):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      secondInstallation,
				"account": map[string]any{"id": 1002, "login": "che-268-second", "type": "Organization"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/user" && authorization == "Bearer token-first":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001, "login": "CHE-268-FIRST"})
		case r.Method == http.MethodGet && r.URL.Path == "/user" && authorization == "Bearer token-second":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2002, "login": "org-owner"})
		case r.Method == http.MethodGet && r.URL.Path == "/user/memberships/orgs/che-268-second":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "active", "role": "admin"})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/applications/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(githubAPI.Close)
	oldAPIBase, oldOAuthBase := githubAPIBase, githubOAuthBase
	githubAPIBase, githubOAuthBase = githubAPI.URL, githubAPI.URL
	t.Cleanup(func() { githubAPIBase, githubOAuthBase = oldAPIBase, oldOAuthBase })

	createIssue := func(title string) IssueResponse {
		t.Helper()
		req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
			"title":  title,
			"status": "in_progress",
		})
		rec := testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated)
		var issue IssueResponse
		if err := json.NewDecoder(rec.Body).Decode(&issue); err != nil {
			t.Fatalf("decode issue: %v", err)
		}
		return issue
	}
	firstIssue := createIssue("CHE-268 first GitHub installation regression")
	secondIssue := createIssue("CHE-268 second GitHub installation regression")

	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM issue_pull_request WHERE issue_id IN ($1, $2)`, firstIssue.ID, secondIssue.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1 AND repo_owner = 'che-268'`, testWorkspaceID)
		_, _ = testPool.Exec(ctx, `DELETE FROM github_installation WHERE workspace_id = $1 AND installation_id IN ($2, $3)`, testWorkspaceID, firstInstallation, secondInstallation)
		_, _ = testPool.Exec(ctx, `DELETE FROM activity_log WHERE issue_id IN ($1, $2)`, firstIssue.ID, secondIssue.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id IN ($1, $2)`, firstIssue.ID, secondIssue.ID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE id IN ($1, $2)`, firstIssue.ID, secondIssue.ID)
	})

	router := chi.NewRouter()
	router.Route("/api/workspaces/{id}", func(r chi.Router) {
		r.Delete("/github/installations/{installationId}", testHandler.DeleteGitHubInstallation)
	})

	install := func(installationID int64, code string) db.GithubInstallation {
		t.Helper()
		state, err := signStateForReturn(testWorkspaceID, githubReturnToRepositories)
		if err != nil {
			t.Fatalf("sign installation state: %v", err)
		}
		callbackReq := httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf(
				"/api/github/setup?installation_id=%d&code=%s&state=%s",
				installationID,
				url.QueryEscape(code),
				url.QueryEscape(state),
			),
			nil,
		)
		callbackRec := testutil.Call(t, testHandler.GitHubSetupCallback, callbackReq).Want(http.StatusFound)
		if location := callbackRec.Header().Get("Location"); !strings.Contains(location, "github_connected=1") {
			t.Fatalf("installation callback for %d did not succeed: %s", installationID, location)
		}

		rows, err := testHandler.Queries.ListGitHubInstallationsByWorkspace(ctx, parseUUID(testWorkspaceID))
		if err != nil {
			t.Fatalf("list installations after installing %d: %v", installationID, err)
		}
		for _, row := range rows {
			if row.InstallationID == installationID {
				location, err := url.Parse(callbackRec.Header().Get("Location"))
				if err != nil {
					t.Fatalf("parse callback redirect for %d: %v", installationID, err)
				}
				if got := location.Query().Get("github_installation"); got != uuidToString(row.ID) {
					t.Fatalf("callback selected installation row = %q, want %q", got, uuidToString(row.ID))
				}
				return row
			}
		}
		t.Fatalf("installation %d was not persisted", installationID)
		return db.GithubInstallation{}
	}

	first := install(firstInstallation, "first")
	second := install(secondInstallation, "second")

	postPullRequest := func(installationID int64, repository string, number int, title string) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"action": "opened",
			"pull_request": map[string]any{
				"number":     number,
				"html_url":   fmt.Sprintf("https://github.com/che-268/%s/pull/%d", repository, number),
				"title":      title,
				"body":       "",
				"state":      "open",
				"draft":      false,
				"merged":     false,
				"created_at": "2026-09-07T00:00:00Z",
				"updated_at": "2026-09-07T00:00:00Z",
				"head":       map[string]any{"ref": "feature", "sha": fmt.Sprintf("sha-%d", number)},
				"user":       map[string]any{"login": "octocat", "avatar_url": ""},
			},
			"repository": map[string]any{
				"name":  repository,
				"owner": map[string]any{"login": "che-268"},
			},
			"installation": map[string]any{"id": installationID},
		})
		if err != nil {
			t.Fatalf("marshal webhook: %v", err)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		req := httptest.NewRequest(http.MethodPost, "/api/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		testutil.Call(t, testHandler.HandleGitHubWebhook, req).Want(http.StatusAccepted)
	}

	assertPullRequest := func(repository string, number int, want bool) {
		t.Helper()
		var count int
		err := testPool.QueryRow(ctx, `
			SELECT count(*) FROM github_pull_request
			WHERE workspace_id = $1 AND repo_owner = 'che-268' AND repo_name = $2 AND pr_number = $3
		`, testWorkspaceID, repository, number).Scan(&count)
		if err != nil {
			t.Fatalf("query pull request %s#%d: %v", repository, number, err)
		}
		if got := count == 1; got != want {
			t.Fatalf("pull request %s#%d present = %v, want %v", repository, number, got, want)
		}
	}
	assertLinked := func(repository string, issueID string) {
		t.Helper()
		var count int
		err := testPool.QueryRow(ctx, `
			SELECT count(*)
			FROM issue_pull_request link
			JOIN github_pull_request pr ON pr.id = link.pull_request_id
			WHERE link.issue_id = $1 AND pr.workspace_id = $2
			  AND pr.repo_owner = 'che-268' AND pr.repo_name = $3
		`, issueID, testWorkspaceID, repository).Scan(&count)
		if err != nil {
			t.Fatalf("query pull request link for %s: %v", repository, err)
		}
		if count != 1 {
			t.Fatalf("pull request %s links = %d, want 1", repository, count)
		}
	}

	postPullRequest(firstInstallation, "first-before-disconnect", 101, "Fix "+firstIssue.Identifier)
	postPullRequest(secondInstallation, "second-before-disconnect", 201, "Fix "+secondIssue.Identifier)
	assertPullRequest("first-before-disconnect", 101, true)
	assertPullRequest("second-before-disconnect", 201, true)
	assertLinked("first-before-disconnect", firstIssue.ID)
	assertLinked("second-before-disconnect", secondIssue.ID)

	deleteReq := httptest.NewRequest(
		http.MethodDelete,
		"/api/workspaces/"+testWorkspaceID+"/github/installations/"+uuidToString(second.ID),
		nil,
	)
	deleteRec := httptest.NewRecorder()
	router.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("disconnect second installation: status %d, body %s", deleteRec.Code, deleteRec.Body.String())
	}

	rows, err := testHandler.Queries.ListGitHubInstallationsByWorkspace(ctx, parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatalf("list installations after disconnect: %v", err)
	}
	if len(rows) != 1 || rows[0].InstallationID != firstInstallation || rows[0].ID != first.ID {
		t.Fatalf("disconnect removed the wrong binding: got %+v, want only installation %d", rows, firstInstallation)
	}

	postPullRequest(firstInstallation, "first-after-disconnect", 102, "PR 102")
	postPullRequest(secondInstallation, "second-after-disconnect", 202, "PR 202")
	assertPullRequest("first-after-disconnect", 102, true)
	assertPullRequest("second-after-disconnect", 202, false)
}
