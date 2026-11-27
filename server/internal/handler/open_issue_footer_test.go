package handler

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const footerIssueID = "01a215c1-9ea5-7097-8cf7-ea9b9748c4a4"

var markdownLinkTarget = regexp.MustCompile(`\]\([^)]*\)`)
var bareIssuePath = regexp.MustCompile(`/issues?/`)

// requireNoBareIssuePath fails when chat notice content holds an /issues/ or
// /issue/ path outside a markdown link (CHE-10217): the web chat shows a bare
// path as raw text.
func requireNoBareIssuePath(t testing.TB, content string) {
	t.Helper()
	if bareIssuePath.MatchString(markdownLinkTarget.ReplaceAllString(content, "]")) {
		t.Fatalf("notice has a bare issue path: %q", content)
	}
}

// requireOpenIssueFooter asserts the notice ends its issue link as a chip.
func requireOpenIssueFooter(t testing.TB, content, identifier, issueID string) {
	t.Helper()
	want := "Open issue: [" + identifier + "](mention://issue/" + issueID + ")"
	if !strings.Contains(content, want) {
		t.Fatalf("notice lacks footer %q: %q", want, content)
	}
	requireNoBareIssuePath(t, content)
}

func TestNoBareIssuePathGuard(t *testing.T) {
	for _, ok := range []string{"Open issue: [CHE-1](mention://issue/" + footerIssueID + ")", "[card](/ws/issues/x)", "no path"} {
		requireNoBareIssuePath(t, ok)
	}
	for _, bad := range []string{"Open issue: /ws/issues/x", "Open issue: /issue/x", "see [a](/ws/issues/x) and /ws/issues/y"} {
		if !bareIssuePath.MatchString(markdownLinkTarget.ReplaceAllString(bad, "]")) {
			t.Fatalf("guard missed %q", bad)
		}
	}
}

func TestAskRecoveryContentEndsWithIssueChip(t *testing.T) {
	requireOpenIssueFooter(t, askRecoveryContent("CHE-10086", footerIssueID, "the ask"), "CHE-10086", footerIssueID)
}

func TestStepNoticeContentEndsWithIssueChip(t *testing.T) {
	var issue db.Issue
	issue.ID = parseUUID(footerIssueID)
	content := stepNoticeContent("CHE-10086", issue, db.IssueStepWatchdogAction{Action: "redispatch"}, pgtype.UUID{})
	requireOpenIssueFooter(t, content, "CHE-10086", footerIssueID)
}

// Every notice must build its footer with util.OpenIssueFooter, so the six
// templates cannot drift back to a raw path.
func TestNoticeTemplatesShareTheFooterHelper(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "Open issue:") {
			t.Errorf("%s hand-writes an \"Open issue:\" footer; use util.OpenIssueFooter", name)
		}
	}
}
