package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func linkStatusGuardPR(t *testing.T, issueID, provider, state string, merged bool) string {
	t.Helper()
	prURL := "https://github.com/cheese-work/multica/pull/2148"
	columns := testutil.Cols{
		"workspace_id": testWorkspaceID,
		"repo_owner":   "cheese-work", "repo_name": "multica", "pr_number": 2148,
		"title": "Status guard regression", "state": state, "html_url": prURL,
		"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()"),
	}
	if merged {
		columns["merged_at"] = testutil.Raw("now()")
	}
	prTable, linkTable := "github_pull_request", "issue_pull_request"
	if provider == "github" {
		columns["installation_id"] = 2148
	} else {
		prURL = "https://forge.example/cheese-work/multica/pulls/2148"
		columns["html_url"] = prURL
		columns["provider"] = provider
		columns["connection_id"] = uuid.NewString()
		prTable, linkTable = "vcs_pull_request", "issue_vcs_pull_request"
	}
	prID := dbfx.Insert(t, prTable, columns)
	dbfx.InsertNoID(t, linkTable, testutil.Cols{
		"issue_id": issueID, "pull_request_id": prID, "linked_by_type": "member",
	}, "issue_id = $1 AND pull_request_id = $2", issueID, prID)
	return prURL
}

func TestAgentIssueStatusGuard(t *testing.T) {
	createTestCustomStatus(t, "agent_accepted", "done")
	createTestCustomStatus(t, "approved", "done")
	createTestCustomStatus(t, "shipped", "done")
	agentID := dbfx.Agent(t, "status guard actor", testRuntimeID)
	cases := []struct {
		name, actor, target, provider, prState string
		merged                                 bool
		wantCode                               int
	}{
		{"agent approval without PR", "agent", "approved", "", "", false, http.StatusForbidden},
		{"agent approval with open PR", "agent", "approved", "github", "open", false, http.StatusForbidden},
		{"agent done with open PR", "agent", "done", "github", "open", false, http.StatusConflict},
		{"agent accepted with draft PR", "agent", "agent_accepted", "github", "draft", false, http.StatusConflict},
		{"agent custom done with open PR", "agent", "shipped", "github", "open", false, http.StatusConflict},
		{"agent done with self-hosted PR", "agent", "done", "forgejo", "open", false, http.StatusConflict},
		{"agent accepted with self-hosted draft", "agent", "agent_accepted", "gitlab", "draft", false, http.StatusConflict},
		{"agent done without PR", "agent", "done", "", "", false, http.StatusOK},
		{"agent accepted without PR", "agent", "agent_accepted", "", "", false, http.StatusOK},
		{"agent done with closed PR", "agent", "done", "github", "closed", false, http.StatusOK},
		{"agent accepted with merged PR", "agent", "agent_accepted", "github", "merged", true, http.StatusOK},
		{"agent done with merged timestamp", "agent", "done", "github", "open", true, http.StatusOK},
		{"agent done with closed self-hosted PR", "agent", "done", "gitea", "closed", false, http.StatusOK},
		{"agent review with open PR", "agent", "in_review", "github", "open", false, http.StatusOK},
		{"agent cancel with open PR", "agent", "cancelled", "github", "open", false, http.StatusOK},
		{"member approval without PR", "member", "approved", "", "", false, http.StatusOK},
		{"member approval with open PR", "member", "approved", "github", "open", false, http.StatusOK},
		{"member done with draft PR", "member", "done", "github", "draft", false, http.StatusOK},
		{"member accepted with open PR", "member", "agent_accepted", "forgejo", "open", false, http.StatusOK},
	}
	for _, writer := range issueWriters {
		for _, withDescription := range []bool{false, true} {
			for _, testCase := range cases {
				t.Run(fmt.Sprintf("%s/description=%t/%s", writer.name, withDescription, testCase.name), func(t *testing.T) {
					issueID := dbfx.Issue(t, "status guard original", testutil.Cols{"status": "in_progress"})
					prURL := ""
					if testCase.provider != "" {
						prURL = linkStatusGuardPR(t, issueID, testCase.provider, testCase.prState, testCase.merged)
					}
					headers := map[string]string{}
					if testCase.actor == "agent" {
						headers = map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": agentID}
					}
					body := map[string]any{"status": testCase.target, "title": "status guard changed", "suppress_run": true}
					if withDescription {
						body["description"] = "status guard description"
					}
					wantCode := testCase.wantCode
					if writer.name == "batch" {
						wantCode = http.StatusOK
					}
					response := writer.write(t, issueID, body, headers).Want(wantCode)
					if writer.name == "batch" {
						var result struct {
							Updated    int      `json:"updated"`
							UpdatedIDs []string `json:"updated_ids"`
						}
						response.JSON(&result)
						if testCase.wantCode == http.StatusOK {
							if result.Updated != 1 || len(result.UpdatedIDs) != 1 || result.UpdatedIDs[0] != issueID {
								t.Fatalf("allowed batch write must report issue %s as updated: %+v", issueID, result)
							}
						} else if result.Updated != 0 || len(result.UpdatedIDs) != 0 {
							t.Fatalf("denied batch write must report no updates: %+v", result)
						}
					}
					if testCase.wantCode != http.StatusOK {
						if writer.name == "single" && prURL != "" && !strings.Contains(response.Text(), prURL) {
							t.Fatalf("error must identify the blocking PR %s: %s", prURL, response.Text())
						}
						if writer.name == "single" && testCase.target == "approved" && !strings.Contains(response.Text(), "only members") {
							t.Fatalf("approval error must identify member-only control: %s", response.Text())
						}
						wantIssueStatus(t, issueID, "in_progress")
						var title string
						dbfx.QueryRow(t, "SELECT title FROM issue WHERE id = $1", issueID).Scan(&title)
						if title != "status guard original" {
							t.Fatalf("rejected write changed title to %q", title)
						}
						return
					}
					wantIssueStatus(t, issueID, testCase.target)
				})
			}
		}
	}
}

func TestAgentIssueStatusGuardMixedBatch(t *testing.T) {
	agentID := dbfx.Agent(t, "mixed batch status guard actor", testRuntimeID)
	for _, provider := range []string{"github", "forgejo"} {
		for _, blockedFirst := range []bool{false, true} {
			for _, withDescription := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/blocked_first=%t/description=%t", provider, blockedFirst, withDescription), func(t *testing.T) {
					cleanID := dbfx.Issue(t, "mixed batch clean", testutil.Cols{"status": "in_progress"})
					blockedID := dbfx.Issue(t, "mixed batch blocked", testutil.Cols{
						"status": "in_progress", "description": "mixed batch original",
					})
					linkStatusGuardPR(t, blockedID, provider, "open", false)
					issueIDs := []string{cleanID, blockedID}
					if blockedFirst {
						issueIDs = []string{blockedID, cleanID}
					}
					updates := map[string]any{"status": "done", "title": "mixed batch changed", "suppress_run": true}
					if withDescription {
						updates["description"] = "mixed batch changed"
					}
					request := newRequest(http.MethodPost, "/api/issues/batch?workspace_id="+testWorkspaceID, map[string]any{
						"issue_ids": issueIDs, "updates": updates,
					})
					request.Header.Set("X-Actor-Source", "task_token")
					request.Header.Set("X-Agent-ID", agentID)
					var result struct {
						Updated    int      `json:"updated"`
						UpdatedIDs []string `json:"updated_ids"`
					}
					testutil.Call(t, testHandler.BatchUpdateIssues, request).Want(http.StatusOK).JSON(&result)
					if result.Updated != 1 || len(result.UpdatedIDs) != 1 || result.UpdatedIDs[0] != cleanID {
						t.Fatalf("mixed batch must report only clean issue %s as updated: %+v", cleanID, result)
					}
					wantIssueStatus(t, cleanID, "done")
					wantIssueStatus(t, blockedID, "in_progress")
					var title, description string
					dbfx.QueryRow(t, "SELECT title, description FROM issue WHERE id = $1", blockedID).Scan(&title, &description)
					if title != "mixed batch blocked" || description != "mixed batch original" {
						t.Fatalf("denied issue changed: title=%q description=%q", title, description)
					}
				})
			}
		}
	}
}

func TestAgentIssueStatusGuardCreateApproved(t *testing.T) {
	createTestCustomStatus(t, "approved", "done")
	agentID := dbfx.Agent(t, "approval create actor", testRuntimeID)
	for _, actor := range []string{"agent", "member"} {
		t.Run(actor, func(t *testing.T) {
			title := "approval create " + actor
			request := newRequest(http.MethodPost, "/api/issues", map[string]any{
				"title": title, "status": "approved", "suppress_run": true,
			})
			wantCode := http.StatusCreated
			if actor == "agent" {
				request.Header.Set("X-Actor-Source", "task_token")
				request.Header.Set("X-Agent-ID", agentID)
				wantCode = http.StatusForbidden
			}
			response := testutil.Call(t, testHandler.CreateIssue, request).Want(wantCode)
			if actor == "agent" && !strings.Contains(response.Text(), "only members") {
				t.Fatalf("approval error must identify member-only control: %s", response.Text())
			}
			if actor == "agent" && dbfx.Count(t, "SELECT count(*) FROM issue WHERE workspace_id = $1 AND title = $2", testWorkspaceID, title) != 0 {
				t.Fatal("rejected approval create persisted an issue")
			}
			dbfx.Cleanup(t, "DELETE FROM issue WHERE workspace_id = $1 AND title = $2", testWorkspaceID, title)
		})
	}
}
