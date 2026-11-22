package handler

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-7337: title_only=true is the instant tier of Cmd+K. It answers from the
// title alone, newest update first, so it can stop at the limit instead of
// scoring every candidate.

func seedTitleTierIssue(t *testing.T, title, description string, updatedAgo time.Duration) string {
	t.Helper()
	title = seedRankIssue(t, title, "todo")
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET description = $1, updated_at = now() - make_interval(secs => $2) WHERE workspace_id = $3 AND title = $4`,
		description, updatedAgo.Seconds(), testWorkspaceID, title); err != nil {
		t.Fatalf("set description and updated_at: %v", err)
	}
	return title
}

func searchTitleTier(t *testing.T, q string, limit int) []string {
	t.Helper()
	path := fmt.Sprintf("/api/issues/search?workspace_id=%s&q=%s&limit=%d&include_closed=true&title_only=true",
		testWorkspaceID, url.QueryEscape(q), limit)
	var out struct {
		Issues []SearchIssueResponse `json:"issues"`
	}
	testutil.Call(t, testHandler.SearchIssues, newRequest("GET", path, nil)).Want(200).JSON(&out)
	titles := make([]string, 0, len(out.Issues))
	for _, issue := range out.Issues {
		titles = append(titles, issue.Title)
	}
	return titles
}

func TestSearchIssues_TitleOnlyOrdersByUpdatedAtAndIgnoresOtherFields(t *testing.T) {
	token := fmt.Sprintf("tiertok%d", time.Now().UnixNano())
	oldest := seedTitleTierIssue(t, token+" oldest", "", 3*time.Hour)
	newest := seedTitleTierIssue(t, "newest "+token, "", 1*time.Minute)
	middle := seedTitleTierIssue(t, token+" middle", "", 1*time.Hour)
	seedTitleTierIssue(t, "unrelated title", token+" only in the description", 10*time.Second)

	assertOrder(t, token, searchTitleTier(t, token, 8), []string{newest, middle, oldest})
	assertOrder(t, token+" limit 2", searchTitleTier(t, token, 2), []string{newest, middle})
}

func TestSearchIssues_TitleOnlyRequiresEveryTermAndFoldsDiacritics(t *testing.T) {
	token := fmt.Sprintf("tierterm%d", time.Now().UnixNano())
	both := seedTitleTierIssue(t, "Đăng "+token+" nhập", "", time.Minute)
	seedTitleTierIssue(t, token+" alone", "", 2*time.Minute)

	assertOrder(t, "two terms", searchTitleTier(t, token+" dang", 8), []string{both})
	// A 2 character term cannot use a trigram index; it must still narrow.
	assertOrder(t, "short term", searchTitleTier(t, token+" ng", 8), []string{both})
}

// The tier reads the newest titleTierScanWindow issues first. When that window holds
// fewer than limit matches it must still return the newest matches of the whole
// workspace, so an old sole match is never hidden.
func TestSearchIssues_TitleOnlyFindsMatchesOlderThanTheWindow(t *testing.T) {
	prev := titleTierScanWindow
	titleTierScanWindow = 3
	t.Cleanup(func() { titleTierScanWindow = prev })

	token := fmt.Sprintf("tierwin%d", time.Now().UnixNano())
	solo := seedTitleTierIssue(t, "solo only "+token+"solo", "", 3*time.Hour)
	for i := 1; i <= 3; i++ {
		seedTitleTierIssue(t, fmt.Sprintf("filler %d %s", i, token[:8]), "", time.Duration(i)*time.Second)
	}
	assertOrder(t, "sole match outside the window", searchTitleTier(t, token+"solo", 8), []string{solo})

	inWindow := seedTitleTierIssue(t, token+" fresh", "", 0)
	old := seedTitleTierIssue(t, token+" old", "", 4*time.Hour)
	assertOrder(t, "window plus older matches", searchTitleTier(t, token, 8), []string{inWindow, solo, old})
	assertOrder(t, "limit 2 stays exact", searchTitleTier(t, token, 2), []string{inWindow, solo})
}

func TestSearchIssues_TitleOnlyHonoursIncludeClosed(t *testing.T) {
	token := fmt.Sprintf("tierclosed%d", time.Now().UnixNano())
	open := seedTitleTierIssue(t, token+" open", "", time.Minute)
	closed := seedRankIssue(t, token+" closed", "done")

	get := func(includeClosed bool) []string {
		path := fmt.Sprintf("/api/issues/search?workspace_id=%s&q=%s&include_closed=%t&title_only=true",
			testWorkspaceID, url.QueryEscape(token), includeClosed)
		var out struct {
			Issues []SearchIssueResponse `json:"issues"`
		}
		testutil.Call(t, testHandler.SearchIssues, newRequest("GET", path, nil)).Want(200).JSON(&out)
		titles := []string{}
		for _, issue := range out.Issues {
			titles = append(titles, issue.Title)
		}
		return titles
	}
	assertOrder(t, "open only", get(false), []string{open})
	if got := get(true); len(got) != 2 || got[0] != open && got[1] != open || got[0] != closed && got[1] != closed {
		t.Fatalf("include_closed: got %v, want %q and %q", got, open, closed)
	}
}

// A term under 3 characters has no trigram, yet the tier still returns the newest
// matches of the whole workspace: a sole match older than the window is found.
func TestSearchIssues_TitleOnlyFindsOlderMatchForShortTerms(t *testing.T) {
	prev := titleTierScanWindow
	titleTierScanWindow = 3
	t.Cleanup(func() { titleTierScanWindow = prev })

	for _, term := range []string{"ʘ", "ʘʙ"} {
		solo := seedTitleTierIssue(t, fmt.Sprintf("solo %s %d", term, time.Now().UnixNano()), "", 3*time.Hour)
		for i := 1; i <= 3; i++ {
			seedTitleTierIssue(t, fmt.Sprintf("short filler %d %d", i, time.Now().UnixNano()), "", time.Duration(i)*time.Second)
		}
		assertOrder(t, "sole older match for "+term, searchTitleTier(t, term, 8), []string{solo})
		if got := searchTitleTier(t, term+"zz", 8); len(got) != 0 {
			t.Fatalf("no-match %q: got %v", term+"zz", got)
		}
	}
}

// Every term short: terms still fold diacritics and must all match.
func TestSearchIssues_TitleOnlyShortTermsFoldAndRequireEveryTerm(t *testing.T) {
	prev := titleTierScanWindow
	titleTierScanWindow = 3
	t.Cleanup(func() { titleTierScanWindow = prev })

	stamp := time.Now().UnixNano()
	both := seedTitleTierIssue(t, fmt.Sprintf("Đăʘ ʙx %d", stamp), "", 3*time.Hour)
	seedTitleTierIssue(t, fmt.Sprintf("only ʘ %d", stamp), "", 4*time.Hour)
	for i := 1; i <= 3; i++ {
		seedTitleTierIssue(t, fmt.Sprintf("fold filler %d %d", i, stamp), "", time.Duration(i)*time.Second)
	}
	assertOrder(t, "folded short terms", searchTitleTier(t, "da ʙx", 8), []string{both})
}
