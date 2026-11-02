package handler

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"golang.org/x/text/unicode/norm"
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

// foldFrom/foldTo are the single mapping: migration 883 must carry the same
// literals, or the stored column and the Go query side would drift.
func TestFoldTable_MatchesMigration(t *testing.T) {
	up, err := os.ReadFile("../../migrations/886_search_folded_text.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := "translate(lower(COALESCE(value, '')), '" + foldFrom + "', '" + foldTo + "')"
	if !strings.Contains(string(up), want) {
		t.Fatalf("migration 883 search_fold() does not use foldFrom/foldTo from search_fold.go")
	}
	if len([]rune(foldFrom)) != len([]rune(foldTo)) {
		t.Fatalf("foldFrom has %d runes, foldTo has %d", len([]rune(foldFrom)), len([]rune(foldTo)))
	}
}

// Every Latin letter that Unicode decomposes into an ASCII letter plus marks,
// in the ranges the table claims (Latin-1, Latin Extended-A, ơ ư, Vietnamese),
// must fold to that base letter, in both cases.
func TestFoldTable_CoversEveryAccentedLetter(t *testing.T) {
	ranges := [][2]rune{{0xC0, 0x17F}, {0x1A0, 0x1A1}, {0x1AF, 0x1B0}, {0x1EA0, 0x1EF9}}
	for _, r := range ranges {
		for c := r[0]; c <= r[1]; c++ {
			d := []rune(norm.NFD.String(string(c)))
			if len(d) < 2 || d[0] > unicode.MaxASCII || !unicode.IsLetter(d[0]) {
				continue
			}
			if got, want := foldDiacritics(string(c)), strings.ToLower(string(d[0])); got != want {
				t.Errorf("foldDiacritics(%q U+%04X) = %q, want %q", c, c, got, want)
			}
		}
	}
	if got := foldDiacritics("Đ đ"); got != "d d" {
		t.Errorf("foldDiacritics(Đ đ) = %q, want %q", got, "d d")
	}
}

// AC5: for every character in the mapping table the Go fold equals the SQL
// fold, both through search_fold() and through the column the triggers fill.
func TestFoldDiacritics_StoredColumnParity(t *testing.T) {
	ctx := context.Background()
	chars := strings.Split(foldFrom, "")
	chars = append(chars, "Plain ASCII 123", "Mixed Lỗi ✓ 日本", "ẤN Đ-Ư 100%")

	rows, err := testPool.Query(ctx, "SELECT search_fold(c) FROM unnest($1::text[]) WITH ORDINALITY AS u(c, n) ORDER BY n", chars)
	if err != nil {
		t.Fatalf("search_fold: %v", err)
	}
	defer rows.Close()
	for i := 0; rows.Next(); i++ {
		var got string
		if err := rows.Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want := foldDiacritics(chars[i]); got != want {
			t.Errorf("fold %q: SQL %q, Go %q", chars[i], got, want)
		}
	}

	all := strings.Join(chars, " ")
	workspaceID := dbfx.Workspace(t, "Fold parity", "fold-parity-"+uuid.NewString())
	dbfx.Member(t, workspaceID, testUserID, "owner")
	issueID := dbfx.Issue(t, all, testutil.Cols{"workspace_id": workspaceID, "description": all})
	commentID := dbfx.Comment(t, issueID, all, testutil.Cols{"workspace_id": workspaceID})
	var title, description, content string
	if err := testPool.QueryRow(ctx, "SELECT title_folded, description_folded FROM issue_search_text WHERE issue_id = $1", issueID).Scan(&title, &description); err != nil {
		t.Fatalf("stored issue text: %v", err)
	}
	if err := testPool.QueryRow(ctx, "SELECT content_folded FROM comment_search_text WHERE comment_id = $1", commentID).Scan(&content); err != nil {
		t.Fatalf("stored comment text: %v", err)
	}
	want := foldDiacritics(all)
	if title != want || description != want || content != want {
		t.Errorf("stored title/description/comment differ from Go fold %q:\n%q\n%q\n%q", want, title, description, content)
	}
}

// The stored text follows edits, or search would keep matching stale words.
func TestIssueSearch_FollowsEditedTitleAndComment(t *testing.T) {
	f := newDiacriticWorkspace(t)
	id := dbfx.Issue(t, "Old title", testutil.Cols{"workspace_id": f.workspaceID})
	commentID := dbfx.Comment(t, id, "Old comment", testutil.Cols{"workspace_id": f.workspaceID})
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, "UPDATE issue SET title = $2 WHERE id = $1", id, "Lỗi đăng nhập"); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, "UPDATE comment SET content = $2 WHERE id = $1", commentID, "Đã sửa thanh toán"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"dang nhap", "thanh toan"} {
		if got := f.search(t, q); len(got) != 1 || got[0].ID != id {
			t.Fatalf("search %q after edit: got %+v, want %s", q, got, id)
		}
	}
	if got := f.search(t, "old title"); len(got) != 0 {
		t.Fatalf("search for the replaced title still finds %+v", got)
	}
}

// No foreign key cascades the delete, so a trigger must drop the stored text.
func TestIssueSearch_DeleteRemovesStoredText(t *testing.T) {
	f := newDiacriticWorkspace(t)
	id := dbfx.Issue(t, "Lỗi đăng nhập", testutil.Cols{"workspace_id": f.workspaceID})
	commentID := dbfx.Comment(t, id, "Đã sửa", testutil.Cols{"workspace_id": f.workspaceID})
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, "DELETE FROM comment WHERE id = $1", commentID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM issue WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	var leftover int
	if err := testPool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM issue_search_text WHERE issue_id = $1) +
		(SELECT count(*) FROM comment_search_text WHERE comment_id = $2)`, id, commentID).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Fatalf("%d stored search rows outlive their issue and comment", leftover)
	}
}

// The workspace deletion manifest classifies both side tables as delete-owned
// by the row triggers. The teardown queries delete issue and comment rows in
// bulk, so the triggers must still clear the side rows.
func TestIssueSearch_WorkspaceTeardownRemovesStoredText(t *testing.T) {
	f := newDiacriticWorkspace(t)
	id := dbfx.Issue(t, "Lỗi đăng nhập", testutil.Cols{"workspace_id": f.workspaceID})
	dbfx.Comment(t, id, "Đã sửa", testutil.Cols{"workspace_id": f.workspaceID})
	ctx := context.Background()
	queries := db.New(testPool)
	workspaceID := parseUUID(f.workspaceID)
	if err := queries.DeleteWorkspaceComments(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	if err := queries.DeleteWorkspaceIssueRoots(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	var leftover int
	if err := testPool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM issue_search_text WHERE workspace_id = $1) +
		(SELECT count(*) FROM comment_search_text WHERE workspace_id = $1)`, f.workspaceID).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Fatalf("%d stored search rows survive workspace teardown", leftover)
	}
}

// The exact-diacritic rank must cost one pass over the stored comment text. A
// second pass over raw comment bodies (a lookup by id per matching comment)
// measured +34% on an accented query (CHE-2943), over the +20% limit.
func TestBuildSearchQuery_ExactDiacriticsRankIsOnePassOverComments(t *testing.T) {
	sql, _ := buildSearchQuery("đăng nhập", splitSearchTerms("đăng nhập"), 0, false, false, nil)
	for _, forbidden := range []string{"comment_cased", "unnest(", "comment rc"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("accented query still contains %q:\n%s", forbidden, sql)
		}
	}
	if !strings.Contains(sql, "content_accents") {
		t.Errorf("accented query does not rank on the stored accented text:\n%s", sql)
	}
}

// content_accents keeps the lowered, unfolded comment only where folding
// changed it, so ASCII comments cost no extra storage and an accented query
// can rank on it without reading the comment table.
func TestIssueSearch_StoresAccentedCommentTextOnlyWhereFoldingChangesIt(t *testing.T) {
	f := newDiacriticWorkspace(t)
	id := dbfx.Issue(t, "Plain title", testutil.Cols{"workspace_id": f.workspaceID})
	plain := dbfx.Comment(t, id, "Plain ASCII comment", testutil.Cols{"workspace_id": f.workspaceID})
	accented := dbfx.Comment(t, id, "Đã sửa Lỗi", testutil.Cols{"workspace_id": f.workspaceID})
	ctx := context.Background()
	for commentID, want := range map[string]*string{plain: nil, accented: strPtr("đã sửa lỗi")} {
		var got *string
		if err := testPool.QueryRow(ctx, "SELECT content_accents FROM comment_search_text WHERE comment_id = $1", commentID).Scan(&got); err != nil {
			t.Fatalf("content_accents: %v", err)
		}
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("comment %s: content_accents = %v, want %v", commentID, derefOrNull(got), derefOrNull(want))
		}
	}
	if _, err := testPool.Exec(ctx, "UPDATE comment SET content = 'now plain' WHERE id = $1", accented); err != nil {
		t.Fatal(err)
	}
	var got *string
	if err := testPool.QueryRow(ctx, "SELECT content_accents FROM comment_search_text WHERE comment_id = $1", accented).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("content_accents after editing to ASCII = %q, want NULL", *got)
	}
}

func TestIssueSearch_StoresAccentedIssueTextOnlyWhereFoldingChangesIt(t *testing.T) {
	f := newDiacriticWorkspace(t)
	plain := dbfx.Issue(t, "Plain title", testutil.Cols{"workspace_id": f.workspaceID, "description": "Plain text"})
	accented := dbfx.Issue(t, "Lỗi Đăng nhập", testutil.Cols{"workspace_id": f.workspaceID, "description": "Không vào được"})
	ctx := context.Background()
	type stored struct{ title, description *string }
	for issueID, want := range map[string]stored{
		plain:    {nil, nil},
		accented: {strPtr("lỗi đăng nhập"), strPtr("không vào được")},
	} {
		var got stored
		if err := testPool.QueryRow(ctx, "SELECT title_accents, description_accents FROM issue_search_text WHERE issue_id = $1", issueID).Scan(&got.title, &got.description); err != nil {
			t.Fatalf("accents: %v", err)
		}
		if derefOrNull(got.title) != derefOrNull(want.title) || derefOrNull(got.description) != derefOrNull(want.description) {
			t.Errorf("issue %s: accents = (%v, %v), want (%v, %v)", issueID,
				derefOrNull(got.title), derefOrNull(got.description), derefOrNull(want.title), derefOrNull(want.description))
		}
	}
	if _, err := testPool.Exec(ctx, "UPDATE issue SET title = 'Now plain', description = 'plain' WHERE id = $1", accented); err != nil {
		t.Fatal(err)
	}
	var title, description *string
	if err := testPool.QueryRow(ctx, "SELECT title_accents, description_accents FROM issue_search_text WHERE issue_id = $1", accented).Scan(&title, &description); err != nil {
		t.Fatal(err)
	}
	if title != nil || description != nil {
		t.Errorf("accents after editing to ASCII = (%v, %v), want NULL, NULL", derefOrNull(title), derefOrNull(description))
	}
}

func derefOrNull(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
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

// The newest matching comment is the snippet, but an older exact comment still
// earns the issue its exact-diacritic rank (CHE-2943 second review, terra).
func TestIssueSearch_ExactDiacriticsRankCountsEveryMatchingComment(t *testing.T) {
	f := newDiacriticWorkspace(t)
	exact := dbfx.Issue(t, "Alpha", testutil.Cols{"workspace_id": f.workspaceID})
	dbfx.Comment(t, exact, "Đã sửa lỗi đăng nhập", testutil.Cols{"workspace_id": f.workspaceID})
	dbfx.Comment(t, exact, "Kiểm tra dang nhap lại", testutil.Cols{"workspace_id": f.workspaceID})
	foldedOnly := dbfx.Issue(t, "Beta", testutil.Cols{"workspace_id": f.workspaceID})
	dbfx.Comment(t, foldedOnly, "Kiểm tra dang nhap lại", testutil.Cols{"workspace_id": f.workspaceID})

	got := f.search(t, "đăng nhập")
	if len(got) != 2 || got[0].ID != exact || got[1].ID != foldedOnly {
		t.Fatalf("got %+v, want exact %s before folded-only %s", got, exact, foldedOnly)
	}
}
