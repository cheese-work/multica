package handler

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestManualPullRequestLinkAtomicityAndRetry(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		for _, existing := range []bool{false, true} {
			for _, failure := range []string{"healthy", "exclusion_delete", "commit"} {
				t.Run(fmt.Sprintf("%s/existing_%t/%s", provider, existing, failure), func(t *testing.T) {
					issueID := dbfx.Issue(t, "manual link retry", testutil.Cols{"status": "in_progress"})
					linkMergedGitHubPRForTest(t, issueID, "manual-closing")
					prTable, joinTable := "github_pull_request", "issue_pull_request"
					cols := testutil.Cols{
						"workspace_id": testWorkspaceID, "repo_owner": "acme", "repo_name": "manual-target",
						"pr_number": 99, "title": "merged related work", "state": "merged",
						"html_url":      "https://example.test/acme/manual-target/pull/99",
						"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()"),
					}
					if provider == "github" {
						cols["installation_id"] = 817
					} else {
						prTable, joinTable = "vcs_pull_request", "issue_vcs_pull_request"
						cols["provider"] = provider
						cols["connection_id"] = dbfx.Insert(t, "vcs_connection", testutil.Cols{
							"workspace_id": testWorkspaceID, "provider": provider, "instance_url": "https://example.test",
							"account_login": "test", "access_token_encrypted": "unused", "webhook_secret_encrypted": "unused",
						})
					}
					prID := dbfx.Insert(t, prTable, cols)
					dbfx.Cleanup(t, "DELETE FROM "+joinTable+" WHERE issue_id = $1", issueID)
					if existing {
						dbfx.InsertNoID(t, joinTable, testutil.Cols{
							"issue_id": issueID, "pull_request_id": prID, "linked_by_type": "system", "close_intent": false,
						}, "issue_id = $1 AND pull_request_id = $2", issueID, prID)
					}
					dbfx.InsertNoID(t, "issue_pull_request_exclusion", testutil.Cols{
						"issue_id": issueID, "pull_request_id": prID, "workspace_id": testWorkspaceID,
						"excluded_by_type": "member", "excluded_by_id": testUserID,
					}, "issue_id = $1 AND pull_request_id = $2", issueID, prID)
					handler := *testHandler
					handler.Bus = events.New()
					var published []string
					handler.Bus.SubscribeAll(func(event events.Event) {
						if event.Type != protocol.EventIssueUpdated && event.Type != protocol.EventPullRequestUpdated {
							return
						}
						published = append(published, event.Type)
						var committed bool
						dbfx.QueryRow(t, "SELECT EXISTS (SELECT 1 FROM "+joinTable+" WHERE issue_id = $1 AND pull_request_id = $2 AND linked_by_type = 'member') AND NOT EXISTS (SELECT 1 FROM issue_pull_request_exclusion WHERE issue_id = $1 AND pull_request_id = $2)", issueID, prID).Scan(&committed)
						if !committed {
							t.Errorf("published %s before link and exclusion changes committed", event.Type)
						}
					})
					link := func() *testutil.Response {
						req := withURLParam(newRequest("POST", "/api/issues/"+issueID+"/pull-requests", map[string]any{"pull_request_id": prID}), "id", issueID)
						return testutil.Call(t, handler.LinkIssuePullRequest, req)
					}
					if failure != "healthy" {
						dbfx.Exec(t, `CREATE FUNCTION pr_manual_link_fail_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'transient manual link failure' USING ERRCODE = '40001'; END $$`)
						dbfx.Cleanup(t, `DROP FUNCTION IF EXISTS pr_manual_link_fail_write() CASCADE`)
						if failure == "exclusion_delete" {
							dbfx.Exec(t, fmt.Sprintf(`CREATE TRIGGER pr_manual_link_fail_write BEFORE DELETE ON issue_pull_request_exclusion FOR EACH ROW WHEN (OLD.issue_id = '%s') EXECUTE FUNCTION pr_manual_link_fail_write()`, issueID))
						} else {
							dbfx.Exec(t, fmt.Sprintf(`CREATE CONSTRAINT TRIGGER pr_manual_link_fail_write AFTER INSERT OR UPDATE ON %s DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.issue_id = '%s') EXECUTE FUNCTION pr_manual_link_fail_write()`, joinTable, issueID))
						}
						link().Want(http.StatusInternalServerError)
						var manual, automatic, excluded int
						dbfx.QueryRow(t, "SELECT count(*) FILTER (WHERE linked_by_type = 'member'), count(*) FILTER (WHERE linked_by_type = 'system') FROM "+joinTable+" WHERE issue_id = $1 AND pull_request_id = $2", issueID, prID).Scan(&manual, &automatic)
						dbfx.QueryRow(t, "SELECT count(*) FROM issue_pull_request_exclusion WHERE issue_id = $1 AND pull_request_id = $2", issueID, prID).Scan(&excluded)
						wantAutomatic := 0
						if existing {
							wantAutomatic = 1
						}
						if manual != 0 || automatic != wantAutomatic || excluded != 1 {
							t.Errorf("failed link persisted changes: manual/automatic/excluded = %d/%d/%d, want 0/%d/1", manual, automatic, excluded, wantAutomatic)
						}
						if status := issueStatusForTest(t, issueID); status != "in_progress" || len(published) != 0 {
							t.Errorf("failed link completed or published: status=%s events=%v", status, published)
						}
						dbfx.Exec(t, `DROP FUNCTION pr_manual_link_fail_write() CASCADE`)
					}
					link().Want(http.StatusOK)
					var manual, excluded int
					dbfx.QueryRow(t, "SELECT count(*) FROM "+joinTable+" WHERE issue_id = $1 AND pull_request_id = $2 AND linked_by_type = 'member' AND linked_by_id = $3 AND NOT close_intent", issueID, prID, testUserID).Scan(&manual)
					dbfx.QueryRow(t, "SELECT count(*) FROM issue_pull_request_exclusion WHERE issue_id = $1 AND pull_request_id = $2", issueID, prID).Scan(&excluded)
					if manual != 1 || excluded != 0 || issueStatusForTest(t, issueID) != "done" {
						t.Errorf("healthy delivery/retry lost link or completion: manual=%d excluded=%d status=%s", manual, excluded, issueStatusForTest(t, issueID))
					}
					if len(published) != 2 || published[0] != protocol.EventIssueUpdated || published[1] != protocol.EventPullRequestUpdated {
						t.Errorf("healthy delivery/retry events = %v", published)
					}
					dbfx.Exec(t, "UPDATE issue SET status = 'in_progress' WHERE id = $1", issueID)
					published = nil
					link().Want(http.StatusOK)
					if status := issueStatusForTest(t, issueID); status != "in_progress" {
						t.Errorf("duplicate link re-completed reopened issue: %s", status)
					}
					if len(published) != 1 || published[0] != protocol.EventPullRequestUpdated {
						t.Errorf("duplicate events = %v", published)
					}
				})
			}
		}
	}
}
