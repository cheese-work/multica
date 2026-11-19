package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/ciwatch"
	"github.com/multica-ai/multica/server/internal/issuecreatedvia/backfill"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-6391 parity: the same cases through the create path and through the
// backfill give equal created_via values (plan risk 1, create/backfill drift).
func TestCreatedViaBackfillParityWithCreatePath(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx,
		`SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND name = $2`,
		testWorkspaceID, "Handler Test Agent",
	).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("find test agent: %v", err)
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	scanID := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("setup: %v", err)
		}
		return id
	}
	seedIssue := func(title string) string {
		// Take the next number from the workspace counter, as the create path does.
		exec(`UPDATE workspace SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1 WHERE id = $1`, testWorkspaceID)
		id := scanID(`INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, number)
			 VALUES ($1, $2, 'todo', 'none', 'member', $3,
			   (SELECT issue_counter FROM workspace WHERE id = $1)) RETURNING id`,
			testWorkspaceID, title, testUserID)
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id) })
		return id
	}
	seedComment := func(issueID, authorType, authorID string) string {
		return scanID(`INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
			 VALUES ($1, $2, $3, $4, 'do it') RETURNING id`, testWorkspaceID, issueID, authorType, authorID)
	}
	seedSession := func() string {
		id := scanID(`INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, runtime_id)
			 VALUES ($1, $2, $3, 'parity dm', $4) RETURNING id`, testWorkspaceID, agentID, testUserID, runtimeID)
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, id) })
		return id
	}
	seedTask := func(chatSessionID, issueID, commentID, evidenceKind any) string {
		id := scanID(`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, chat_session_id,
			    trigger_comment_id, trigger_evidence_kind, originator_user_id, accountable_user_id)
			 VALUES ($1, $2, $3, 'running', 0, $4, $5, $6, $7, $7) RETURNING id`,
			agentID, runtimeID, issueID, chatSessionID, commentID, evidenceKind, testUserID)
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, id) })
		return id
	}

	type made struct {
		id         string
		agentMade  bool
		createdVia string
	}
	cases := map[string]made{}
	record := func(name, id string, agentMade bool) {
		var via *string
		if err := testPool.QueryRow(ctx, `SELECT created_via FROM issue WHERE id = $1`, id).Scan(&via); err != nil {
			t.Fatal(err)
		}
		if via == nil {
			t.Fatalf("%s: the create path stored NULL", name)
		}
		cases[name] = made{id: id, agentMade: agentMade, createdVia: *via}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM github_release_deploy WHERE issue_id = $1`, id)
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id)
		})
	}
	createAs := func(name, taskID string, body map[string]any) string {
		body["title"] = "parity " + name
		req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)
		if taskID != "" {
			req.Header.Set("X-Agent-ID", agentID)
			req.Header.Set("X-Task-ID", taskID)
		}
		w := httptest.NewRecorder()
		testHandler.CreateIssue(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateIssue %s: got %d: %s", name, w.Code, w.Body.String())
		}
		var created IssueResponse
		if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		record(name, created.ID, taskID != "")
		return created.ID
	}

	host := seedIssue("parity host")
	memberComment := seedComment(host, "member", testUserID)
	agentComment := seedComment(host, "agent", agentID)
	older := seedIssue("parity older parent")

	createAs("dm", seedTask(seedSession(), nil, nil, nil), map[string]any{})
	createAs("comment", seedTask(nil, host, memberComment, "comment"), map[string]any{})
	createAs("wakeup", seedTask(nil, host, memberComment, "issue_wakeup"), map[string]any{})
	createAs("comment by agent", seedTask(nil, host, agentComment, "comment"), map[string]any{})
	createAs("dm under older parent", seedTask(seedSession(), nil, nil, nil), map[string]any{"parent_issue_id": older})
	sameRun := seedTask(seedSession(), nil, nil, nil)
	parent := createAs("dm same-run parent", sameRun, map[string]any{})
	createAs("dm same-run child", sameRun, map[string]any{"parent_issue_id": parent})
	createAs("member form", "", map[string]any{})

	// CI-watch issue: platform create path, member creator, counter in metadata.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ciID, err := testHandler.CIWatchDestinations().Issue.CreateIssue(ctx, tx, ciwatch.IssueSpec{
		WorkspaceID: testWorkspaceID, Title: "parity ci-watch", Body: "body", Priority: "urgent", CreatorID: testUserID,
		Metadata: map[string]any{"ci_watch_count": 2},
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	record("ci-watch", ciID, false)

	// Release issue: platform create path, member creator, linked by github_release_deploy.
	rel, _, err := testHandler.IssueService.CreateRelease(ctx, service.ReleaseDeploy{
		WorkspaceID: util.MustParseUUID(testWorkspaceID), RepoOwner: "cheese-work", RepoName: "parity",
		RunID: 1, OldSHA: "1111111", NewSHA: "2222222", Digest: "sha256:parity",
	})
	if err != nil {
		t.Fatalf("CreateRelease: %v", err)
	}
	record("release", util.UUIDToString(rel.ID), false)

	// Make the rows look as they did before the server classified them: agent
	// creators stored `agent`, everyone else NULL.
	var ids []pgtype.UUID
	for _, c := range cases {
		old := any(nil)
		if c.agentMade {
			old = "agent"
		}
		exec(`UPDATE issue SET created_via = $2 WHERE id = $1`, c.id, old)
		ids = append(ids, util.MustParseUUID(c.id))
	}

	receipt := filepath.Join(t.TempDir(), "receipt.jsonl")
	t.Cleanup(func() {
		backfill.Revert(context.Background(), testPool, backfill.Options{IssueIDs: ids}, receipt)
	})
	if _, err := backfill.Apply(ctx, testPool, backfill.Options{IssueIDs: ids}, receipt); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// Pin the values too, so equal-but-wrong cannot pass.
	want := map[string]string{
		"dm": "dm", "comment": "comment", "wakeup": "agent", "comment by agent": "agent",
		"dm under older parent": "dm", "dm same-run parent": "dm", "dm same-run child": "agent",
		"member form": "form", "ci-watch": "autopilot", "release": "autopilot",
	}
	for name, c := range cases {
		if w, ok := want[name]; ok && c.createdVia != w {
			t.Errorf("%s: create path = %q, want %q", name, c.createdVia, w)
		}
		var got *string
		if err := testPool.QueryRow(ctx, `SELECT created_via FROM issue WHERE id = $1`, c.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == nil || *got != c.createdVia {
			t.Errorf("%s: backfill = %v, create path = %q", name, got, c.createdVia)
		}
	}
}
