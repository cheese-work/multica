package handler

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-10268 (S3 of CHE-10198): GET /api/github/pull-requests/signers exposes
// the VerifiedSigners service. The server returns facts only; any merge rule
// lives in the consumer.

var signersTestHead = strings.Repeat("0123456789", 4)

type signersTestFixture struct {
	issueID  string
	prNumber int
	reviewer string
	revTask  string
	author   string
}

type signersTestBody struct {
	Repo              string `json:"repo"`
	PR                int    `json:"pr"`
	HeadSHA           string `json:"head_sha"`
	IssueID           string `json:"issue_id"`
	Linked            bool   `json:"linked"`
	CommentProtection bool   `json:"comment_protection"`
	PRAuthor          *struct {
		AgentID   string `json:"agent_id"`
		AgentName string `json:"agent_name"`
		Lab       string `json:"lab"`
	} `json:"pr_author"`
	Signers []struct {
		AgentID   string `json:"agent_id"`
		AgentName string `json:"agent_name"`
		Lab       string `json:"lab"`
		CommentID string `json:"comment_id"`
		VerdictAt string `json:"verdict_at"`
	} `json:"signers"`
}

// newSignersFixture stores a PR, links it to an issue, binds an author agent to
// it and posts a PASS on signersTestHead from a reviewer agent's own run.
func newSignersFixture(t *testing.T, link bool) signersTestFixture {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	f := signersTestFixture{prNumber: 91000 + int(time.Now().UnixNano()%1000)}
	f.issueID = dbfx.Issue(t, "pr signers endpoint")
	f.reviewer = createHandlerTestAgent(t, "signers-endpoint-reviewer", nil)
	f.revTask = createHandlerTestTaskForAgentOnIssue(t, f.reviewer, f.issueID)
	f.author = createHandlerTestAgent(t, "signers-endpoint-author", nil)

	var prID string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO github_pull_request
		(workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url, pr_created_at, pr_updated_at, head_sha)
		VALUES ($1, 1, 'acme', 'widget', $2, 'signers PR', 'open', 'https://example.test/pr', now(), now(), $3)
		RETURNING id::text`, testWorkspaceID, f.prNumber, signersTestHead).Scan(&prID); err != nil {
		t.Fatalf("seed PR: %v", err)
	}
	if link {
		dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, f.issueID, prID)
	}
	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previous)
	dbfx.Exec(t, `UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'::jsonb), '{github_review_authors}',
		COALESCE(settings->'github_review_authors', '{}'::jsonb) || jsonb_build_object('acme/widget#' || $2::text, $3::text))
		WHERE id = $1`, testWorkspaceID, strconv.Itoa(f.prNumber), f.author)
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prID)
		testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE id = $1`, prID)
		testPool.Exec(ctx, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(previous))
	})
	return f
}

func (f signersTestFixture) pass(t *testing.T, at time.Time) string {
	t.Helper()
	return dbfx.Comment(t, f.issueID, "**Verdict: PASS** — PR #"+strconv.Itoa(f.prNumber)+", head `"+signersTestHead+"`.\n\nReviewer: signers-endpoint-reviewer.", testutil.Cols{
		"author_type": "agent", "author_id": f.reviewer, "source_task_id": f.revTask, "created_at": at,
	})
}

func (f signersTestFixture) query(repo, head, issue string) url.Values {
	return url.Values{"repo": {repo}, "pr": {strconv.Itoa(f.prNumber)}, "head": {head}, "issue": {issue}}
}

func getSigners(t *testing.T, req *http.Request) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.GetPullRequestSigners, req)
}

func memberSignersRequest(q url.Values) *http.Request {
	return newRequest(http.MethodGet, "/api/github/pull-requests/signers?"+q.Encode(), nil)
}

func TestGetPullRequestSignersReturnsTheContractShape(t *testing.T) {
	f := newSignersFixture(t, true)
	commentID := f.pass(t, time.Now().Add(-time.Hour))

	var got signersTestBody
	getSigners(t, memberSignersRequest(f.query("acme/widget", signersTestHead, f.issueID))).Want(http.StatusOK).JSON(&got)

	if got.Repo != "acme/widget" || got.PR != f.prNumber || got.HeadSHA != signersTestHead || got.IssueID != f.issueID || !got.Linked {
		t.Fatalf("echo fields = %+v, want repo, pr, head_sha, issue_id and linked=true", got)
	}
	if !got.CommentProtection {
		t.Fatal("comment_protection = false, want true with the real S1 helper")
	}
	if got.PRAuthor == nil || got.PRAuthor.AgentID != f.author || got.PRAuthor.AgentName != "signers-endpoint-author" {
		t.Fatalf("pr_author = %+v, want the bound author agent", got.PRAuthor)
	}
	if len(got.Signers) != 1 || got.Signers[0].AgentID != f.reviewer || got.Signers[0].AgentName != "signers-endpoint-reviewer" || got.Signers[0].CommentID != commentID || got.Signers[0].VerdictAt == "" {
		t.Fatalf("signers = %+v, want the reviewer with its PASS comment %s", got.Signers, commentID)
	}
}

func TestGetPullRequestSignersAcceptsAnIssueIdentifier(t *testing.T) {
	f := newSignersFixture(t, true)
	f.pass(t, time.Now().Add(-time.Hour))
	var number int32
	var prefix string
	dbfx.QueryRow(t, `SELECT i.number, w.issue_prefix FROM issue i JOIN workspace w ON w.id = i.workspace_id WHERE i.id = $1`, f.issueID).Scan(&number, &prefix)

	var got signersTestBody
	getSigners(t, memberSignersRequest(f.query("acme/widget", signersTestHead, prefix+"-"+strconv.Itoa(int(number))))).Want(http.StatusOK).JSON(&got)
	if got.IssueID != f.issueID || len(got.Signers) != 1 {
		t.Fatalf("by identifier: issue_id = %s, signers = %+v, want %s with 1 signer", got.IssueID, got.Signers, f.issueID)
	}
}

func TestGetPullRequestSignersWithNoPassReturnsAnEmptyList(t *testing.T) {
	f := newSignersFixture(t, true)
	rec := getSigners(t, memberSignersRequest(f.query("acme/widget", signersTestHead, f.issueID))).Want(http.StatusOK)
	if body := rec.Map(); body["signers"] == nil {
		t.Fatalf("signers = null in %s, want []", rec.Text())
	}
}

func TestGetPullRequestSignersReadableByAgentTokenAndMemberToken(t *testing.T) {
	f := newSignersFixture(t, true)
	f.pass(t, time.Now().Add(-time.Hour))
	askerTask := createHandlerTestTaskForAgentOnIssue(t, f.author, f.issueID)
	q := f.query("acme/widget", signersTestHead, f.issueID)

	var asMember, asAgentBody signersTestBody
	getSigners(t, memberSignersRequest(q)).Want(http.StatusOK).JSON(&asMember)
	getSigners(t, asAgent(memberSignersRequest(q), f.author, askerTask)).Want(http.StatusOK).JSON(&asAgentBody)
	if len(asMember.Signers) != 1 || len(asAgentBody.Signers) != 1 || asMember.Signers[0].AgentID != asAgentBody.Signers[0].AgentID {
		t.Fatalf("member = %+v, agent = %+v, want the same single signer", asMember.Signers, asAgentBody.Signers)
	}
}

func TestGetPullRequestSignersRejectsABadHeadOrRepo(t *testing.T) {
	f := newSignersFixture(t, true)
	for name, q := range map[string]url.Values{
		"head too short":     f.query("acme/widget", "abc123", f.issueID),
		"head not hex":       f.query("acme/widget", strings.Repeat("z", 40), f.issueID),
		"head empty":         f.query("acme/widget", "", f.issueID),
		"repo without slash": f.query("widget", signersTestHead, f.issueID),
		"repo with 3 parts":  f.query("a/b/c", signersTestHead, f.issueID),
		"repo empty":         f.query("", signersTestHead, f.issueID),
		"pr not a number": {
			"repo": {"acme/widget"}, "pr": {"x"}, "head": {signersTestHead}, "issue": {f.issueID},
		},
		"pr zero": {
			"repo": {"acme/widget"}, "pr": {"0"}, "head": {signersTestHead}, "issue": {f.issueID},
		},
		"issue missing": f.query("acme/widget", signersTestHead, ""),
	} {
		t.Run(name, func(t *testing.T) {
			getSigners(t, memberSignersRequest(q)).Want(http.StatusBadRequest)
		})
	}
}

func TestGetPullRequestSignersIs404WhenThePRIsNotStored(t *testing.T) {
	f := newSignersFixture(t, true)
	q := f.query("acme/widget", signersTestHead, f.issueID)
	q.Set("pr", strconv.Itoa(f.prNumber+500000))
	getSigners(t, memberSignersRequest(q)).Want(http.StatusNotFound)
	getSigners(t, memberSignersRequest(f.query("acme/other", signersTestHead, f.issueID))).Want(http.StatusNotFound)
}

func TestGetPullRequestSignersIs404WhenTheIssueDoesNotExist(t *testing.T) {
	f := newSignersFixture(t, true)
	getSigners(t, memberSignersRequest(f.query("acme/widget", signersTestHead, "00000000-0000-4000-8000-000000000000"))).Want(http.StatusNotFound)
}

func TestGetPullRequestSignersIs409WhenTheIssueIsNotLinkedToThePR(t *testing.T) {
	f := newSignersFixture(t, false)
	f.pass(t, time.Now().Add(-time.Hour))
	getSigners(t, memberSignersRequest(f.query("acme/widget", signersTestHead, f.issueID))).Want(http.StatusConflict)
}
