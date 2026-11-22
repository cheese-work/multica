package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// CHE-7519: a multi-token query that mixes a very common token (the repository
// name) with rare ones (red-main, build-images--web-) timed out in production
// (HTTP 503). The corpus has the production shape of CHE-3164: 3,200 issues and
// 62,000 comments, most of which name the repository.
func TestSearchCommonTokenMultiTermIsFast(t *testing.T) {
	ctx := context.Background()
	wsID := seedSearchBenchShaped(t, ctx, "che7519-search-hyphen", 3200, 62000)
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM comment WHERE workspace_id = $1`,
			`DELETE FROM issue WHERE workspace_id = $1`,
			`DELETE FROM workspace WHERE id = $1`,
		} {
			if _, err := testPool.Exec(ctx, stmt, wsID); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	for _, stmt := range []string{
		// The repository name sits in 80% of descriptions and comments.
		`UPDATE issue_search_text SET description_folded = description_folded || ' cheese-work/multica' WHERE workspace_id = $1 AND hashtext(issue_id::text) % 5 <> 0`,
		`UPDATE comment_search_text SET content_folded = content_folded || ' cheese-work/multica' WHERE workspace_id = $1 AND hashtext(comment_id::text) % 5 <> 0`,
		// red-main is common too (the incident issues): about 6% of descriptions and comments.
		`UPDATE issue_search_text SET description_folded = description_folded || ' red-main' WHERE workspace_id = $1 AND hashtext(issue_id::text) % 16 = 1`,
		`UPDATE comment_search_text SET content_folded = content_folded || ' red-main' WHERE workspace_id = $1 AND hashtext(comment_id::text) % 16 = 1`,
		// Two issues hold the rare tokens: one in the title, one split title/comment.
		`UPDATE issue_search_text SET title_folded = 'red-main cheese-work/multica build-images--web-: fix red check' WHERE issue_id = (SELECT id FROM issue WHERE workspace_id = $1 AND number = 11)`,
		`UPDATE issue_search_text SET title_folded = 'red-main cheese-work/multica x' WHERE issue_id = (SELECT id FROM issue WHERE workspace_id = $1 AND number = 12)`,
		`UPDATE comment_search_text SET content_folded = 'build-images--web- failed' WHERE comment_id = (SELECT id FROM comment WHERE workspace_id = $1 AND issue_id = (SELECT id FROM issue WHERE workspace_id = $1 AND number = 12) LIMIT 1)`,
		`UPDATE issue SET title = 'red-main cheese-work/multica build-images--web-: fix red check' WHERE workspace_id = $1 AND number = 11`,
		`ANALYZE issue_search_text; ANALYZE comment_search_text`,
	} {
		if _, err := testPool.Exec(ctx, stmt, wsID); err != nil && stmt[0] != 'A' {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// The longest term is the repository name, which names most comments, so it
	// made the candidate list the whole workspace. The rarest term must drive.
	terms := splitSearchTerms("red-main cheese-work/multica build-images--web-")
	wsUUID, _ := parseUUIDOrBadRequest(httptest.NewRecorder(), wsID, "workspace_id")
	if got := testHandler.pickDrivingTerm(ctx, wsUUID, terms); got != 2 {
		t.Errorf("pickDrivingTerm = %d, want 2 (build-images--web-)", got)
	}
	// A short term and a single term never probe: the default stays.
	if got := testHandler.pickDrivingTerm(ctx, wsUUID, splitSearchTerms("red-main ab")); got != -1 {
		t.Errorf("pickDrivingTerm with one long term = %d, want -1", got)
	}
	if got := testHandler.pickDrivingTerm(ctx, wsUUID, splitSearchTerms("red-main")); got != -1 {
		t.Errorf("pickDrivingTerm with one term = %d, want -1", got)
	}
	// The claim script searches before any fix issue exists, so the common case
	// finds nothing and falls through to the typo fallback, which must stay a
	// best-effort step: bounded, and never a 503 for a query the normal search
	// already answered.
	for _, tc := range []struct {
		q    string
		want int // -1: any count, the fallback may or may not finish in its budget
	}{
		{"red-main cheese-work/multica build-images--web-", 1},
		{"red-main cheese-work/multica build-images--web-x", -1},
		{"red-main cheese-work/multica deploy-images--api-", -1},
	} {
		path := fmt.Sprintf("/api/issues/search?workspace_id=%s&limit=50&q=%s", wsID, url.QueryEscape(tc.q))
		start := time.Now()
		w := httptest.NewRecorder()
		req := newRequest("GET", path, nil)
		req.Header.Set("X-Workspace-ID", wsID)
		testHandler.SearchIssues(w, req)
		took := time.Since(start)
		t.Logf("%q: HTTP %d in %v", tc.q, w.Code, took)
		if w.Code != http.StatusOK {
			t.Errorf("%q: HTTP %d: %s", tc.q, w.Code, w.Body.String())
			continue
		}
		var resp struct {
			Issues []SearchIssueResponse `json:"issues"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if tc.want >= 0 && len(resp.Issues) != tc.want {
			t.Errorf("%q: want %d issues, got %d", tc.q, tc.want, len(resp.Issues))
		}
		if took > 3*time.Second {
			t.Errorf("%q: search took %v, want under 3s", tc.q, took)
		}
	}
}
