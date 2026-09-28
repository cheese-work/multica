package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type prIssueLookupFailure struct {
	failAt int
	reads  int
	err    error
}

type prIssueLookupDB struct {
	db.DBTX
	failure *prIssueLookupFailure
}

func (lookup prIssueLookupDB) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, "-- name: GetIssueByNumber") {
		lookup.failure.reads++
		if lookup.failure.reads == lookup.failure.failAt {
			return errRow{err: lookup.failure.err}
		}
	}
	return lookup.DBTX.QueryRow(ctx, query, args...)
}

type prIssueLookupTx struct {
	pgx.Tx
	failure *prIssueLookupFailure
}

func (tx prIssueLookupTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	return (prIssueLookupDB{DBTX: tx.Tx, failure: tx.failure}).QueryRow(ctx, query, args...)
}

type prIssueLookupTxStarter struct {
	txStarter
	failure *prIssueLookupFailure
}

func (starter prIssueLookupTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := starter.txStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return prIssueLookupTx{Tx: tx, failure: starter.failure}, nil
}

func TestPRIssueLookupDistinguishesAbsenceFromFailure(t *testing.T) {
	transient := errors.New("transient issue lookup failure")
	for _, test := range []struct {
		name       string
		identifier string
		lookupErr  error
		wantReads  int
		wantErr    error
	}{
		{name: "malformed", identifier: "HAN-invalid"},
		{name: "other_workspace", identifier: "OTHER-1"},
		{name: "not_found", identifier: "HAN-1", lookupErr: pgx.ErrNoRows, wantReads: 1},
		{name: "wrapped_not_found", identifier: "HAN-1", lookupErr: fmt.Errorf("query: %w", pgx.ErrNoRows), wantReads: 1},
		{name: "transient", identifier: "HAN-1", lookupErr: transient, wantReads: 1, wantErr: transient},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := &prIssueLookupFailure{failAt: 1, err: test.lookupErr}
			queries := db.New(prIssueLookupDB{DBTX: testPool, failure: failure})
			_, found, err := testHandler.lookupIssueByIdentifier(context.Background(), queries, parseUUID(testWorkspaceID), "HAN", test.identifier)
			if found || !errors.Is(err, test.wantErr) || failure.reads != test.wantReads {
				t.Fatalf("found/error/reads = %t/%v/%d, want false/%v/%d", found, err, failure.reads, test.wantErr, test.wantReads)
			}
		})
	}
}

func TestPRIssueLookupFailurePreservesLinksAndRetry(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		for _, lifecycle := range []string{"edited", "merged"} {
			for _, failAt := range []int{1, 2} {
				t.Run(fmt.Sprintf("%s/%s/lookup_%d", provider, lifecycle, failAt), func(t *testing.T) {
					ctx := context.Background()
					const installationID int64 = 81799001
					const secret = vcsTestSecret
					t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
					connectionID := ""
					if provider != "github" {
						connectionID = seedVCSConnection(t, ctx, withVCSBox(t), provider, "https://forgejo.test")
						t.Cleanup(func() { cleanupVCS(ctx, "") })
					} else {
						dbfx.Insert(t, "github_installation", testutil.Cols{
							"workspace_id": testWorkspaceID, "installation_id": installationID,
							"account_login": "acme", "account_type": "User",
						})
					}
					issueID := dbfx.Issue(t, "lookup failure must not complete", testutil.Cols{"status": "in_progress"})
					issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
					if err != nil {
						t.Fatal(err)
					}
					ws, err := testHandler.Queries.GetWorkspace(ctx, issue.WorkspaceID)
					if err != nil {
						t.Fatal(err)
					}
					identifier := fmt.Sprintf("%s-%d", issuePrefixForWorkspace(ws), issue.Number)
					joinTable, prTable := "issue_pull_request", "github_pull_request"
					if provider != "github" {
						joinTable, prTable = "issue_vcs_pull_request", "vcs_pull_request"
					}
					dbfx.Cleanup(t, "DELETE FROM "+prTable+" WHERE workspace_id = $1", testWorkspaceID)
					dbfx.Cleanup(t, "DELETE FROM "+joinTable+" WHERE issue_id = $1", issueID)
					assertState := func(wantState, wantTitle, wantStatus string) {
						t.Helper()
						var links, closingLinks int
						dbfx.QueryRow(t, "SELECT count(*), count(*) FILTER (WHERE close_intent) FROM "+joinTable+" WHERE issue_id = $1", issueID).Scan(&links, &closingLinks)
						if links != 2 || closingLinks != 0 {
							t.Fatalf("links/close intent changed: %d/%d, want 2/0", links, closingLinks)
						}
						var state, title string
						dbfx.QueryRow(t, "SELECT state, title FROM "+prTable+" WHERE workspace_id = $1 AND pr_number = 2", testWorkspaceID).Scan(&state, &title)
						if state != wantState || title != wantTitle {
							t.Fatalf("PR state/title = %q/%q, want %q/%q", state, title, wantState, wantTitle)
						}
						if status := issueStatusForTest(t, issueID); status != wantStatus {
							t.Fatalf("issue status = %s, want %s", status, wantStatus)
						}
					}
					send := func(handler *Handler, number int, title, event string) *testutil.Response {
						t.Helper()
						state, action := "open", event
						merged := event == "merged"
						var mergedAt any
						if merged {
							state, action = "closed", "closed"
							mergedAt = "2026-04-29T00:00:00Z"
						}
						raw, err := json.Marshal(map[string]any{
							"action": action, "installation": map[string]any{"id": installationID},
							"repository": map[string]any{"name": "widget", "owner": map[string]any{"login": "acme", "username": "acme"}},
							"pull_request": map[string]any{
								"number": number, "title": title, "body": "Closes " + identifier,
								"state": state, "merged": merged, "html_url": fmt.Sprintf("https://example.test/acme/widget/pull/%d", number),
								"merged_at": mergedAt, "closed_at": mergedAt,
								"created_at": "2026-04-28T00:00:00Z", "updated_at": "2026-04-29T00:00:00Z",
								"head": map[string]any{"ref": "fix/lookup"},
							},
						})
						if err != nil {
							t.Fatal(err)
						}
						if provider != "github" {
							return testutil.Call(t, handler.HandleVCSWebhook, vcsWebhookReq(connectionID, map[string]string{
								"X-Gitea-Event": "pull_request", "X-Gitea-Signature": giteaSig(raw),
							}, raw))
						}
						mac := hmac.New(sha256.New, []byte(secret))
						mac.Write(raw)
						req := httptest.NewRequest("POST", "/api/webhooks/github", bytes.NewReader(raw))
						req.Header.Set("X-GitHub-Event", "pull_request")
						req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
						req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("lookup-%d-%s", number, event))
						return testutil.Call(t, handler.HandleGitHubWebhook, req)
					}
					send(testHandler, 1, identifier, "opened").Want(http.StatusAccepted)
					send(testHandler, 2, identifier, "opened").Want(http.StatusAccepted)
					send(testHandler, 1, identifier, "merged").Want(http.StatusAccepted)
					assertState("open", identifier, "in_progress")
					failure := &prIssueLookupFailure{failAt: failAt, err: errors.New("transient issue lookup failure")}
					failingHandler := *testHandler
					failingHandler.Queries = db.New(prIssueLookupDB{DBTX: testPool, failure: failure})
					failingHandler.TxStarter = prIssueLookupTxStarter{txStarter: testHandler.TxStarter, failure: failure}
					title := "Updated " + identifier
					response := send(&failingHandler, 2, title, lifecycle)
					t.Logf("failure lookup=%d: HTTP %d, reads=%d", failAt, response.Code, failure.reads)
					assertState("open", identifier, "in_progress")
					response.Want(http.StatusInternalServerError)
					if failure.reads != failAt {
						t.Fatalf("lookup failure did not abort: reads=%d, want %d", failure.reads, failAt)
					}
					send(testHandler, 2, title, lifecycle).Want(http.StatusAccepted)
					if lifecycle == "merged" {
						assertState("merged", title, "done")
						dbfx.Exec(t, "UPDATE issue SET status = 'in_progress' WHERE id = $1", issueID)
						send(testHandler, 2, title, lifecycle).Want(http.StatusAccepted)
						assertState("merged", title, "in_progress")
					} else {
						assertState("open", title, "in_progress")
						send(testHandler, 2, title, "merged").Want(http.StatusAccepted)
						assertState("merged", title, "done")
					}
				})
			}
		}
	}
}
