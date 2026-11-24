package ghsnapshot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPRQueueTimelineReadsRemovalsCommitsAndForcePushes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"timelineItems":{"nodes":[
		 {"__typename":"PullRequestCommit","id":"C1","commit":{"committedDate":"2026-11-19T01:00:00Z","pushedDate":"2026-11-19T01:01:00Z"}},
		 {"__typename":"RemovedFromMergeQueueEvent","id":"R1","createdAt":"2026-11-19T02:00:00Z","reason":"CI_FAILURE","beforeCommit":{"oid":"abc123"}},
		 {"__typename":"HeadRefForcePushedEvent","id":"F1","createdAt":"2026-11-19T03:00:00Z"}]}}}}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.tokens[1] = cachedToken{token: "test-token", expiry: c.now().Add(2 * tokenRenewSkew)}
	items, err := c.PRQueueTimeline(context.Background(), 1, "acme", "widget", 7)
	if err != nil || len(items) != 3 {
		t.Fatalf("items = %+v, err %v", items, err)
	}
	if items[0].Type != "PullRequestCommit" || items[0].At.Minute() != 1 {
		t.Errorf("commit item = %+v, want pushed date", items[0])
	}
	if r := items[1]; r.Type != "RemovedFromMergeQueueEvent" || r.NodeID != "R1" || r.BeforeOID != "abc123" || r.Reason != "CI_FAILURE" {
		t.Errorf("removal item = %+v", r)
	}
	if items[2].Type != "HeadRefForcePushedEvent" {
		t.Errorf("force push item = %+v", items[2])
	}
}

func TestPRQueueTimelineRejectsGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"boom"}]}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.tokens[1] = cachedToken{token: "test-token", expiry: c.now().Add(2 * tokenRenewSkew)}
	if _, err := c.PRQueueTimeline(context.Background(), 1, "acme", "widget", 7); err == nil {
		t.Fatal("want an error for a GraphQL error response")
	}
}

func TestListMergeGroupRunsFiltersByEventAndHeadSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if !strings.HasSuffix(r.URL.Path, "/repos/acme/widget/actions/runs") || q.Get("event") != "merge_group" || q.Get("head_sha") != "abc123" {
			t.Errorf("unexpected request %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"workflow_runs":[{"id":9,"run_attempt":2,"status":"completed","conclusion":"failure","created_at":"2026-11-19T02:01:00Z","updated_at":"2026-11-19T02:09:00Z"}]}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.tokens[1] = cachedToken{token: "test-token", expiry: c.now().Add(2 * tokenRenewSkew)}
	runs, err := c.ListMergeGroupRuns(context.Background(), 1, "acme", "widget", "abc123")
	if err != nil || len(runs) != 1 || runs[0].ID != 9 || runs[0].Attempt != 2 || runs[0].Conclusion != "failure" || runs[0].Status != "completed" {
		t.Fatalf("runs = %+v, err %v", runs, err)
	}
}
