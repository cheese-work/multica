package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type webhookStatusCatalog struct {
	issuestatus.Querier
	reads      map[pgtype.UUID]int
	pointReads int
	fail       bool
}

func (c *webhookStatusCatalog) ListIssueStatusEntries(ctx context.Context, arg db.ListIssueStatusEntriesParams) ([]db.IssueStatus, error) {
	c.reads[arg.WorkspaceID]++
	if c.fail {
		return nil, errors.New("test catalog unavailable")
	}
	return c.Querier.ListIssueStatusEntries(ctx, arg)
}

func (c *webhookStatusCatalog) GetIssueStatusEntryByKey(ctx context.Context, arg db.GetIssueStatusEntryByKeyParams) (db.IssueStatus, error) {
	c.pointReads++
	if c.fail {
		return db.IssueStatus{}, errors.New("test catalog unavailable")
	}
	return c.Querier.GetIssueStatusEntryByKey(ctx, arg)
}

// Exercise both real mirror paths, including the PR auto-complete gate.
// Signature parsing is covered by the existing provider webhook suites.
func TestWebhookStatusResolver(t *testing.T) {
	ctx := context.Background()
	for _, provider := range []string{"github", "forgejo"} {
		t.Run(provider, func(t *testing.T) {
			for _, tc := range []struct {
				name           string
				statuses, want []string
				fail           bool
				wantReads      int
			}{
				{"custom", []string{"approved", "dropped", "review", "approved", "done"}, []string{"done", "done", "done", "done", "done"}, false, 0},
				{"builtins", []string{"done", "cancelled", "in_review"}, []string{"done", "cancelled", "done"}, false, 0},
				// Only the literal built-in done and cancelled are terminal (CHE-1990),
				// so the merge gate reads no catalog. An unresolved key is not terminal.
				{"unknown", []string{"missing", "missing"}, []string{"done", "done"}, false, 0},
				{"unavailable", []string{"review", "review"}, []string{"done", "done"}, true, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					catalog := &webhookStatusCatalog{Querier: testHandler.Queries, reads: map[pgtype.UUID]int{}, fail: tc.fail}
					h := *testHandler
					h.IssueStatusCatalog = catalog
					// The same key is terminal in one workspace and nonterminal in
					// the other; a process-wide resolver would close the wrong set.
					for workspace := 0; workspace < 2; workspace++ {
						ws := dbfx.Workspace(t, "Webhook status resolver", fmt.Sprintf("resolver-%s-%s-%d", provider, tc.name, workspace), testutil.Cols{"issue_prefix": "RSL"})
						wsID := parseUUID(ws)
						fixture := testutil.New(testPool, ws, testUserID)
						for key, category := range map[string]string{"approved": "done", "dropped": "closed", "review": "started"} {
							if workspace == 1 && key == "approved" {
								category = "started"
							}
							cols := testutil.Cols{"workspace_id": ws, "key": key, "name": key, "category": category, "color": "#123456"}
							if key == "approved" {
								cols["archived_at"] = testutil.Raw("now()")
							}
							fixture.Insert(t, "issue_status", cols)
						}
						var ids, closing []string
						for i, status := range tc.statuses {
							ids = append(ids, fixture.Issue(t, "Resolver fixture", testutil.Cols{"status": status, "number": i + 1}))
							closing = append(closing, fmt.Sprintf("Closes RSL-%d", i+1))
						}
						// Mirroring creates rows outside the fixture builders.
						for _, table := range []string{"issue_pull_request", "issue_vcs_pull_request"} {
							fixture.Cleanup(t, "DELETE FROM "+table+" WHERE issue_id IN (SELECT id FROM issue WHERE workspace_id = $1)", ws)
						}
						fixture.Cleanup(t, "DELETE FROM github_pull_request WHERE workspace_id = $1", ws)
						fixture.Cleanup(t, "DELETE FROM vcs_pull_request WHERE workspace_id = $1", ws)
						const timestamp = "2026-09-08T00:00:00Z"
						// mirror delivers a merged PR whose body closes every
						// fixture issue. Each PR number is a separate delivery.
						var mirror func(number int32)
						if provider == "github" {
							mirror = func(number int32) {
								p := &ghPullRequestPayload{}
								p.Action = "closed"
								p.Repository.ID = 424242
								p.Repository.Owner.Login, p.Repository.Name = "fixture", "resolver"
								fork := false
								p.PullRequest.Head.Repo = &ghPRHeadRepository{ID: 424242, FullName: "fixture/resolver", Fork: &fork}
								p.PullRequest.Number, p.PullRequest.Title = number, "Resolve linked issues"
								p.PullRequest.Body = strings.Join(closing, "\n")
								p.PullRequest.State, p.PullRequest.Merged = "closed", true
								p.PullRequest.HTMLURL = fmt.Sprintf("https://github.test/fixture/resolver/pull/%d", number)
								p.PullRequest.CreatedAt, p.PullRequest.UpdatedAt = timestamp, timestamp
								p.PullRequest.MergedAt = timestamp
								if err := h.mirrorPullRequestForWorkspace(ctx, wsID, int64(91000+workspace), p, prLinkPolicy{unrestricted: true}); err != nil {
									t.Fatalf("mirrorPullRequestForWorkspace: %v", err)
								}
							}
						} else {
							connID := fixture.Insert(t, "vcs_connection", testutil.Cols{"workspace_id": ws, "provider": provider, "instance_url": "https://forgejo.test", "account_login": "fixture", "access_token_encrypted": "unused", "webhook_secret_encrypted": "unused"})
							conn := db.VcsConnection{ID: parseUUID(connID), WorkspaceID: wsID, Provider: provider}
							mirror = func(number int32) {
								ev := vcs.PullRequestEvent{Action: "closed", State: "merged", RepoOwner: "fixture", RepoName: "resolver", Number: number, Title: "Resolve linked issues", Body: strings.Join(closing, "\n"), HTMLURL: fmt.Sprintf("https://forgejo.test/fixture/resolver/pulls/%d", number), CreatedAt: timestamp, UpdatedAt: timestamp}
								h.mirrorVCSPullRequest(ctx, conn, ev)
							}
						}
						mirror(1)
						if got := catalog.reads[wsID]; got != tc.wantReads {
							t.Errorf("workspace %d catalog reads = %d, want %d", workspace, got, tc.wantReads)
						}
						for i, id := range ids {
							want := tc.want[i]
							var status string
							fixture.QueryRow(t, "SELECT status FROM issue WHERE id = $1", id).Scan(&status)
							if status != want {
								t.Errorf("workspace %d issue %d status = %q, want %q", workspace, i, status, want)
							}
						}
						// A redelivery of the same merge is not a new PR event, so it
						// decides nothing. A second merged PR is a new event and moves
						// a custom closed status like any other non-built-in one.
						if tc.name == "custom" {
							fixture.Exec(t, "UPDATE issue SET status = 'dropped' WHERE id = $1", ids[0])
							mirror(1)
							var status string
							fixture.QueryRow(t, "SELECT status FROM issue WHERE id = $1", ids[0]).Scan(&status)
							if status != "dropped" {
								t.Errorf("redelivery changed status to %q", status)
							}
							mirror(2)
							fixture.QueryRow(t, "SELECT status FROM issue WHERE id = $1", ids[0]).Scan(&status)
							if status != "done" {
								t.Errorf("second merge: dropped status = %q, want done", status)
							}
						}
					}
					if catalog.pointReads != 0 {
						t.Errorf("per-key reads = %d, want 0", catalog.pointReads)
					}
				})
			}
		})
	}
}
