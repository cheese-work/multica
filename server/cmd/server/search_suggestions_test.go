package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func suggestionsRouter(test *testing.T) http.HandlerFunc {
	test.Helper()
	return NewRouter(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil).ServeHTTP
}

func suggestionsRequest(query, slug, token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/search/suggestions?"+url.Values{"q": {query}, "workspace_slug": {slug}}.Encode(), nil)
	if token != "" {
		request.AddCookie(&http.Cookie{Name: "multica_auth", Value: token})
	}
	return request
}

func suggestionsRows(test *testing.T, response *httptest.ResponseRecorder, query, outcome string, status int) []string {
	test.Helper()
	if response.Code != status {
		test.Fatalf("status = %d, want %d; body %s", response.Code, status, response.Body.String())
	}
	for name, want := range map[string]string{
		"Cache-Control": "no-store", "Vary": "Cookie", "X-Content-Type-Options": "nosniff", "X-Search-Suggestions-Outcome": outcome,
	} {
		if got := response.Header().Get(name); got != want {
			test.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if status != 200 {
		return nil
	}
	if response.Header().Get("Content-Type") != "application/x-suggestions+json" {
		test.Errorf("wrong content type: %s", response.Header().Get("Content-Type"))
	}
	var body []json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body) != 4 {
		test.Fatalf("expected four-element suggestions response: %s (%v)", response.Body.String(), err)
	}
	var echoed string
	var rows []string
	json.Unmarshal(body[0], &echoed)
	json.Unmarshal(body[1], &rows)
	if echoed != query || string(body[2]) != "[]" || string(body[3]) != "[]" || string(body[1]) == "null" {
		test.Errorf("wrong response format: %s", response.Body.String())
	}
	return rows
}

func TestSearchSuggestionsAdmission(test *testing.T) {
	for _, testcase := range []struct{ query, enabled, outcome string }{
		{"x", "true", "too_short"}, {"  x ", "true", "too_short"}, {strings.Repeat("x", 201), "true", "too_long"}, {"private", "false", "disabled"},
	} {
		test.Run(testcase.outcome+testcase.query[:1], func(test *testing.T) {
			test.Setenv("SEARCH_SUGGESTIONS_ENABLED", testcase.enabled)
			response := testutil.Call(test, suggestionsRouter(test), suggestionsRequest(testcase.query, integrationTestWorkspaceSlug, "")).ResponseRecorder
			if rows := suggestionsRows(test, response, testcase.query, testcase.outcome, 200); len(rows) != 0 {
				test.Fatalf("admission leaked rows: %v", rows)
			}
		})
	}
}

func TestSearchSuggestionsAuthAndScoping(test *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	marker := fmt.Sprintf("private%d", time.Now().UnixNano())
	fx.Issue(test, marker)
	foreignSlug := fmt.Sprintf("foreign-%d", time.Now().UnixNano())
	foreignID := fx.Workspace(test, "Foreign", foreignSlug)
	fx.Issue(test, marker+" foreign", testutil.Cols{"workspace_id": foreignID})
	otherEmail := fmt.Sprintf("suggestions-%d@multica.test", time.Now().UnixNano())
	otherUser := fx.User(test, "Other", otherEmail)
	otherToken, err := generateTestJWT(otherUser, otherEmail, "Other")
	if err != nil {
		test.Fatal(err)
	}
	router := suggestionsRouter(test)
	expiredToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": testUserID, "exp": time.Now().Add(-time.Hour).Unix()}).SignedString(auth.JWTSecret())
	if err != nil {
		test.Fatal(err)
	}
	for _, testcase := range []struct {
		name, slug, token, outcome string
		status                     int
	}{
		{"logged out", integrationTestWorkspaceSlug, "", "unauthenticated", 401},
		{"invalid session", integrationTestWorkspaceSlug, "invalid", "unauthenticated", 401},
		{"expired session", integrationTestWorkspaceSlug, expiredToken, "unauthenticated", 401},
		{"another workspace", foreignSlug, testToken, "not_member", 404},
		{"another session user", integrationTestWorkspaceSlug, otherToken, "not_member", 404},
		{"missing workspace", "", testToken, "bad_request", 400},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			response := testutil.Call(test, router, suggestionsRequest(marker, testcase.slug, testcase.token)).ResponseRecorder
			suggestionsRows(test, response, marker, testcase.outcome, testcase.status)
			if strings.Contains(response.Body.String(), marker) {
				test.Fatal("rejected request leaked title or query")
			}
		})
	}
	response := testutil.Call(test, router, suggestionsRequest(marker, integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	rows := suggestionsRows(test, response, marker, "rows", 200)
	if len(rows) != 1 || !strings.HasSuffix(rows[0], marker) {
		test.Fatalf("scoped rows = %v", rows)
	}
	var agentID, runtimeID string
	fx.QueryRow(test, `SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND runtime_id IS NOT NULL LIMIT 1`, testWorkspaceID).Scan(&agentID, &runtimeID)
	taskID := fx.Insert(test, "agent_task_queue", testutil.Cols{"agent_id": agentID, "runtime_id": runtimeID, "status": "running", "priority": 0, "originator_user_id": testUserID, "accountable_user_id": testUserID, "originator_source": "direct_human"})
	taskToken := mintAgentTaskToken(test, agentID, taskID, testUserID)
	request := suggestionsRequest(marker, integrationTestWorkspaceSlug, "")
	request.Header.Set("Authorization", "Bearer "+taskToken)
	response = testutil.Call(test, router, request).ResponseRecorder
	suggestionsRows(test, response, marker, "forbidden", 403)
	pat := "mul_suggestions_human_token_" + marker
	fx.Insert(test, "personal_access_token", testutil.Cols{"user_id": testUserID, "name": "Suggestions test", "token_hash": auth.HashToken(pat), "token_prefix": pat[:8]})
	request = suggestionsRequest(marker, integrationTestWorkspaceSlug, "")
	request.Header.Set("Authorization", "Bearer "+pat)
	response = testutil.Call(test, router, request).ResponseRecorder
	suggestionsRows(test, response, marker, "rows", 200)
	request = suggestionsRequest(marker, foreignSlug, testToken)
	request.Header.Set("X-Workspace-Slug", integrationTestWorkspaceSlug)
	response = testutil.Call(test, router, request).ResponseRecorder
	suggestionsRows(test, response, marker, "not_member", 404)
}

func TestSearchSuggestionsResults(test *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	marker := fmt.Sprintf("suggest%d", time.Now().UnixNano())
	fx.Issue(test, marker+" closed", testutil.Cols{"status": "done"})
	fx.Issue(test, "description only", testutil.Cols{"description": marker})
	commentID := fx.Issue(test, "comment only")
	fx.Comment(test, commentID, marker)
	fx.Project(test, "description project", testutil.Cols{"description": marker})
	for index := 0; index < 7; index++ {
		fx.Issue(test, marker+"\n"+strings.Repeat("a", 150))
	}
	for index := 0; index < 4; index++ {
		fx.Project(test, marker+" project")
	}
	query := "  " + marker + "  "
	response := testutil.Call(test, suggestionsRouter(test), suggestionsRequest(query, integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	rows := suggestionsRows(test, response, query, "rows", 200)
	if len(rows) != 8 {
		test.Fatalf("rows = %v, want five issues and three projects", rows)
	}
	for index, row := range rows {
		if strings.ContainsAny(row, "\r\n\t") || len([]rune(row)) > 120 || !strings.Contains(row, marker) {
			test.Errorf("invalid row: %q", row)
		}
		if index >= 5 && row != marker+" project" {
			test.Errorf("project row: %q", row)
		}
	}
}

func TestSearchSuggestionsIdentifierPrefix(test *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	slug := fmt.Sprintf("digit-prefix-%d", time.Now().UnixNano())
	workspace := fx.Workspace(test, "Digits", slug, testutil.Cols{"issue_prefix": "C9"})
	fx.Member(test, workspace, testUserID, "owner")
	fx.Issue(test, "identifier target", testutil.Cols{"workspace_id": workspace, "number": 123})
	router := suggestionsRouter(test)
	for _, query := range []string{"c9-123", "OTHER-123", "123"} {
		outcome := "rows"
		if query == "OTHER-123" {
			outcome = "empty"
		}
		response := testutil.Call(test, router, suggestionsRequest(query, slug, testToken)).ResponseRecorder
		rows := suggestionsRows(test, response, query, outcome, 200)
		if query != "OTHER-123" && (len(rows) != 1 || rows[0] != "C9-123 identifier target") {
			test.Errorf("identifier rows: %v", rows)
		}
	}
}

func TestSearchSuggestionsRateLimitAndLogPrivacy(test *testing.T) {
	test.Setenv("RATE_LIMIT_SEARCH_SUGGESTIONS", "1")
	router := suggestionsRouter(test)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	test.Cleanup(func() { slog.SetDefault(previous) })
	query := "sensitive-marker-do-not-log"
	first := testutil.Call(test, router, suggestionsRequest(query, integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	suggestionsRows(test, first, query, "empty", 200)
	second := testutil.Call(test, router, suggestionsRequest(query, integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	suggestionsRows(test, second, query, "throttled", 200)
	if second.Header().Get("Retry-After") == "" {
		test.Error("no retry hint")
	}
	if strings.Contains(logs.String(), query) || strings.Contains(logs.String(), testToken) {
		test.Fatal("request data leaked in logs")
	}
	if !strings.Contains(logs.String(), "outcome=throttled") {
		test.Fatalf("missing outcome log: %s", logs.String())
	}
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	email := fmt.Sprintf("rate-%d@multica.test", time.Now().UnixNano())
	user := fx.User(test, "Rate", email)
	fx.Member(test, testWorkspaceID, user, "member")
	token, err := generateTestJWT(user, email, "Rate")
	if err != nil {
		test.Fatal(err)
	}
	response := testutil.Call(test, router, suggestionsRequest(query, integrationTestWorkspaceSlug, token)).ResponseRecorder
	suggestionsRows(test, response, query, "empty", 200)
}

func TestSearchSuggestionsCrossOriginAndMetrics(test *testing.T) {
	test.Setenv("CORS_ALLOWED_ORIGINS", "https://attacker.example")
	registry := obsmetrics.NewRegistry(obsmetrics.RegistryOptions{})
	router, _ := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{HTTPMetrics: registry.HTTP})
	request := suggestionsRequest("query-marker-no-logs", integrationTestWorkspaceSlug, testToken)
	request.Header.Set("Origin", "https://attacker.example")
	response := testutil.Call(test, router.ServeHTTP, request).ResponseRecorder
	suggestionsRows(test, response, "query-marker-no-logs", "empty", 200)
	if response.Header().Get("Access-Control-Allow-Origin") != "" {
		test.Fatal("cross-origin page can read suggestions")
	}
	metrics := testutil.Call(test, obsmetrics.NewHandler(registry.Gatherer).ServeHTTP, httptest.NewRequest("GET", "/metrics", nil)).Body.String()
	if !strings.Contains(metrics, `multica_search_suggestions_requests_total{outcome="empty"} 1`) || strings.Contains(metrics, "query-marker-no-logs") {
		test.Fatalf("unsafe or missing outcome metric: %s", metrics)
	}
}

func TestSearchSuggestionsTimeoutReleasesCapacity(test *testing.T) {
	test.Setenv("SEARCH_SUGGESTIONS_MAX_CONCURRENT", "1")
	router := suggestionsRouter(test)
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { tx.Rollback(context.Background()) })
	if _, err := tx.Exec(context.Background(), `LOCK TABLE issue IN ACCESS EXCLUSIVE MODE`); err != nil {
		test.Fatal(err)
	}
	started := time.Now()
	response := testutil.Call(test, router, suggestionsRequest("locked-search", integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	suggestionsRows(test, response, "locked-search", "timeout", 200)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		test.Fatalf("timeout took %s", elapsed)
	}
	tx.Rollback(context.Background())
	response = testutil.Call(test, router, suggestionsRequest("released-search", integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	suggestionsRows(test, response, "released-search", "empty", 200)
}

func TestSearchSuggestionsOverCapAndCancellation(test *testing.T) {
	test.Setenv("SEARCH_SUGGESTIONS_MAX_CONCURRENT", "1")
	router := suggestionsRouter(test)
	transaction, err := testPool.Begin(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { transaction.Rollback(context.Background()) })
	if _, err := transaction.Exec(context.Background(), `LOCK TABLE issue IN ACCESS EXCLUSIVE MODE`); err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var running sync.WaitGroup
	running.Add(1)
	first := httptest.NewRecorder()
	go func() {
		defer running.Done()
		router.ServeHTTP(first, suggestionsRequest("held-search", integrationTestWorkspaceSlug, testToken).WithContext(ctx))
	}()
	defer running.Wait()
	deadline := time.Now().Add(time.Second)
	for {
		var blocked bool
		if err := testPool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock')`).Scan(&blocked); err != nil {
			test.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			test.Fatal("first request did not reach database lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	second := testutil.Call(test, router, suggestionsRequest("over-cap-search", integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	suggestionsRows(test, second, "over-cap-search", "over_cap", 200)
	cancel()
	running.Wait()
	suggestionsRows(test, first, "held-search", "cancelled", 200)
	transaction.Rollback(context.Background())
	third := testutil.Call(test, router, suggestionsRequest("released-search", integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	suggestionsRows(test, third, "released-search", "empty", 200)
}

func TestSearchSuggestionsClosedAndHiddenMatches(test *testing.T) {
	fixture := testutil.New(testPool, testWorkspaceID, testUserID)
	marker := fmt.Sprintf("closed%d", time.Now().UnixNano())
	fixture.Issue(test, marker, testutil.Cols{"status": "cancelled"})
	fixture.Issue(test, "description only", testutil.Cols{"description": marker})
	issue := fixture.Issue(test, "comment only")
	fixture.Comment(test, issue, marker)
	fixture.Project(test, "description project", testutil.Cols{"description": marker})
	response := testutil.Call(test, suggestionsRouter(test), suggestionsRequest(marker, integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	rows := suggestionsRows(test, response, marker, "rows", 200)
	if len(rows) != 1 || !strings.HasSuffix(rows[0], marker) {
		test.Fatalf("non-visible match leaked or closed issue lost: %v", rows)
	}
}

type unavailableSuggestionsDatabase struct{}

func (unavailableSuggestionsDatabase) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("database unavailable")
}

func TestSearchSuggestionsFailureReturnsEmptyRows(test *testing.T) {
	router, handlers := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{})
	handlers.TxStarter = unavailableSuggestionsDatabase{}
	response := testutil.Call(test, router.ServeHTTP, suggestionsRequest("private-error-query", integrationTestWorkspaceSlug, testToken)).ResponseRecorder
	rows := suggestionsRows(test, response, "private-error-query", "error", 200)
	if len(rows) != 0 {
		test.Fatalf("error returned rows: %v", rows)
	}
}
