package handler

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestFoldDiacritics(t *testing.T) {
	for in, want := range map[string]string{
		"Lỗi đăng nhập":  "loi dang nhap",
		"ĐĂNG NHẬP":      "dang nhap",
		"dang NHAP":      "dang nhap",
		"Đường Ưu Tiên":  "duong uu tien",
		"login 123 ✓ 日本": "login 123 ✓ 日本",
		"Café":           "cafe",
	} {
		if got := foldDiacritics(in); got != want {
			t.Errorf("foldDiacritics(%q) = %q, want %q", in, got, want)
		}
	}
}

// The Go fold only locates snippets, but it must agree with the SQL fold on
// every Vietnamese letter (both cases) and on NFD input, or a match would be
// found yet not highlighted.
func TestFoldDiacritics_SQLParity(t *testing.T) {
	const vietnamese = "àáảãạăằắẳẵặâầấẩẫậèéẻẽẹêềếểễệìíỉĩịòóỏõọôồốổỗộơờớởỡợùúủũụưừứửữựỳýỷỹỵđ"
	inputs := []string{"Plain ASCII 123", "Mixed Lỗi ✓ 日本", "e\u0302 o\u031b\u0323", "éèêëçñü"}
	for _, r := range vietnamese + strings.ToUpper(vietnamese) {
		inputs = append(inputs, string(r))
	}
	got, err := foldQueryTexts(context.Background(), testPool, inputs)
	if err != nil {
		t.Fatalf("fold in SQL: %v", err)
	}
	for i, in := range inputs {
		if want := foldDiacritics(in); got[i] != want {
			t.Errorf("fold %q: SQL %q, Go %q", in, got[i], want)
		}
	}
}

// Characters outside the Vietnamese alphabet that unaccent maps (and NFD does
// not) must still match, because the query is folded by the same function.
func TestIssueSearch_QueryFoldMatchesColumnFoldBeyondVietnamese(t *testing.T) {
	f := newDiacriticWorkspace(t)
	id := dbfx.Issue(t, "Straße zu Fuß", testutil.Cols{"workspace_id": f.workspaceID})
	for _, q := range []string{"strasse", "Straße", "fuss"} {
		got := f.search(t, q)
		if len(got) != 1 || got[0].ID != id {
			t.Fatalf("search %q: got %+v, want %s", q, got, id)
		}
	}
}

type diacriticSearchFixture struct {
	workspaceID string
}

func (f diacriticSearchFixture) search(t *testing.T, q string) []SearchIssueResponse {
	t.Helper()
	req := newRequest(http.MethodGet, "/api/issues/search?q="+url.QueryEscape(q), nil)
	req.Header.Set("X-Workspace-ID", f.workspaceID)
	var response struct {
		Issues []SearchIssueResponse `json:"issues"`
	}
	testutil.Call(t, testHandler.SearchIssues, req).Want(http.StatusOK).JSON(&response)
	return response.Issues
}

func newDiacriticWorkspace(t *testing.T) diacriticSearchFixture {
	t.Helper()
	workspaceID := dbfx.Workspace(t, "Diacritic search", "diacritic-search-"+uuid.NewString())
	dbfx.Member(t, workspaceID, testUserID, "owner")
	return diacriticSearchFixture{workspaceID: workspaceID}
}

func TestIssueSearch_FoldsDiacriticsInTitle(t *testing.T) {
	f := newDiacriticWorkspace(t)
	id := dbfx.Issue(t, "Lỗi đăng nhập", testutil.Cols{"workspace_id": f.workspaceID})
	dbfx.Issue(t, "Unrelated", testutil.Cols{"workspace_id": f.workspaceID})

	for _, q := range []string{"dang nhap", "ĐĂNG NHẬP", "dang NHAP", "đăng nhập", "loi dang"} {
		t.Run(q, func(t *testing.T) {
			got := f.search(t, q)
			if len(got) != 1 || got[0].ID != id || got[0].MatchSource != "title" {
				t.Fatalf("search %q: got %+v, want only title match %s", q, got, id)
			}
		})
	}
}

func TestIssueSearch_FoldsDiacriticlessTitleForAccentedQuery(t *testing.T) {
	f := newDiacriticWorkspace(t)
	id := dbfx.Issue(t, "Loi dang nhap", testutil.Cols{"workspace_id": f.workspaceID})

	got := f.search(t, "đăng nhập")
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("got %+v, want %s", got, id)
	}
}

func TestIssueSearch_FoldsDiacriticsInDescriptionAndComment(t *testing.T) {
	f := newDiacriticWorkspace(t)
	descID := dbfx.Issue(t, "Description hit", testutil.Cols{
		"workspace_id": f.workspaceID, "description": "Người dùng không đăng nhập được",
	})
	commentID := dbfx.Issue(t, "Comment hit", testutil.Cols{"workspace_id": f.workspaceID})
	dbfx.Comment(t, commentID, "Đã sửa lỗi đăng nhập", testutil.Cols{"workspace_id": f.workspaceID})

	got := f.search(t, "dang nhap")
	sources := map[string]string{}
	for _, issue := range got {
		sources[issue.ID] = issue.MatchSource
	}
	if len(got) != 2 || sources[descID] != "description" || sources[commentID] != "comment" {
		t.Fatalf("got %+v, want description %s and comment %s", got, descID, commentID)
	}
	for _, issue := range got {
		if issue.MatchSource == "description" && (issue.MatchedDescriptionSnippet == nil || *issue.MatchedDescriptionSnippet != "Người dùng không đăng nhập được") {
			t.Errorf("description snippet = %v, want the folded match located", issue.MatchedDescriptionSnippet)
		}
		if issue.MatchSource == "comment" && (issue.MatchedCommentSnippet == nil || *issue.MatchedCommentSnippet != "Đã sửa lỗi đăng nhập") {
			t.Errorf("comment snippet = %v, want the folded match located", issue.MatchedCommentSnippet)
		}
	}
}

func TestIssueSearch_ExactDiacriticsRankAboveFoldedOnlyMatch(t *testing.T) {
	f := newDiacriticWorkspace(t)
	// The folded-only match is newer, so recency alone would put it first.
	exact := dbfx.Issue(t, "Lỗi đăng nhập", testutil.Cols{"workspace_id": f.workspaceID})
	foldedOnly := dbfx.Issue(t, "Lỗi dang nhap", testutil.Cols{"workspace_id": f.workspaceID})

	got := f.search(t, "đăng nhập")
	if len(got) != 2 || got[0].ID != exact || got[1].ID != foldedOnly {
		t.Fatalf("got %+v, want exact %s before folded-only %s", got, exact, foldedOnly)
	}
}

// Terms spread over other words still agree on diacritics, so they rank above
// a match that only agrees after folding (CHE-2943 review, terra).
func TestIssueSearch_ExactDiacriticsRankAcrossNonContiguousTerms(t *testing.T) {
	f := newDiacriticWorkspace(t)
	exact := dbfx.Issue(t, "đăng lỗi nhập", testutil.Cols{"workspace_id": f.workspaceID})
	foldedOnly := dbfx.Issue(t, "dang loi nhap", testutil.Cols{"workspace_id": f.workspaceID})

	got := f.search(t, "đăng nhập")
	if len(got) != 2 || got[0].ID != exact || got[1].ID != foldedOnly {
		t.Fatalf("got %+v, want exact %s before folded-only %s", got, exact, foldedOnly)
	}
}
