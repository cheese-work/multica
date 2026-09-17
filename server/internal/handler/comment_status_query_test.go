package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestParseStatusQuery is the canonical matrix for parseStatusQuery (CHE-487
// Unit D1): the five exact phrases, case-insensitively, and nothing else.
// Pure function, no DB — must always run.
func TestParseStatusQuery(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    statusQueryKind
		wantOK  bool
	}{
		{"status lower", "status", statusQueryStatus, true},
		{"status upper", "STATUS", statusQueryStatus, true},
		{"eta lower", "eta", statusQueryETA, true},
		{"eta mixed", "Eta", statusQueryETA, true},
		{"active runs exact", "active runs", statusQueryActiveRuns, true},
		{"active runs mixed case", "Active Runs", statusQueryActiveRuns, true},
		{"ci lower", "ci", statusQueryCI, true},
		{"ci padded", " Ci ", statusQueryCI, true},
		{"pr head exact", "pr head", statusQueryPRHead, true},
		{"pr head mixed", "PR Head", statusQueryPRHead, true},
		{"outer whitespace only", "  status\t\n", statusQueryStatus, true},

		{"trailing question mark", "status?", "", false},
		{"plural", "statuses", "", false},
		{"trailing instruction", "status please", "", false},
		{"leading instruction", "please status", "", false},
		{"eta with extra text", "eta for this", "", false},
		{"empty", "", "", false},
		{"whitespace only", "   ", "", false},
		{"note prefixed", "/note status", "", false},
		{"ci cd lookalike", "ci/cd", "", false},
		{"mixed status and instruction", "status and please fix the tests", "", false},
		{"what's the status question", "what's the status?", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseStatusQuery(tc.content)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("parseStatusQuery(%q) = (%q, %v), want (%q, %v)", tc.content, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// createStatusQueryTestIssue creates a bare issue for status-query tests,
// following the same shape as createCommentTriggerPreviewIssue but allowing a
// due_date to be seeded so the ETA answer can be asserted to never echo it.
func createStatusQueryTestIssue(t *testing.T, title, dueDate string) string {
	t.Helper()
	ctx := context.Background()

	var number int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1
		WHERE id = $1 RETURNING issue_counter
	`, testWorkspaceID).Scan(&number); err != nil {
		t.Fatalf("next issue number: %v", err)
	}

	var dueDateArg any
	if dueDate != "" {
		dueDateArg = dueDate
	}

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, creator_type, creator_id, title, number, due_date, last_activity_at)
		VALUES ($1, 'member', $2, $3, $4, $5, now())
		RETURNING id
	`, testWorkspaceID, testUserID, title, number, dueDateArg).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}

	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	return issueID
}

// postStatusQueryComment posts content to the issue and decodes the response.
func postStatusQueryComment(t *testing.T, issueID, content string) CommentResponse {
	t.Helper()
	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content}), "id", issueID)
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp CommentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	return resp
}

// TestCreateComment_StatusQuerySkipsAgentAdmission is the headline acceptance
// criterion: posting exactly "status" creates the comment, answers it
// deterministically, and produces ZERO agent task admission.
func TestCreateComment_StatusQuerySkipsAgentAdmission(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	// Assign an agent to the issue so a normal comment WOULD trigger it — the
	// absence of any row below is a real assertion, not a vacuous one.
	agentID := createHandlerTestAgent(t, "Status Query Assignee", nil)
	issueID := createStatusQueryTestIssue(t, "status query zero admission", "")
	if _, err := testPool.Exec(ctx, `UPDATE issue SET assignee_type = 'agent', assignee_id = $1 WHERE id = $2`, agentID, issueID); err != nil {
		t.Fatalf("assign issue: %v", err)
	}

	resp := postStatusQueryComment(t, issueID, "status")

	if resp.ID == "" {
		t.Fatal("comment was not saved")
	}
	if len(resp.TriggerOutcomes) != 0 {
		t.Fatalf("trigger_outcomes = %+v, want none (agent admission must be skipped)", resp.TriggerOutcomes)
	}
	if resp.StatusAnswer == nil {
		t.Fatal("status_answer = nil, want populated")
	}
	if resp.StatusAnswer.Kind != statusQueryStatus {
		t.Errorf("status_answer.kind = %q, want %q", resp.StatusAnswer.Kind, statusQueryStatus)
	}
	if resp.StatusAnswer.Availability != availabilityCurrent {
		t.Errorf("status_answer.availability = %q, want %q", resp.StatusAnswer.Availability, availabilityCurrent)
	}
	if resp.StatusAnswer.AnsweredAt == "" {
		t.Error("status_answer.answered_at is empty")
	}
	if resp.StatusAnswer.IssueStatus == nil || *resp.StatusAnswer.IssueStatus == "" {
		t.Error("status_answer.issue_status is empty, want the issue's status")
	}

	var taskCount int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 0 {
		t.Errorf("agent_task_queue rows for issue = %d, want 0 (zero-model receipt)", taskCount)
	}
}

// TestCreateComment_MixedStatusRequestGoesThroughNormalTriggerPath is the
// mixed-request regression: any additional instruction alongside a status
// word is NOT a status query. The comment must retain its exact content
// (status words not stripped) and go through the ordinary trigger path.
func TestCreateComment_MixedStatusRequestGoesThroughNormalTriggerPath(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	issueID := createCommentTriggerPreviewIssue(t, "mixed status request regression", "", "")
	content := "status and also please update the README"

	resp := postStatusQueryComment(t, issueID, content)

	if resp.StatusAnswer != nil {
		t.Fatalf("status_answer = %+v, want nil (mixed request must not be treated as a status query)", resp.StatusAnswer)
	}
	if resp.Content != content {
		t.Errorf("comment content = %q, want unmodified %q", resp.Content, content)
	}
	if !strings.Contains(resp.Content, "status") || !strings.Contains(resp.Content, "please update the README") {
		t.Errorf("comment content = %q, want both the status word and the instruction retained", resp.Content)
	}
}

// TestBuildStatusAnswer_ETAAlwaysUnavailable pins that ETA is ALWAYS reported
// as unknown/unavailable and NEVER echoes due_date, even when one is set on
// the issue.
func TestBuildStatusAnswer_ETAAlwaysUnavailable(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	issueID := createStatusQueryTestIssue(t, "eta never echoes due date", "2099-12-25")
	issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}

	answer := testHandler.buildStatusAnswer(ctx, issue, statusQueryETA)

	if answer.Availability != availabilityUnavailable {
		t.Errorf("availability = %q, want %q", answer.Availability, availabilityUnavailable)
	}
	if strings.Contains(answer.Detail, "2099-12-25") {
		t.Errorf("detail = %q, must not echo the issue's due_date", answer.Detail)
	}
	payload, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("marshal answer: %v", err)
	}
	if strings.Contains(string(payload), "2099-12-25") {
		t.Errorf("serialized status_answer contains the due_date: %s", payload)
	}
}

// TestBuildStatusAnswer_FiveKindsAvailabilityMatrix covers, for each of the
// five kinds, at least one current case and one unavailable case; PR-backed
// kinds additionally get a stale case.
func TestBuildStatusAnswer_FiveKindsAvailabilityMatrix(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	t.Run("status is always current", func(t *testing.T) {
		issueID := createStatusQueryTestIssue(t, "status current", "")
		issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
		if err != nil {
			t.Fatalf("get issue: %v", err)
		}
		answer := testHandler.buildStatusAnswer(ctx, issue, statusQueryStatus)
		if answer.Availability != availabilityCurrent {
			t.Errorf("availability = %q, want current", answer.Availability)
		}
		if answer.IssueStatus == nil || *answer.IssueStatus != issue.Status {
			t.Errorf("issue_status = %v, want %q", answer.IssueStatus, issue.Status)
		}
	})

	t.Run("eta is always unavailable", func(t *testing.T) {
		issueID := createStatusQueryTestIssue(t, "eta unavailable", "")
		issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
		if err != nil {
			t.Fatalf("get issue: %v", err)
		}
		answer := testHandler.buildStatusAnswer(ctx, issue, statusQueryETA)
		if answer.Availability != availabilityUnavailable {
			t.Errorf("availability = %q, want unavailable", answer.Availability)
		}
	})

	t.Run("active runs current with a queued task", func(t *testing.T) {
		agentID := createHandlerTestAgent(t, "Active Runs Current Agent", nil)
		runtimeID := handlerTestRuntimeID(t)
		issueID := createStatusQueryTestIssue(t, "active runs current", "")
		issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
		if err != nil {
			t.Fatalf("get issue: %v", err)
		}
		if _, err := testPool.Exec(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, issue_id)
			VALUES ($1, $2, 'queued', 0, $3)
		`, agentID, runtimeID, issueID); err != nil {
			t.Fatalf("seed queued task: %v", err)
		}

		answer := testHandler.buildStatusAnswer(ctx, issue, statusQueryActiveRuns)
		if answer.Availability != availabilityCurrent {
			t.Errorf("availability = %q, want current", answer.Availability)
		}
		if len(answer.ActiveRuns) != 1 {
			t.Fatalf("active_runs = %+v, want 1 entry", answer.ActiveRuns)
		}
		if answer.ActiveRuns[0].AgentID != agentID || answer.ActiveRuns[0].Status != "queued" {
			t.Errorf("active_runs[0] = %+v, want agent %s / queued", answer.ActiveRuns[0], agentID)
		}
	})

	t.Run("active runs current with none", func(t *testing.T) {
		issueID := createStatusQueryTestIssue(t, "active runs empty", "")
		issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
		if err != nil {
			t.Fatalf("get issue: %v", err)
		}
		answer := testHandler.buildStatusAnswer(ctx, issue, statusQueryActiveRuns)
		if answer.Availability != availabilityCurrent {
			t.Errorf("availability = %q, want current (empty is a valid current answer, not unavailable)", answer.Availability)
		}
		if len(answer.ActiveRuns) != 0 {
			t.Errorf("active_runs = %+v, want none", answer.ActiveRuns)
		}
	})

	t.Run("ci and pr head unavailable with no linked PR", func(t *testing.T) {
		issueID := createStatusQueryTestIssue(t, "ci unavailable no pr", "")
		issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
		if err != nil {
			t.Fatalf("get issue: %v", err)
		}
		for _, kind := range []statusQueryKind{statusQueryCI, statusQueryPRHead} {
			answer := testHandler.buildStatusAnswer(ctx, issue, kind)
			if answer.Availability != availabilityUnavailable {
				t.Errorf("kind %q availability = %q, want unavailable", kind, answer.Availability)
			}
			if answer.PullRequest != nil {
				t.Errorf("kind %q pull_request = %+v, want nil", kind, answer.PullRequest)
			}
		}
	})

	// The next two cases exercise applyStatusAnswerFromPullRequestRow directly
	// with snapshotEnabled=true, the same split
	// TestIssuePullRequestResponseHidesUnavailableSnapshot uses for
	// issuePullRequestRowToResponse: h.PRRefresh.Enabled() depends on a GitHub
	// App private key that is never configured in this test harness, so
	// routing through buildStatusAnswer (which calls h.PRRefresh.Enabled())
	// would report every PR-backed answer as unavailable regardless of the
	// seeded snapshot. The row itself still comes from the real
	// ListPullRequestsByIssue query, so the SQL aggregation is exercised.
	t.Run("ci and pr head current with a fresh snapshot", func(t *testing.T) {
		issueID, prID := seedStatusQueryPR(t, "status-query-current", 900001, "headcurrent", true)
		row := fetchStatusQueryPRRow(t, ctx, issueID)

		var ciAnswer StatusAnswer
		applyStatusAnswerFromPullRequestRow(row, true, statusQueryCI, &ciAnswer)
		if ciAnswer.Availability != availabilityCurrent {
			t.Errorf("ci availability = %q, want current", ciAnswer.Availability)
		}
		if ciAnswer.PullRequest == nil || ciAnswer.PullRequest.PullRequestID != prID {
			t.Errorf("ci pull_request = %+v, want id %q", ciAnswer.PullRequest, prID)
		}
		if ciAnswer.PullRequest.ChecksRollup == nil || *ciAnswer.PullRequest.ChecksRollup != "success" {
			t.Errorf("ci checks_rollup = %v, want success", ciAnswer.PullRequest.ChecksRollup)
		}

		var prHeadAnswer StatusAnswer
		applyStatusAnswerFromPullRequestRow(row, true, statusQueryPRHead, &prHeadAnswer)
		if prHeadAnswer.Availability != availabilityCurrent {
			t.Errorf("pr head availability = %q, want current", prHeadAnswer.Availability)
		}
		if prHeadAnswer.PullRequest == nil || prHeadAnswer.PullRequest.HeadSHA != "headcurrent" {
			t.Errorf("pr head = %+v, want head_sha headcurrent", prHeadAnswer.PullRequest)
		}
	})

	t.Run("ci and pr head stale with an old snapshot", func(t *testing.T) {
		issueID, _ := seedStatusQueryPR(t, "status-query-stale", 900002, "headstale", false)
		row := fetchStatusQueryPRRow(t, ctx, issueID)

		for _, kind := range []statusQueryKind{statusQueryCI, statusQueryPRHead} {
			var answer StatusAnswer
			applyStatusAnswerFromPullRequestRow(row, true, kind, &answer)
			if answer.Availability != availabilityStale {
				t.Errorf("kind %q availability = %q, want stale", kind, answer.Availability)
			}
			if answer.PullRequest == nil {
				t.Errorf("kind %q pull_request = nil, want populated (stale still carries last-known data)", kind)
			}
		}
	})
}

// fetchStatusQueryPRRow reads the issue's linked-PR row through the real
// ListPullRequestsByIssue query, exercising the same aggregation the handler
// path uses, for use with applyStatusAnswerFromPullRequestRow in tests that
// need to bypass h.PRRefresh.Enabled().
func fetchStatusQueryPRRow(t *testing.T, ctx context.Context, issueID string) db.ListPullRequestsByIssueRow {
	t.Helper()
	rows, err := testHandler.Queries.ListPullRequestsByIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("list pull requests: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("list pull requests for issue %s = %d rows, want 1", issueID, len(rows))
	}
	return rows[0]
}

// seedStatusQueryPR creates an issue with one linked PR that has a GitHub API
// snapshot and one successful check run. When fresh is true the snapshot was
// fetched "now" (current); when false it is fetched far enough in the past to
// cross prSnapshotStaleThreshold (stale). Returns the issue id and PR id.
func seedStatusQueryPR(t *testing.T, branch string, prNumber int, headSHA string, fresh bool) (issueID, prID string) {
	t.Helper()
	ctx := context.Background()

	issueID = createStatusQueryTestIssue(t, "pr-backed status query "+branch, "")

	fetchedAtExpr := "now()"
	if !fresh {
		fetchedAtExpr = "now() - interval '2 hours'"
	}

	if err := testPool.QueryRow(ctx, `
		INSERT INTO github_pull_request (
			workspace_id, installation_id, repo_owner, repo_name, pr_number, title, state, html_url,
			pr_created_at, pr_updated_at, head_sha,
			api_mergeable, api_merge_state_status, checks_rollup_state, snapshot_head_sha, snapshot_fetched_at
		)
		VALUES (
			$1, 1, 'multica-ai', 'multica', $2, 'status query PR', 'open', 'https://example.test/pr',
			now(), now(), $3,
			'MERGEABLE', 'CLEAN', 'SUCCESS', $3, `+fetchedAtExpr+`
		)
		RETURNING id
	`, testWorkspaceID, prNumber, headSHA).Scan(&prID); err != nil {
		t.Fatalf("seed PR: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id = $1`, prID)
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prID)
		testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id = $1`, prID)
	})

	if _, err := testPool.Exec(ctx, `
		INSERT INTO github_pull_request_check_run (pr_id, head_sha, ordinal, name, status, conclusion, is_status_context)
		VALUES ($1, $2, 0, 'build', 'completed', 'success', false)
	`, prID, headSHA); err != nil {
		t.Fatalf("seed check run: %v", err)
	}

	if _, err := testPool.Exec(ctx, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, issueID, prID); err != nil {
		t.Fatalf("link PR: %v", err)
	}

	return issueID, prID
}
