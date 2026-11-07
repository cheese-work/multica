package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// countingTxStarter counts search transactions so a test can tell whether the
// typo fallback ran: the normal query is one transaction, the fallback a second.
type countingTxStarter struct {
	inner txStarter
	begun atomic.Int32
}

func (c *countingTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	c.begun.Add(1)
	return c.inner.Begin(ctx)
}

// searchIssueIDs runs SearchIssues and returns the matched issue IDs plus how
// many search transactions the request opened.
func searchIssueIDs(t *testing.T, rawQuery string) ([]string, int) {
	t.Helper()

	counter := &countingTxStarter{inner: testHandler.TxStarter}
	h := *testHandler
	h.TxStarter = counter

	path := fmt.Sprintf("/api/issues/search?workspace_id=%s&limit=50&%s", testWorkspaceID, rawQuery)
	w := httptest.NewRecorder()
	h.SearchIssues(w, newRequest("GET", path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("SearchIssues(%q): expected 200, got %d: %s", rawQuery, w.Code, w.Body.String())
	}
	var response struct {
		Issues []SearchIssueResponse `json:"issues"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode search response: %v", err)
	}
	ids := make([]string, 0, len(response.Issues))
	for _, issue := range response.Issues {
		ids = append(ids, issue.ID)
	}
	return ids, int(counter.begun.Load())
}

func containsID(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

func TestSearchIssues_FuzzyFallbackFindsTypoInTitle(t *testing.T) {
	id := dbfx.Issue(t, "fuzzy search")

	ids, begun := searchIssueIDs(t, "q="+url.QueryEscape("fuzy serch"))
	if !containsID(ids, id) {
		t.Fatalf("fuzy serch did not find %q; got %v", "fuzzy search", ids)
	}
	if begun != 2 {
		t.Fatalf("search transactions = %d, want 2 (normal query + fallback)", begun)
	}
}

func TestSearchIssues_FuzzyFallbackFindsTypoWithoutDiacritics(t *testing.T) {
	id := dbfx.Issue(t, "Lỗi đăng nhập")

	ids, _ := searchIssueIDs(t, "q="+url.QueryEscape("dang nhapp"))
	if !containsID(ids, id) {
		t.Fatalf("dang nhapp did not find %q; got %v", "Lỗi đăng nhập", ids)
	}
}

func TestSearchIssues_FuzzyFallbackFindsTypoInDescription(t *testing.T) {
	id := dbfx.Issue(t, "Unrelated heading", testutil.Cols{
		"description": "the importer crashes when the spreadsheet contains duplicate columns",
	})

	ids, _ := searchIssueIDs(t, "q="+url.QueryEscape("spreadsheat duplcate")) //nolint:misspell // intentional typo
	if !containsID(ids, id) {
		t.Fatalf("typo in description query did not find the issue; got %v", ids)
	}
}

func TestSearchIssues_FuzzyFallbackOrdersByScore(t *testing.T) {
	weak := dbfx.Issue(t, "fuzzy sketch of everything")
	strong := dbfx.Issue(t, "fuzzy search")

	ids, _ := searchIssueIDs(t, "q="+url.QueryEscape("fuzy serch"))
	strongAt, weakAt := indexOfID(ids, strong), indexOfID(ids, weak)
	if strongAt < 0 {
		t.Fatalf("best match missing; got %v", ids)
	}
	if weakAt >= 0 && weakAt < strongAt {
		t.Fatalf("weaker match ranked above stronger one: %v", ids)
	}
}

func TestSearchIssues_FuzzyFallbackKeepsClosedFilter(t *testing.T) {
	id := dbfx.Issue(t, "fuzzy search closed", testutil.Cols{"status": "done"})

	ids, _ := searchIssueIDs(t, "q="+url.QueryEscape("fuzy serch closd"))
	if containsID(ids, id) {
		t.Fatalf("closed issue returned without include_closed; got %v", ids)
	}
	ids, _ = searchIssueIDs(t, "include_closed=true&q="+url.QueryEscape("fuzy serch closd"))
	if !containsID(ids, id) {
		t.Fatalf("closed issue missing with include_closed=true; got %v", ids)
	}
}

func TestSearchIssues_FuzzyFallbackSkipsComments(t *testing.T) {
	id := dbfx.Issue(t, "Plain heading")
	dbfx.Comment(t, id, "the importer crashes on duplicate columns")

	ids, _ := searchIssueIDs(t, "q="+url.QueryEscape("importr crashs duplcate"))
	if containsID(ids, id) {
		t.Fatalf("comment text matched the fuzzy fallback; got %v", ids)
	}
}

func TestSearchIssues_FuzzyFallbackSkippedWhenNormalSearchHits(t *testing.T) {
	dbfx.Issue(t, "exactlyfoundtitle xk92")

	ids, begun := searchIssueIDs(t, "q="+url.QueryEscape("exactlyfoundtitle"))
	if len(ids) == 0 {
		t.Fatal("normal search found nothing")
	}
	if begun != 1 {
		t.Fatalf("search transactions = %d, want 1 (no fallback on a hit)", begun)
	}
}

func TestSearchIssues_FuzzyFallbackSkippedForShortQueryAndNumbers(t *testing.T) {
	for _, q := range []string{"ab", "123", "ZZ-123", "đa"} {
		if _, begun := searchIssueIDs(t, "q="+url.QueryEscape(q)); begun != 1 {
			t.Errorf("q=%q opened %d search transactions, want 1", q, begun)
		}
	}
}

func TestSearchIssues_FuzzyFallbackSkippedOnLaterPages(t *testing.T) {
	dbfx.Issue(t, "fuzzy search")

	if _, begun := searchIssueIDs(t, "offset=20&q="+url.QueryEscape("fuzy serch")); begun != 1 {
		t.Fatalf("search transactions = %d, want 1 on a later page", begun)
	}
}

func indexOfID(ids []string, id string) int {
	for i, got := range ids {
		if got == id {
			return i
		}
	}
	return -1
}
