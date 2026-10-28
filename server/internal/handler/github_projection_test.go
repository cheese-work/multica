package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func newGitHubProjectionFixture(test *testing.T) (githubReviewFixture, string) {
	test.Helper()
	fixture := newGitHubIntakeFixture(test)
	project := dbfx.Project(test, "GitHub projection")
	dbfx.Exec(test, `UPDATE issue SET project_id=$1 WHERE id=$2`, project, fixture.issueID)
	dbfx.Exec(test, `INSERT INTO github_route_grant(project_id,user_id) VALUES($1,$2)`, project, testUserID)
	test.Cleanup(func() {
		dbfx.Exec(test, `DELETE FROM github_route_grant WHERE project_id=$1`, project)
		dbfx.Exec(test, `DELETE FROM github_webhook_dispatch WHERE workspace_id=$1`, testWorkspaceID)
	})
	fixture.payload["review"].(map[string]any)["node_id"] = "review-node"
	fixture.payload["review"].(map[string]any)["body"] = "UNTRUSTED BODY [@Other](mention://agent/" + fixture.other + ")"
	transport := http.DefaultTransport
	http.DefaultTransport = githubReviewTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/graphql" {
			return transport.RoundTrip(request)
		}
		var query struct {
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(request.Body).Decode(&query); err != nil {
			return nil, err
		}
		typeName, object := "PullRequestReview", fixture.payload["review"].(map[string]any)
		if comment, exists := fixture.payload["comment"]; exists {
			object = comment.(map[string]any)
			typeName = "PullRequestReviewComment"
			if _, exists := fixture.payload["issue"]; exists {
				typeName = "IssueComment"
			}
		}
		node := map[string]any{"id": query.Variables["id"], "databaseId": object["id"], "__typename": typeName,
			"body": object["body"], "editor": nil, "lastEditedAt": nil, "state": "CHANGES_REQUESTED"}
		if fixture.payload["action"] == "edited" {
			node["editor"] = map[string]any{"__typename": "User", "databaseId": 42}
			node["lastEditedAt"] = "2026-10-26T08:03:00Z"
		}
		body, err := json.Marshal(map[string]any{"data": map[string]any{"node": node}})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	return fixture, project
}

func projectionWorker(fixture githubReviewFixture) githubIntakeWorker {
	return githubIntakeWorker{handler: fixture.handler, admit: fixture.handler.admitGitHubIntake}
}

func assertGitHubProjection(test *testing.T, fixture githubReviewFixture, delivery, event, anchor string) {
	test.Helper()
	var content, authorType string
	dbfx.QueryRow(test, `SELECT content,author_type FROM comment WHERE issue_id=$1`, fixture.issueID).Scan(&content, &authorType)
	want := fmt.Sprintf("GitHub %s\nhttps://github.com/acme/widget/pull/2019#%s\nAuthor: reviewer (42)\nObject ID: 77\nIntake ID: %s", event, anchor, delivery)
	if content != want || authorType != "system" {
		test.Fatalf("notify-only comment = %q by %q, want %q by system", content, authorType, want)
	}
	if strings.Contains(content, "mention://") || strings.Contains(content, "UNTRUSTED BODY") {
		test.Fatalf("GitHub text or mention escaped into issue history: %s", content)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID); count != 0 {
		test.Fatalf("notify-only projection started %d runs", count)
	}
}

func TestGitHubProjectionT10LinksAndIDsOnly(test *testing.T) {
	for _, scenario := range []struct{ event, anchor string }{
		{"pull_request_review", "pullrequestreview-77"},
		{"pull_request_review_comment", "discussion_r77"},
		{"issue_comment", "issuecomment-77"},
	} {
		test.Run(scenario.event, func(test *testing.T) {
			fixture, _ := newGitHubProjectionFixture(test)
			if scenario.event != "pull_request_review" {
				fixture.payload["comment"] = fixture.payload["review"]
				fixture.payload["action"] = "created"
			}
			if scenario.event == "issue_comment" {
				fixture.payload["issue"] = map[string]any{"number": 2019, "pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widget/pulls/2019"}}
			}
			delivery := fixture.issueID + "-T10"
			if response := fixture.post(scenario.event, fixture.secret, delivery); response.Code != http.StatusAccepted {
				test.Fatalf("intake = %d: %s", response.Code, response.Body.String())
			}
			assertGitHubIntakeNoEffects(test, fixture)
			if _, err := projectionWorker(fixture).runOnce(context.Background()); err != nil {
				test.Fatal(err)
			}
			assertIntakeWorkerState(test, delivery, "projected")
			assertGitHubProjection(test, fixture, delivery, scenario.event, scenario.anchor)
		})
	}
}

func TestGitHubProjectionT19RevokedInsideDispatch(test *testing.T) {
	fixture, project := newGitHubProjectionFixture(test)
	delivery := postIntakeForWorker(test, fixture, "-T19")
	worker := projectionWorker(fixture)
	worker.dispatchEnabled = true
	admit := worker.admit
	worker.admit = func(ctx context.Context, queries *db.Queries, revision db.GithubWebhookRevision, payload githubIntakePayload) (githubIntakeDecision, error) {
		decision, err := admit(ctx, queries, revision, payload)
		if err != nil {
			return githubIntakeDecision{}, err
		}
		if len(decision.Targets) != 1 || decision.Targets[0].Route.Verdict != routeAllowed {
			test.Fatalf("first route check = %+v", decision)
		}
		if _, err := queries.DeleteGitHubRouteGrant(ctx, db.DeleteGitHubRouteGrantParams{ProjectID: parseUUID(project), UserID: parseUUID(testUserID)}); err != nil {
			return githubIntakeDecision{}, err
		}
		return decision, nil
	}
	if _, err := worker.runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	assertIntakeWorkerState(test, delivery, "denied")
	assertGitHubIntakeNoEffects(test, fixture)
}

func TestGitHubProjectionT21PerIssueGrant(test *testing.T) {
	fixture, _ := newGitHubProjectionFixture(test)
	otherProject := dbfx.Project(test, "No routing grant")
	otherIssue := dbfx.Issue(test, "Unpermitted linked issue", testutil.Cols{"project_id": otherProject})
	dbfx.Exec(test, `INSERT INTO issue_pull_request(issue_id,pull_request_id)
SELECT $1,pull_request_id FROM issue_pull_request WHERE issue_id=$2`, otherIssue, fixture.issueID)
	test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM issue_pull_request WHERE issue_id=$1`, otherIssue) })
	delivery := postIntakeForWorker(test, fixture, "-T21")
	if _, err := projectionWorker(fixture).runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	assertIntakeWorkerState(test, delivery, "projected")
	assertGitHubProjection(test, fixture, delivery, "pull_request_review", "pullrequestreview-77")
	if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id=$1`, otherIssue); count != 0 {
		test.Fatalf("ungranted issue received %d comments", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, otherIssue); count != 0 {
		test.Fatalf("ungranted issue received %d runs", count)
	}
	var deniedReason string
	dbfx.QueryRow(test, `SELECT reason FROM github_webhook_dispatch WHERE issue_id=$1 AND state='denied'`, otherIssue).Scan(&deniedReason)
	if deniedReason != "no grant for project" {
		test.Fatalf("ungranted issue reason = %q", deniedReason)
	}
}

func TestGitHubProjectionCommittedReceiptDedup(test *testing.T) {
	fixture, _ := newGitHubProjectionFixture(test)
	delivery := postIntakeForWorker(test, fixture, "-duplicate")
	worker := projectionWorker(fixture)
	for attempt := 0; attempt < 2; attempt++ {
		dbfx.Exec(test, `UPDATE github_webhook_intake SET state='deferred',next_attempt_at=now() WHERE delivery_guid=$1`, delivery)
		dbfx.Exec(test, `UPDATE github_webhook_revision SET state='deferred' WHERE source_delivery_guid=$1`, delivery)
		if _, err := worker.runOnce(context.Background()); err != nil {
			test.Fatal(err)
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id=$1`, fixture.issueID); count != 1 {
		test.Fatalf("duplicate delivery produced %d comments, want 1", count)
	}
}

func TestGitHubProjectionCannotTriggerCommentWakeup(test *testing.T) {
	fixture, _ := newGitHubProjectionFixture(test)
	wakeupID := subscribeToIssueEvent(test, fixture.issueID, fixture.author, "comment.created")
	postIntakeForWorker(test, fixture, "-no-wakeup")
	if _, err := projectionWorker(fixture).runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id=$1`, fixture.issueID); count != 1 {
		test.Fatalf("notify-only comments = %d, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1`, wakeupID); count != 0 {
		test.Fatalf("notify-only projection captured %d executable wakeups", count)
	}
	dbfx.Exec(test, `INSERT INTO comment(issue_id,workspace_id,author_type,author_id,content,type)
VALUES($1,$2,'member',$3,'ordinary human comment','comment')`, fixture.issueID, testWorkspaceID, testUserID)
	if count := dbfx.Count(test, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1`, wakeupID); count != 1 {
		test.Fatalf("negative control: normal comment captured %d wakeups, want 1", count)
	}
}

func TestGitHubProjectionSameBodySelfEditIsNewRevision(test *testing.T) {
	fixture, _ := newGitHubProjectionFixture(test)
	postIntakeForWorker(test, fixture, "-initial")
	worker := projectionWorker(fixture)
	if _, err := worker.runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	fixture.payload["action"] = "edited"
	fixture.payload["review"].(map[string]any)["updated_at"] = "2026-10-26T08:03:00Z"
	delivery := postIntakeForWorker(test, fixture, "-self-edit")
	if _, err := worker.runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	assertIntakeWorkerState(test, delivery, "projected")
	if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id=$1`, fixture.issueID); count != 2 {
		test.Fatalf("same-body self-edit produced %d comments, want 2 independently admitted revisions", count)
	}
}

func TestGitHubProjectionOtherItemsPostNothing(test *testing.T) {
	for _, scenario := range []struct{ name, state string }{
		{"revoked grant", "denied"}, {"foreign editor", "quarantined"}, {"API outage", "deferred"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture, project := newGitHubProjectionFixture(test)
			switch scenario.name {
			case "revoked grant":
				dbfx.Exec(test, `DELETE FROM github_route_grant WHERE project_id=$1`, project)
			case "foreign editor":
				fixture.payload["sender"].(map[string]any)["id"] = 43
			case "API outage":
				fixture.handler.PRRefresh = nil
			}
			delivery := postIntakeForWorker(test, fixture, "-other")
			if _, err := projectionWorker(fixture).runOnce(context.Background()); err != nil {
				test.Fatal(err)
			}
			assertIntakeWorkerState(test, delivery, scenario.state)
			assertGitHubIntakeNoEffects(test, fixture)
		})
	}
}

type githubProjectionLookupStarter struct {
	txStarter
	skip int
}

type githubProjectionLookupTx struct {
	pgx.Tx
	skip *int
}

func (starter githubProjectionLookupStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.txStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return githubProjectionLookupTx{Tx: transaction, skip: &starter.skip}, nil
}

func (transaction githubProjectionLookupTx) Begin(ctx context.Context) (pgx.Tx, error) {
	nested, err := transaction.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return githubProjectionLookupTx{Tx: nested, skip: transaction.skip}, nil
}

func (transaction githubProjectionLookupTx) QueryRow(ctx context.Context, statement string, args ...any) pgx.Row {
	return failingDB{DBTX: transaction.Tx, match: "FROM github_route_grant", skip: transaction.skip}.QueryRow(ctx, statement, args...)
}

func TestGitHubProjectionLookupFailureDefersAtBothChecks(test *testing.T) {
	for skip := 0; skip < 2; skip++ {
		test.Run(fmt.Sprintf("check-%d", skip+1), func(test *testing.T) {
			fixture, _ := newGitHubProjectionFixture(test)
			delivery := postIntakeForWorker(test, fixture, "-lookup-failure")
			fixture.handler.TxStarter = githubProjectionLookupStarter{txStarter: fixture.handler.TxStarter, skip: skip}
			if _, err := projectionWorker(fixture).runOnce(context.Background()); err != nil {
				test.Fatal(err)
			}
			assertIntakeWorkerState(test, delivery, "deferred")
			assertGitHubIntakeNoEffects(test, fixture)
			if count := dbfx.Count(test, `SELECT count(*) FROM github_webhook_dispatch WHERE issue_id=$1`, fixture.issueID); count != 0 {
				test.Fatalf("lookup failure committed %d terminal receipts", count)
			}
		})
	}
}

func TestGitHubProjectionIssueCommentUsesSignedPRMetadata(test *testing.T) {
	fixture, _ := newGitHubProjectionFixture(test)
	fixture.payload["comment"] = fixture.payload["review"]
	fixture.payload["action"] = "created"
	fixture.payload["issue"] = map[string]any{"number": 2019, "pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widget/pulls/2019"}}
	delete(fixture.payload, "pull_request")
	delivery := fixture.issueID + "-real-issue-comment"
	if response := fixture.post("issue_comment", fixture.secret, delivery); response.Code != http.StatusAccepted {
		test.Fatalf("intake = %d: %s", response.Code, response.Body.String())
	}
	if _, err := projectionWorker(fixture).runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	assertIntakeWorkerState(test, delivery, "projected")
	assertGitHubProjection(test, fixture, delivery, "issue_comment", "issuecomment-77")
}
