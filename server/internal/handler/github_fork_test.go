package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func githubForkGateCases() []struct {
	name                  string
	head                  any
	wantLink, wantTrusted int
} {
	return []struct {
		name                  string
		head                  any
		wantLink, wantTrusted int
	}{
		{"same_repository", map[string]any{"id": 10, "full_name": "acme/widget", "fork": false}, 1, 1},
		{"same_repository_case", map[string]any{"id": 10, "full_name": "ACME/Widget", "fork": false}, 1, 1},
		{"fork", map[string]any{"id": 20, "full_name": "outsider/widget", "fork": true}, 0, 0},
		{"fork_flag_on_same_repository", map[string]any{"id": 10, "full_name": "acme/widget", "fork": true}, 1, 1},
		{"different_id", map[string]any{"id": 20, "full_name": "acme/widget", "fork": false}, 0, 0},
		{"different_name", map[string]any{"id": 10, "full_name": "outsider/widget", "fork": false}, 0, 0},
		{"deleted_head_repository", nil, 0, 0},
		{"missing_id", map[string]any{"full_name": "acme/widget", "fork": false}, 0, 0},
		{"missing_name", map[string]any{"id": 10, "fork": false}, 0, 0},
		{"missing_fork_flag", map[string]any{"id": 10, "full_name": "acme/widget"}, 0, 0},
		{"same_owner_fork", sameOwnerFork(30, "acme/widget-fork", true, 100), 1, 0},
		{"same_owner_fork_renamed_login", sameOwnerFork(30, "acme-old/widget-fork", true, 100), 1, 0},
		{"same_login_other_owner_id", sameOwnerFork(30, "acme/widget-fork", true, 200), 0, 0},
		{"head_owner_missing", map[string]any{"id": 30, "full_name": "acme/widget-fork", "fork": true}, 0, 0},
		{"head_owner_id_zero", sameOwnerFork(30, "acme/widget-fork", true, 0), 0, 0},
		{"head_fork_missing_owner_match", map[string]any{"id": 30, "full_name": "acme/widget-fork", "owner": map[string]any{"id": 100}}, 0, 0},
		{"head_id_missing_owner_match", map[string]any{"full_name": "acme/widget-fork", "fork": true, "owner": map[string]any{"id": 100}}, 0, 0},
		{"head_name_missing_owner_match", map[string]any{"id": 30, "fork": true, "owner": map[string]any{"id": 100}}, 0, 0},
	}
}

// setForkGateBaseRepo makes the base acme/widget: repository id 10, owner id 100.
func setForkGateBaseRepo(payload map[string]any) {
	repo := payload["repository"].(map[string]any)
	repo["id"] = 10
	repo["owner"].(map[string]any)["id"] = 100
}

func sameOwnerFork(id int, fullName string, fork bool, ownerID int) map[string]any {
	return map[string]any{"id": id, "full_name": fullName, "fork": fork, "owner": map[string]any{"id": ownerID}}
}

func githubForkGatePR(t *testing.T, fixture githubReviewFixture, head any) map[string]any {
	t.Helper()
	var branch string
	dbfx.QueryRow(t, `SELECT branch FROM github_pull_request WHERE workspace_id=$1 AND pr_number=2019`, testWorkspaceID).Scan(&branch)
	issue, err := fixture.handler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"number": 2019, "title": fmt.Sprintf("Closes HAN-%d", issue.Number), "body": "",
		"html_url": "https://github.com/acme/widget/pull/2019", "state": "open", "draft": false, "merged": false,
		"created_at": "2026-10-27T00:00:00Z", "updated_at": time.Now().Add(time.Minute).Format(time.RFC3339Nano),
		"head": map[string]any{"ref": branch, "sha": "review-head", "repo": head},
		"base": map[string]any{"ref": "main"},
	}
}

func TestGitHubForkGateAutoLinkAndManualLink(t *testing.T) {
	for _, candidate := range githubForkGateCases() {
		t.Run(candidate.name, func(t *testing.T) {
			fixture := newGitHubReviewFixture(t)
			fixture.payload["action"] = "opened"
			setForkGateBaseRepo(fixture.payload)
			pull := githubForkGatePR(t, fixture, candidate.head)
			pull["number"] = 2020
			fixture.payload["pull_request"] = pull
			if response := fixture.post("pull_request", fixture.secret, candidate.name); response.Code != http.StatusAccepted {
				t.Fatalf("webhook: %d %s", response.Code, response.Body.String())
			}
			mirrored, err := fixture.handler.Queries.GetGitHubPullRequest(context.Background(), db.GetGitHubPullRequestParams{
				WorkspaceID: parseUUID(testWorkspaceID), RepoOwner: "acme", RepoName: "widget", PrNumber: 2020,
			})
			if err != nil {
				t.Fatal(err)
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM issue_pull_request WHERE pull_request_id=$1`, mirrored.ID); count != candidate.wantLink {
				t.Errorf("automatic links = %d, want %d", count, candidate.wantLink)
			}
			request := withURLParam(newRequest("POST", "/api/issues/"+fixture.issueID+"/pull-requests", map[string]any{"pull_request_id": uuidToString(mirrored.ID)}), "id", fixture.issueID)
			testutil.Call(t, fixture.handler.LinkIssuePullRequest, request).Want(http.StatusOK)
			fixture.payload["action"] = "edited"
			if response := fixture.post("pull_request", fixture.secret, candidate.name+"-redelivered"); response.Code != http.StatusAccepted {
				t.Fatalf("redelivery: %d %s", response.Code, response.Body.String())
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM issue_pull_request WHERE pull_request_id=$1 AND linked_by_type='member'`, mirrored.ID); count != 1 {
				t.Fatalf("member links = %d, want 1", count)
			}
		})
	}
}

func TestGitHubForkGateReviewProjection(t *testing.T) {
	for _, candidate := range githubForkGateCases() {
		t.Run(candidate.name, func(t *testing.T) {
			fixture, _ := newGitHubProjectionFixture(t)
			setForkGateBaseRepo(fixture.payload)
			fixture.payload["pull_request"] = githubForkGatePR(t, fixture, candidate.head)
			if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"-"+candidate.name); response.Code != http.StatusAccepted {
				t.Fatalf("review: %d %s", response.Code, response.Body.String())
			}
			assertGitHubIntakeNoEffects(t, fixture)
			if _, err := projectionWorker(fixture).runOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID); count != 0 {
				t.Errorf("notify-only intake started %d runs", count)
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, fixture.issueID); count != candidate.wantTrusted {
				t.Errorf("review comments = %d, want %d", count, candidate.wantTrusted)
			}
		})
	}
}

func TestGitHubForkGateExplicitAuthorBinding(t *testing.T) {
	fixture, _ := newGitHubProjectionFixture(t)
	setForkGateBaseRepo(fixture.payload)
	fixture.payload["pull_request"] = githubForkGatePR(t, fixture, map[string]any{"id": 20, "full_name": "outsider/widget", "fork": true})
	dbfx.Exec(t, `UPDATE workspace SET settings=jsonb_set(settings,'{github_review_authors}',jsonb_build_object('acme/widget#2019',$1::text)) WHERE id=$2`, fixture.author, testWorkspaceID)
	delivery := fixture.issueID + "-fork-explicit-author"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		t.Fatalf("review: %d %s", response.Code, response.Body.String())
	}
	if _, err := projectionWorker(fixture).runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "denied")
	if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID); count != 0 {
		t.Fatalf("explicit author binding bypassed fork gate: %d runs", count)
	}
}

func TestGitHubForkGateReviewIntake(test *testing.T) {
	for _, candidate := range githubForkGateCases() {
		test.Run(candidate.name, func(test *testing.T) {
			fixture := newGitHubIntakeFixture(test)
			setForkGateBaseRepo(fixture.payload)
			fixture.payload["pull_request"] = githubForkGatePR(test, fixture, candidate.head)
			delivery := fixture.issueID + "-fork-" + candidate.name
			if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
				test.Fatalf("review: %d %s", response.Code, response.Body.String())
			}
			wantState, wantReason := "denied", "fork_pull_request"
			if candidate.wantTrusted == 1 {
				wantState, wantReason = "deferred", "awaiting_admission"
			}
			switch candidate.name {
			case "deleted_head_repository", "missing_id", "missing_name", "missing_fork_flag",
				"head_fork_missing_owner_match", "head_id_missing_owner_match", "head_name_missing_owner_match":
				wantState, wantReason = "deferred", "missing_pull_request_metadata"
			}
			var state, reason string
			dbfx.QueryRow(test, `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(&state, &reason)
			if state != wantState || reason != wantReason {
				test.Errorf("revision = %s/%s, want %s/%s", state, reason, wantState, wantReason)
			}
			assertGitHubIntakeNoEffects(test, fixture)
		})
	}
}

func TestGitHubForkGateIntakeExplicitAuthorBinding(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	setForkGateBaseRepo(fixture.payload)
	fixture.payload["pull_request"] = githubForkGatePR(test, fixture, map[string]any{"id": 20, "full_name": "outsider/widget", "fork": true})
	dbfx.Exec(test, `UPDATE workspace SET settings=jsonb_set(settings,'{github_review_authors}',jsonb_build_object('acme/widget#2019',$1::text)) WHERE id=$2`, fixture.author, testWorkspaceID)
	delivery := fixture.issueID + "-fork-explicit-author"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		test.Fatalf("review: %d %s", response.Code, response.Body.String())
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID); count != 0 {
		test.Fatalf("explicit author binding bypassed fork gate: %d runs", count)
	}
	var state, reason string
	dbfx.QueryRow(test, `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(&state, &reason)
	if state != "denied" || reason != "fork_pull_request" {
		test.Fatalf("explicit author binding revision = %s/%s, want denied/fork_pull_request", state, reason)
	}
}

func TestGitHubForkGateLinkedPRWake(t *testing.T) {
	for _, event := range []string{"merged", "converted_to_draft", "snapshot_failure", "snapshot_success", "snapshot_conflict"} {
		t.Run(event, func(t *testing.T) {
			fixture := newGitHubReviewFixture(t)
			setWorkspacePRWakeSetting(t, "github_wake_on_ci_failure", true)
			setWorkspacePRWakeSetting(t, "github_wake_on_ci_success", true)
			fixture.payload["action"] = "synchronize"
			setForkGateBaseRepo(fixture.payload)
			pull := githubForkGatePR(t, fixture, map[string]any{"id": 20, "full_name": "outsider/widget", "fork": true})
			fixture.payload["pull_request"] = pull
			switch event {
			case "merged":
				fixture.payload["action"] = "closed"
				pull["merged"], pull["state"], pull["merge_commit_sha"] = true, "closed", "merge-sha"
			case "converted_to_draft":
				fixture.payload["action"], pull["draft"] = event, true
				dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, fixture.issueID)
			}
			if response := fixture.post("pull_request", fixture.secret, event); response.Code != http.StatusAccepted {
				t.Fatalf("webhook: %d %s", response.Code, response.Body.String())
			}
			if event == "snapshot_failure" || event == "snapshot_success" || event == "snapshot_conflict" {
				rollup, mergeState := "FAILURE", "CLEAN"
				if event == "snapshot_success" {
					rollup = "SUCCESS"
				}
				if event == "snapshot_conflict" {
					rollup, mergeState = "PENDING", "DIRTY"
				}
				dbfx.Exec(t, `UPDATE github_pull_request SET snapshot_head_sha=head_sha, snapshot_base_ref='main', checks_rollup_state=$1, api_merge_state_status=$2 WHERE workspace_id=$3 AND pr_number=2019`, rollup, mergeState, testWorkspaceID)
				var pullID string
				dbfx.QueryRow(t, `SELECT id FROM github_pull_request WHERE workspace_id=$1 AND pr_number=2019`, testWorkspaceID).Scan(&pullID)
				fixture.handler.broadcastPRSnapshotApplied(context.Background(), parseUUID(pullID))
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID); count != 0 {
				t.Fatalf("fork %s created %d runs", event, count)
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, fixture.issueID); count != 0 {
				t.Errorf("fork %s created %d comments", event, count)
			}
			t.Logf("fork %s: no run and no comment", event)
		})
	}
}

// postForkGatePR delivers a pull_request webhook for PR 2020 (head = head, closing the fixture issue)
// and returns the mirrored row.
func postForkGatePR(t *testing.T, fixture githubReviewFixture, action, delivery string, head any, mutate func(pull map[string]any)) db.GithubPullRequest {
	t.Helper()
	fixture.payload["action"] = action
	pull := githubForkGatePR(t, fixture, head)
	pull["number"] = 2020
	if mutate != nil {
		mutate(pull)
	}
	fixture.payload["pull_request"] = pull
	if response := fixture.post("pull_request", fixture.secret, delivery); response.Code != http.StatusAccepted {
		t.Fatalf("webhook: %d %s", response.Code, response.Body.String())
	}
	mirrored, err := fixture.handler.Queries.GetGitHubPullRequest(context.Background(), db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), RepoOwner: "acme", RepoName: "widget", PrNumber: 2020,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mirrored
}

func TestGitHubSameOwnerForkBaseSideMetadata(t *testing.T) {
	for name, mutate := range map[string]func(repo map[string]any){
		"base_owner_missing": func(repo map[string]any) { delete(repo["owner"].(map[string]any), "id") },
		"base_owner_zero":    func(repo map[string]any) { repo["owner"].(map[string]any)["id"] = 0 },
		"base_repo_missing":  func(repo map[string]any) { delete(repo, "id") },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newGitHubReviewFixture(t)
			setForkGateBaseRepo(fixture.payload)
			mutate(fixture.payload["repository"].(map[string]any))
			mirrored := postForkGatePR(t, fixture, "opened", name, sameOwnerFork(30, "acme/widget-fork", true, 100), nil)
			if count := dbfx.Count(t, `SELECT count(*) FROM issue_pull_request WHERE pull_request_id=$1`, mirrored.ID); count != 0 {
				t.Errorf("automatic links = %d, want 0", count)
			}
		})
	}
}

func TestGitHubSameOwnerForkLinksButDoesNotMove(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	setForkGateBaseRepo(fixture.payload)
	fork := sameOwnerFork(30, "acme/widget-fork", true, 100)
	mirrored := postForkGatePR(t, fixture, "opened", "same-owner-open", fork, nil)
	if mirrored.HeadRepoSame {
		t.Error("HeadRepoSame = true for a same-owner fork")
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM issue_pull_request WHERE pull_request_id=$1 AND issue_id=$2`, mirrored.ID, fixture.issueID); count != 1 {
		t.Fatalf("automatic links = %d, want 1", count)
	}
	var statusBefore string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id=$1`, fixture.issueID).Scan(&statusBefore)

	postForkGatePR(t, fixture, "closed", "same-owner-merged", fork, func(pull map[string]any) {
		pull["merged"], pull["state"], pull["merge_commit_sha"] = true, "closed", "merge-sha"
	})
	var statusAfter string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id=$1`, fixture.issueID).Scan(&statusAfter)
	if statusAfter != statusBefore {
		t.Errorf("merge moved the issue: %s -> %s", statusBefore, statusAfter)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID); count != 0 {
		t.Errorf("merge created %d runs", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, fixture.issueID); count != 0 {
		t.Errorf("merge created %d comments", count)
	}
}

func TestGitHubSameOwnerForkReconcileRemovesLink(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	setForkGateBaseRepo(fixture.payload)
	fork := sameOwnerFork(30, "acme/widget-fork", true, 100)
	mirrored := postForkGatePR(t, fixture, "opened", "same-owner-reconcile-open", fork, nil)
	if count := dbfx.Count(t, `SELECT count(*) FROM issue_pull_request WHERE pull_request_id=$1`, mirrored.ID); count != 1 {
		t.Fatalf("automatic links = %d, want 1", count)
	}
	postForkGatePR(t, fixture, "edited", "same-owner-reconcile-edit", fork, func(pull map[string]any) { pull["title"] = "No issue here" })
	if count := dbfx.Count(t, `SELECT count(*) FROM issue_pull_request WHERE pull_request_id=$1`, mirrored.ID); count != 0 {
		t.Errorf("automatic links after title edit = %d, want 0", count)
	}
}
