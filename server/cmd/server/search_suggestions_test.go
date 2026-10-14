package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func suggestionsRouter(t *testing.T) http.HandlerFunc {
	t.Helper()
	return NewRouter(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil).ServeHTTP
}

func suggestionsRequest(query, slug, token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/search/suggestions?"+url.Values{"q": {query}, "workspace_slug": {slug}}.Encode(), nil)
	if token != "" {
		request.AddCookie(&http.Cookie{Name: "multica_auth", Value: token})
	}
	return request
}

func suggestionsRows(t *testing.T, response *httptest.ResponseRecorder, query, outcome string, status int) []string {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body %s", response.Code, status, response.Body.String())
	}
	for name, want := range map[string]string{
		"Cache-Control": "no-store", "Vary": "Cookie", "X-Content-Type-Options": "nosniff", "X-Search-Suggestions-Outcome": outcome,
	} {
		if got := response.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if status != 200 {
		return nil
	}
	if response.Header().Get("Content-Type") != "application/x-suggestions+json" {
		t.Errorf("wrong content type: %s", response.Header().Get("Content-Type"))
	}
	var body []json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body) != 4 {
		t.Fatalf("expected four-element suggestions response: %s (%v)", response.Body.String(), err)
	}
	var echoed string
	var rows []string
	json.Unmarshal(body[0], &echoed)
	json.Unmarshal(body[1], &rows)
	if echoed != query || string(body[2]) != "[]" || string(body[3]) != "[]" || string(body[1]) == "null" {
		t.Errorf("wrong response format: %s", response.Body.String())
	}
	return rows
}

func TestSearchSuggestionsAdmission(t *testing.T) {
	for _, testcase := range []struct{ query, enabled, outcome string }{
		{"x", "true", "too_short"}, {"  x ", "true", "too_short"}, {strings.Repeat("x", 201), "true", "too_long"}, {"private", "false", "disabled"},
	} {
		t.Run(testcase.outcome+testcase.query[:1], func(t *testing.T) {
			t.Setenv("SEARCH_SUGGESTIONS_ENABLED", testcase.enabled)
			response := testutil.Call(t, suggestionsRouter(t), suggestionsRequest(testcase.query, integrationTestWorkspaceSlug, "")).ResponseRecorder
			if rows := suggestionsRows(t, response, testcase.query, testcase.outcome, 200); len(rows) != 0 {
				t.Fatalf("admission leaked rows: %v", rows)
			}
		})
	}
}

func TestSearchSuggestionsAuthAndScoping(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	marker := fmt.Sprintf("private%d", time.Now().UnixNano())
	fx.Issue(t, marker)
	foreignSlug := fmt.Sprintf("foreign-%d", time.Now().UnixNano())
	foreignID := fx.Workspace(t, "Foreign", foreignSlug)
	fx.Issue(t, marker+" foreign", testutil.Cols{"workspace_id": foreignID})
	otherEmail := fmt.Sprintf("suggestions-%d@multica.test", time.Now().UnixNano())
	otherUser := fx.User(t, "Other", otherEmail)
	otherToken, err := generateTestJWT(otherUser, otherEmail, "Other")
	if err != nil {
		t.Fatal(err)
	}
	router := suggestionsRouter(t)
	for _, testcase := range []struct {
		name, slug, token, outcome string
		status                     int
	}{
		{"logged out", integrationTestWorkspaceSlug, "", "unauthenticated", 401},
		{"invalid session", integrationTestWorkspaceSlug, "invalid", "unauthenticated", 401},
		{"another workspace", foreignSlug, testToken, "not_member", 404},
		{"another session user", integrationTestWorkspaceSlug, otherToken, "not_member", 404},
		{"missing workspace", "", testToken, "bad_request", 400},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			response := testutil.Call(t, router, suggestionsRequest(marker, testcase.slug, testcase.token)).ResponseRecorder
			suggestionsRows(t, response, marker, testcase.outcome, testcase.status)
			if strings.Contains(response.Body.String(), marker) {
				t.Fatal("rejected request leaked title or query")
			}
		})
	}
	response := testutil.Call(t, router, suggestionsRequest(marker, integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	rows := suggestionsRows(t, response, marker, "rows", 200)
	if len(rows) != 1 || !strings.HasSuffix(rows[0], marker) {
		t.Fatalf("scoped rows = %v", rows)
	}
	var agentID string
	fx.QueryRow(t, `SELECT id FROM agent WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&agentID)
	taskID := fx.Insert(t, "agent_task_queue", testutil.Cols{"agent_id": agentID, "status": "running", "priority": 0, "originator_user_id": testUserID})
	taskToken := mintAgentTaskToken(t, agentID, taskID, testUserID)
	request := suggestionsRequest(marker, integrationTestWorkspaceSlug, "")
	request.Header.Set("Authorization", "Bearer "+taskToken)
	response = testutil.Call(t, router, request).ResponseRecorder
	suggestionsRows(t, response, marker, "forbidden", 403)
}

func TestSearchSuggestionsResults(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	marker := fmt.Sprintf("suggest%d", time.Now().UnixNano())
	fx.Issue(t, marker+" closed", testutil.Cols{"status": "done"})
	fx.Issue(t, "description only", testutil.Cols{"description": marker})
	commentID := fx.Issue(t, "comment only")
	fx.Comment(t, commentID, marker)
	fx.Project(t, "description project", testutil.Cols{"description": marker})
	for index := 0; index < 7; index++ {
		fx.Issue(t, marker+"\n"+strings.Repeat("a", 150))
	}
	for index := 0; index < 4; index++ {
		fx.Project(t, marker+" project")
	}
	query := "  " + marker + "  "
	response := testutil.Call(t, suggestionsRouter(t), suggestionsRequest(query, integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	rows := suggestionsRows(t, response, query, "rows", 200)
	if len(rows) != 8 {
		t.Fatalf("rows = %v, want five issues and three projects", rows)
	}
	for index, row := range rows {
		if strings.ContainsAny(row, "\r\n\t") || len([]rune(row)) > 120 || !strings.Contains(row, marker) {
			t.Errorf("invalid row: %q", row)
		}
		if index >= 5 && row != marker+" project" {
			t.Errorf("project row: %q", row)
		}
	}
}

func TestSearchSuggestionsIdentifierPrefix(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	slug := fmt.Sprintf("digit-prefix-%d", time.Now().UnixNano())
	workspace := fx.Workspace(t, "Digits", slug, testutil.Cols{"issue_prefix": "C9"})
	fx.Member(t, workspace, testUserID, "owner")
	fx.Issue(t, "identifier target", testutil.Cols{"workspace_id": workspace, "number": 123})
	router := suggestionsRouter(t)
	for _, query := range []string{"c9-123", "OTHER-123", "123"} {
		outcome := "rows"
		if query == "OTHER-123" {
			outcome = "empty"
		}
		response := testutil.Call(t, router, suggestionsRequest(query, slug, testToken)).ResponseRecorder
		rows := suggestionsRows(t, response, query, outcome, 200)
		if query != "OTHER-123" && (len(rows) != 1 || rows[0] != "C9-123 identifier target") {
			t.Errorf("identifier rows: %v", rows)
		}
	}
}

func TestSearchSuggestionsRateLimitAndLogPrivacy(t *testing.T) {
	t.Setenv("RATE_LIMIT_SEARCH_SUGGESTIONS", "1")
	router := suggestionsRouter(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	query := "sensitive-marker-do-not-log"
	first := testutil.Call(t, router, suggestionsRequest(query, integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	suggestionsRows(t, first, query, "empty", 200)
	second := testutil.Call(t, router, suggestionsRequest(query, integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	suggestionsRows(t, second, query, "throttled", 200)
	if second.Header().Get("Retry-After") == "" {
		t.Error("no retry hint")
	}
	if strings.Contains(logs.String(), query) || strings.Contains(logs.String(), testToken) {
		t.Fatal("request data leaked in logs")
	}
	if !strings.Contains(logs.String(), "outcome=throttled") {
		t.Fatalf("missing outcome log: %s", logs.String())
	}
}

func TestSearchSuggestionsTimeoutReleasesCapacity(t *testing.T) {
	t.Setenv("SEARCH_SUGGESTIONS_MAX_CONCURRENT", "1")
	router := suggestionsRouter(t)
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	if _, err := tx.Exec(context.Background(), `LOCK TABLE issue IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response := testutil.Call(t, router, suggestionsRequest("locked-search", integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	suggestionsRows(t, response, "locked-search", "timeout", 200)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	tx.Rollback(context.Background())
	response = testutil.Call(t, router, suggestionsRequest("released-search", integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	suggestionsRows(t, response, "released-search", "empty", 200)
}
