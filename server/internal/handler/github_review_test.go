package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/yuin/goldmark"
)

type githubReviewTransport func(*http.Request) (*http.Response, error)

func (transport githubReviewTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type githubReviewFixture struct {
	handler *Handler
	issueID string
	author  string
	other   string
	source  string
	secret  string
	payload map[string]any
}

func newGitHubReviewFixture(t *testing.T) githubReviewFixture {
	t.Helper()
	if testHandler == nil {
		t.Fatal("database required for review-to-run acceptance")
	}
	const installationID int64 = 2019001
	fixture := newPRMergeWebhookFixture(t, installationID)
	author := createHandlerTestAgent(t, "github-review-author", nil)
	other := createHandlerTestAgent(t, "github-review-assignee", nil)
	setIssueAssigneeDirect(t, fixture.child.ID, "agent", other)
	sourceTaskID := dbfx.Task(t, author, testutil.Cols{"issue_id": fixture.parent.ID, "runtime_id": handlerTestRuntimeID(t), "status": "completed"})
	taskKey := strings.ReplaceAll(sourceTaskID, "-", "")
	pull := linkedPRForWakeup(t, fixture, installationID, 2019, "review-head")
	dbfx.Exec(t, `UPDATE github_pull_request SET branch=$1 WHERE id=$2`, "agent/github-review-author/che-2019-"+taskKey[len(taskKey)-12:], pull.ID)
	workspace, err := testHandler.Queries.GetWorkspace(context.Background(), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings=$1 WHERE id=$2`, workspace.Settings, testWorkspaceID)
	})
	settings, _ := json.Marshal(map[string]any{"github_review_members": map[string]string{"42": testUserID}})
	dbfx.Exec(t, `UPDATE workspace SET settings=$1 WHERE id=$2`, settings, testWorkspaceID)
	key, _ := generateTestRSAKeyPEM(t)
	t.Setenv("GITHUB_APP_ID", "123")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(key))
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	http.DefaultTransport = githubReviewTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "api.github.com" {
			return nil, fmt.Errorf("unexpected API host: %s", request.URL.Host)
		}
		status := http.StatusOK
		var body string
		switch request.URL.Path {
		case "/app/installations/2019001/access_tokens":
			status = http.StatusCreated
			body = `{"token":"test-installation-token","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
		case "/repos/acme/widget/pulls/2019/reviews/77/comments", "/repos/acme/widget/pulls/2019/reviews/78/comments":
			if request.Header.Get("Authorization") != "Bearer test-installation-token" {
				return nil, fmt.Errorf("review comments request was not installation-authenticated")
			}
			body = `[{"id":1,"path":"server.go","line":12,"body":"Handle the error. [@Other](mention://agent/` + other + `)","html_url":"https://github.com/acme/widget/pull/2019#discussion_r1"}]`
		default:
			return nil, fmt.Errorf("unexpected API request: %s", request.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	client, err := ghsnapshot.NewClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	handler := *testHandler
	handler.PRRefresh = ghsnapshot.NewManager(client, handler.Queries, testPool, nil)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE issue_id=$1`, fixture.child.ID)
		dbfx.Exec(t, `DELETE FROM comment WHERE issue_id=$1`, fixture.child.ID)
	})
	return githubReviewFixture{
		handler: &handler, issueID: fixture.child.ID, author: author, other: other, source: sourceTaskID, secret: fixture.secret,
		payload: map[string]any{
			"action": "submitted", "installation": map[string]any{"id": installationID},
			"repository":   githubRepositoryForTest("acme", "widget"),
			"pull_request": map[string]any{"number": 2019, "head": map[string]any{"repo": githubRepositoryForTest("acme", "widget")}},
			"review": map[string]any{
				"id": 77, "state": "changes_requested", "body": "Please fix the error path.",
				"html_url": "https://github.com/acme/widget/pull/2019#pullrequestreview-77",
				"user":     map[string]any{"id": 42, "login": "reviewer", "type": "User"},
			},
		},
	}
}

func TestGitHubPRReviewIntakeDoesNotWakeRenamedAuthor(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	dbfx.Exec(t, `UPDATE agent SET name='Renamed PR Author' WHERE id=$1`, fixture.author)
	if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"-renamed-author"); response.Code != http.StatusAccepted {
		t.Fatalf("renamed author review: %d %s", response.Code, response.Body.String())
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubPRReviewIntakeDoesNotWakeCustomBranchAuthor(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	dbfx.Exec(t, `UPDATE github_pull_request SET branch='che-2012/revenue-events' WHERE workspace_id=$1`, testWorkspaceID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET issue_id=$1, branch_name='che-2012/revenue-events' WHERE id=$2`, fixture.issueID, fixture.source)
	if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"-custom-branch"); response.Code != http.StatusAccepted {
		t.Fatalf("custom branch review: %d %s", response.Code, response.Body.String())
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubPRReviewIntakeDoesNotWakeExplicitAuthorBinding(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	dbfx.Exec(t, `UPDATE github_pull_request SET branch='che-2012/revenue-events' WHERE workspace_id=$1`, testWorkspaceID)
	dbfx.Exec(t, `UPDATE workspace SET settings=jsonb_set(settings,'{github_review_authors}',jsonb_build_object('acme/widget#2019',$1::text)) WHERE id=$2`, fixture.author, testWorkspaceID)
	if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"-explicit-author-binding"); response.Code != http.StatusAccepted {
		t.Fatalf("explicit author review: %d %s", response.Code, response.Body.String())
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func (fixture githubReviewFixture) post(event, secret, delivery string) *httptest.ResponseRecorder {
	return fixture.postContext(context.Background(), event, secret, delivery)
}

func (fixture githubReviewFixture) postContext(ctx context.Context, event, secret, delivery string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(fixture.payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	request := httptest.NewRequest(http.MethodPost, "/api/webhooks/github", bytes.NewReader(body))
	request = request.WithContext(ctx)
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-GitHub-Delivery", delivery)
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response := httptest.NewRecorder()
	fixture.handler.HandleGitHubWebhook(response, request)
	return response
}

func TestGitHubPRReviewIntakeStaysDeferredAcrossRedelivery(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	for delivery := 0; delivery < 2; delivery++ {
		response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+fmt.Sprint(delivery))
		if response.Code != http.StatusAccepted {
			t.Fatalf("review webhook: %d %s", response.Code, response.Body.String())
		}
	}
	assertGitHubIntakeNoEffects(t, fixture)
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1 AND state='deferred'`, testWorkspaceID); count != 1 {
		t.Fatalf("pending revision rows = %d, want 1", count)
	}
}

func TestGitHubPRReviewIntakeDoesNotFetchExternalText(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	fixture.handler.PRRefresh = nil
	http.DefaultTransport = githubReviewTransport(func(request *http.Request) (*http.Response, error) {
		t.Errorf("intake fetched GitHub text: %s", request.URL.Path)
		return nil, fmt.Errorf("no GitHub API calls during intake")
	})
	if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"-no-api"); response.Code != http.StatusAccepted {
		t.Fatalf("intake response = %d %s", response.Code, response.Body.String())
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubPRReviewIntakeRetainsDistinctReviews(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	for _, reviewID := range []int64{77, 78, 77, 78} {
		fixture.payload["review"].(map[string]any)["id"] = reviewID
		if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+fmt.Sprint(reviewID)); response.Code != http.StatusAccepted {
			t.Fatalf("review %d: %d %s", reviewID, response.Code, response.Body.String())
		}
	}
	assertGitHubIntakeNoEffects(t, fixture)
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID); count != 2 {
		t.Fatalf("distinct review revisions = %d, want 2", count)
	}
}

func TestGitHubPRReviewSubmittedConcurrentRedelivery(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	config := testPool.Config().Copy()
	config.MaxConns = 1
	config.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	queries := db.New(pool)
	fixture.handler.Queries = queries
	fixture.handler.TxStarter = pool
	fixture.handler.TaskService = service.NewTaskService(queries, pool, fixture.handler.Hub, fixture.handler.Bus, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	statuses := make(chan int, 6)
	for delivery := 0; delivery < 6; delivery++ {
		workers.Go(func() {
			statuses <- fixture.postContext(ctx, "pull_request_review", fixture.secret, fixture.issueID+"-concurrent").Code
		})
	}
	workers.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusAccepted {
			t.Errorf("concurrent review response = %d", status)
		}
	}
	assertGitHubIntakeNoEffects(t, fixture)
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID); count != 1 {
		t.Fatalf("concurrent review revisions = %d, want 1", count)
	}
}

func TestGitHubPRReviewContentDoesNotRenderExternalMentionChips(t *testing.T) {
	payload := &ghReviewPayload{}
	payload.Review.Body = "[@Other](mention://agent/other)"
	content := githubReviewContent("[@Author](mention://agent/author)", db.GithubPullRequest{}, payload,
		[]ghsnapshot.ReviewComment{{Body: "[@Inline](mention://agent/inline)"}})
	var rendered bytes.Buffer
	if err := goldmark.Convert([]byte(content), &rendered); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.String(), `href="mention://agent/author"`) {
		t.Fatal("trusted author mention did not render")
	}
	for _, target := range []string{"other", "inline"} {
		if strings.Contains(rendered.String(), `href="mention://agent/`+target+`"`) {
			t.Errorf("external %s mention rendered as a chip link: %s", target, rendered.String())
		}
	}
}

func TestGitHubLegacyReviewDispatchHelper(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	body, err := json.Marshal(fixture.payload)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := fixture.handler.handlePullRequestReviewEvent(context.Background(), body); err != nil {
			t.Fatal(err)
		}
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID); count != 1 {
		t.Fatalf("isolated legacy helper dispatches = %d, want 1", count)
	}
}

func TestGitHubPRReviewSubmittedRejectsUntrustedReviews(t *testing.T) {
	for _, scenario := range []string{"bot", "unmapped-user", "departed-member", "edited", "inline-comment", "bad-signature", "unlinked", "integration-disabled", "unknown-author", "unknown-task", "wrong-installation", "malformed-member-map", "missing-secret", "ambiguous-author", "invalid-author-binding"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newGitHubIntakeFixture(t)
			event, secret, expectedStatus := "pull_request_review", fixture.secret, http.StatusAccepted
			switch scenario {
			case "bot":
				fixture.payload["review"].(map[string]any)["user"].(map[string]any)["type"] = "Bot"
			case "unmapped-user":
				fixture.payload["review"].(map[string]any)["user"].(map[string]any)["id"] = 99
			case "departed-member":
				dbfx.Exec(t, `UPDATE workspace SET settings='{"github_review_members":{"42":"00000000-0000-0000-0000-000000000042"}}' WHERE id=$1`, testWorkspaceID)
			case "edited":
				fixture.payload["action"] = "edited"
			case "inline-comment":
				event = "pull_request_review_comment"
			case "bad-signature":
				secret, expectedStatus = "invalid-test-secret", http.StatusUnauthorized
			case "unlinked":
				dbfx.Exec(t, `DELETE FROM issue_pull_request WHERE issue_id=$1`, fixture.issueID)
			case "integration-disabled":
				dbfx.Exec(t, `UPDATE workspace SET settings=settings || '{"github_enabled":false}'::jsonb WHERE id=$1`, testWorkspaceID)
			case "unknown-author":
				dbfx.Exec(t, `UPDATE github_pull_request SET branch='feature/unattributed' WHERE workspace_id=$1`, testWorkspaceID)
			case "unknown-task":
				dbfx.Exec(t, `UPDATE github_pull_request SET branch='agent/github-review-author/not-a-task' WHERE workspace_id=$1`, testWorkspaceID)
			case "wrong-installation":
				fixture.payload["installation"].(map[string]any)["id"] = 123456789
			case "malformed-member-map":
				dbfx.Exec(t, `UPDATE workspace SET settings='{"github_review_members":[42]}' WHERE id=$1`, testWorkspaceID)
			case "missing-secret":
				t.Setenv("GITHUB_WEBHOOK_SECRET", "")
				expectedStatus = http.StatusNotFound
			case "ambiguous-author":
				dbfx.Exec(t, `UPDATE github_pull_request SET branch='feature/ambiguous', pr_created_at=now() WHERE workspace_id=$1`, testWorkspaceID)
				dbfx.Exec(t, `UPDATE agent_task_queue SET issue_id=$1, branch_name='feature/ambiguous' WHERE id=$2`, fixture.issueID, fixture.source)
				dbfx.Task(t, fixture.other, testutil.Cols{"issue_id": fixture.issueID, "runtime_id": handlerTestRuntimeID(t), "branch_name": "feature/ambiguous", "status": "completed", "created_at": time.Now().Add(-time.Minute)})
			case "invalid-author-binding":
				dbfx.Exec(t, `UPDATE workspace SET settings=settings || '{"github_review_authors":{"acme/widget#2019":"invalid"}}'::jsonb WHERE id=$1`, testWorkspaceID)
			}
			response := fixture.post(event, secret, fixture.issueID+scenario)
			if response.Code != expectedStatus {
				t.Fatalf("response = %d %s, want %d", response.Code, response.Body.String(), expectedStatus)
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'github_review_key' IS NOT NULL`, fixture.issueID); count != 0 {
				t.Fatalf("untrusted review created %d runs", count)
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, fixture.issueID); count != 0 {
				t.Fatalf("untrusted review created %d comments", count)
			}
		})
	}
}
