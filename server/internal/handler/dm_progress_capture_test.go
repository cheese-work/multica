package handler

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// In-transaction event capture (CHE-3404, plan I1.2): every write path of an
// issue's status, assignee and PR state leaves one dm_progress_pending row per
// active follow, committed with the mutation. One test per write path.

type pendingRow struct {
	Lineage, Kind string
	Immediate     bool
}

func pendingRows(t *testing.T, issue string) []pendingRow {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT lineage_id::text, event_kind, immediate FROM dm_progress_pending WHERE issue_id = $1 ORDER BY created_at, event_id`, issue)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	defer rows.Close()
	var out []pendingRow
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.Lineage, &r.Kind, &r.Immediate); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func wantPending(t *testing.T, issue, lane string, kinds ...string) []pendingRow {
	t.Helper()
	rows := pendingRows(t, issue)
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.Kind
		if r.Lineage != lane {
			t.Errorf("pending row %d is in lane %s, want %s", i, r.Lineage, lane)
		}
	}
	if !slices.Equal(got, kinds) {
		t.Fatalf("pending kinds = %v, want %v", got, kinds)
	}
	return rows
}

// capWorld is an issue followed (origin) by a fresh DM lane, with the
// baseline pending rows from creation cleared.
type capWorld struct {
	dm          dmWorld
	lane, issue string
}

func newCapWorld(t *testing.T) capWorld {
	t.Helper()
	w := newDMWorld(t)
	lane := w.dm(t, testUserID)
	issue := w.agentIssue(t, w.task(t, lane, testUserID), nil)
	dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, issue)
	return capWorld{dm: w, lane: lane, issue: issue}
}

func uuidOf(s string) pgtype.UUID { return parseUUID(s) }

func TestDMProgressCapture_StatusWritePaths(t *testing.T) {
	q := testHandler.Queries
	ctx := t.Context()
	paths := map[string]func(t *testing.T, issue string){
		"UpdateIssue": func(t *testing.T, issue string) {
			if _, err := q.UpdateIssue(ctx, db.UpdateIssueParams{ID: uuidOf(issue), Status: pgtype.Text{String: "in_progress", Valid: true}}); err != nil {
				t.Fatal(err)
			}
		},
		"UpdateIssueStatus": func(t *testing.T, issue string) {
			if _, err := q.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{ID: uuidOf(issue), Status: "in_progress", WorkspaceID: uuidOf(testWorkspaceID)}); err != nil {
				t.Fatal(err)
			}
		},
		"HTTP UpdateIssue": func(t *testing.T, issue string) {
			updateIssueField(t, issue, map[string]any{"status": "in_progress"})
		},
		"MoveIssueFromPullRequests": func(t *testing.T, issue string) {
			pr := insertGitHubPR(t, "merged")
			dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, issue, pr)
			dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, issue) // the link is its own event
			if _, err := q.MoveIssueFromPullRequests(ctx, db.MoveIssueFromPullRequestsParams{ID: uuidOf(issue), WorkspaceID: uuidOf(testWorkspaceID), TargetStatus: "in_progress", ExpectedStatus: "todo"}); err != nil {
				t.Fatal(err)
			}
		},
		"RemapProjectIssueStatus": func(t *testing.T, issue string) {
			project := dbfx.Project(t, "capture remap "+t.Name())
			dbfx.Exec(t, `UPDATE issue SET project_id = $2 WHERE id = $1`, issue, project)
			dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, issue)
			if _, err := q.RemapProjectIssueStatus(ctx, db.RemapProjectIssueStatusParams{ToStatus: "in_progress", WorkspaceID: uuidOf(testWorkspaceID), ProjectIds: []pgtype.UUID{uuidOf(project)}, FromStatus: "todo"}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, write := range paths {
		t.Run(name, func(t *testing.T) {
			w := newCapWorld(t)
			write(t, w.issue)
			wantPending(t, w.issue, w.lane, "status")
		})
	}
}

func TestDMProgressCapture_AssigneeWritePaths(t *testing.T) {
	q := testHandler.Queries
	t.Run("UpdateIssue", func(t *testing.T) {
		w := newCapWorld(t)
		agent := createHandlerTestAgent(t, "capture-assignee", []byte("[]"))
		if _, err := q.UpdateIssue(t.Context(), db.UpdateIssueParams{ID: uuidOf(w.issue), AssigneeType: pgtype.Text{String: "agent", Valid: true}, AssigneeID: uuidOf(agent)}); err != nil {
			t.Fatal(err)
		}
		wantPending(t, w.issue, w.lane, "assignee")
	})
	t.Run("TransferSquadAssignees", func(t *testing.T) {
		w := newCapWorld(t)
		leader := createHandlerTestAgent(t, "capture-leader", []byte("[]"))
		squad := dbfx.Squad(t, "capture "+t.Name(), leader)
		dbfx.Exec(t, `UPDATE issue SET assignee_type = 'squad', assignee_id = $2 WHERE id = $1`, w.issue, squad)
		dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, w.issue)
		if err := q.TransferSquadAssignees(t.Context(), db.TransferSquadAssigneesParams{AssigneeID: uuidOf(squad), AssigneeID_2: uuidOf(leader)}); err != nil {
			t.Fatal(err)
		}
		wantPending(t, w.issue, w.lane, "assignee")
	})
}

var capturePRSeq = 7_340_000

func insertGitHubPR(t *testing.T, state string) string {
	t.Helper()
	capturePRSeq++
	return dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "capture", "repo_name": "capture",
		"pr_number": capturePRSeq, "title": "capture", "state": state, "html_url": "https://example.test/pr",
		"pr_created_at": time.Now(), "pr_updated_at": time.Now(), "head_sha": "abc",
	})
}

func insertVCSPR(t *testing.T, state string) string {
	t.Helper()
	capturePRSeq++
	return dbfx.Insert(t, "vcs_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "connection_id": uuid.NewString(), "repo_owner": "capture", "repo_name": "capture",
		"pr_number": capturePRSeq, "title": "capture", "state": state, "html_url": "https://example.test/pr",
		"pr_created_at": time.Now(), "pr_updated_at": time.Now(),
	})
}

func TestDMProgressCapture_PRLinkWritePaths(t *testing.T) {
	q := testHandler.Queries
	ctx := t.Context()
	manual := pgtype.UUID{}
	paths := map[string]func(t *testing.T, issue string){
		"LinkIssueToPullRequest": func(t *testing.T, issue string) {
			if _, err := q.LinkIssueToPullRequest(ctx, db.LinkIssueToPullRequestParams{IssueID: uuidOf(issue), PullRequestID: uuidOf(insertGitHubPR(t, "open"))}); err != nil {
				t.Fatal(err)
			}
		},
		"LinkIssueToPullRequestManually": func(t *testing.T, issue string) {
			if _, err := q.LinkIssueToPullRequestManually(ctx, db.LinkIssueToPullRequestManuallyParams{IssueID: uuidOf(issue), PullRequestID: uuidOf(insertGitHubPR(t, "open")), LinkedByID: manual}); err != nil {
				t.Fatal(err)
			}
		},
		"LinkIssueToVCSPullRequest": func(t *testing.T, issue string) {
			if _, err := q.LinkIssueToVCSPullRequest(ctx, db.LinkIssueToVCSPullRequestParams{IssueID: uuidOf(issue), PullRequestID: uuidOf(insertVCSPR(t, "open"))}); err != nil {
				t.Fatal(err)
			}
		},
		"LinkIssueToVCSPullRequestManually": func(t *testing.T, issue string) {
			if _, err := q.LinkIssueToVCSPullRequestManually(ctx, db.LinkIssueToVCSPullRequestManuallyParams{IssueID: uuidOf(issue), PullRequestID: uuidOf(insertVCSPR(t, "open")), LinkedByID: manual}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, write := range paths {
		t.Run(name, func(t *testing.T) {
			w := newCapWorld(t)
			write(t, w.issue)
			wantPending(t, w.issue, w.lane, "pr")
		})
	}
}

func TestDMProgressCapture_PRStateWritePaths(t *testing.T) {
	q := testHandler.Queries
	ctx := t.Context()
	now := pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}
	t.Run("UpsertGitHubPullRequest", func(t *testing.T) {
		w := newCapWorld(t)
		pr := insertGitHubPR(t, "open")
		dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, w.issue, pr)
		dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, w.issue)
		if _, err := q.UpsertGitHubPullRequest(ctx, db.UpsertGitHubPullRequestParams{
			WorkspaceID: uuidOf(testWorkspaceID), InstallationID: 1, RepoOwner: "capture", RepoName: "capture",
			PrNumber: int32(capturePRSeq), Title: "capture", State: "merged", HtmlUrl: "https://example.test/pr",
			PrCreatedAt: now, PrUpdatedAt: now, HeadSha: "abc", MergedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		wantPending(t, w.issue, w.lane, "pr")
	})
	t.Run("UpsertVCSPullRequest", func(t *testing.T) {
		w := newCapWorld(t)
		pr := insertVCSPR(t, "open")
		var conn string
		if err := testPool.QueryRow(ctx, `SELECT connection_id::text FROM vcs_pull_request WHERE id = $1`, pr).Scan(&conn); err != nil {
			t.Fatal(err)
		}
		dbfx.Exec(t, `INSERT INTO issue_vcs_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, w.issue, pr)
		dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, w.issue)
		if _, err := q.UpsertVCSPullRequest(ctx, db.UpsertVCSPullRequestParams{
			WorkspaceID: uuidOf(testWorkspaceID), ConnectionID: uuidOf(conn), Provider: "forgejo", RepoOwner: "capture", RepoName: "capture",
			PrNumber: int32(capturePRSeq), Title: "capture", State: "merged", HtmlUrl: "https://example.test/pr",
			PrCreatedAt: now, PrUpdatedAt: now, MergedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		wantPending(t, w.issue, w.lane, "pr")
	})
	t.Run("draft to ready captures", func(t *testing.T) {
		w := newCapWorld(t)
		pr := insertGitHubPR(t, "draft")
		dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, w.issue, pr)
		dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, w.issue)
		dbfx.Exec(t, `UPDATE github_pull_request SET state = 'open' WHERE id = $1`, pr)
		wantPending(t, w.issue, w.lane, "pr")
	})
	t.Run("unchanged state captures nothing", func(t *testing.T) {
		w := newCapWorld(t)
		pr := insertGitHubPR(t, "open")
		dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, w.issue, pr)
		dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, w.issue)
		dbfx.Exec(t, `UPDATE github_pull_request SET title = 'renamed', state = 'open' WHERE id = $1`, pr)
		wantPending(t, w.issue, w.lane)
	})
}

func TestDMProgressCapture_OnlyActiveUnmutedFollowsCapture(t *testing.T) {
	cases := map[string]string{
		"unfollowed": `UPDATE dm_progress_follow SET state = 'unfollowed' WHERE issue_id = $1`,
		"muted":      `UPDATE dm_progress_follow SET muted_at = now() WHERE issue_id = $1`,
		"no follow":  `DELETE FROM dm_progress_follow WHERE issue_id = $1`,
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w := newCapWorld(t)
			dbfx.Exec(t, setup, w.issue)
			updateIssueField(t, w.issue, map[string]any{"status": "in_progress"})
			wantPending(t, w.issue, w.lane)
		})
	}
}

func TestDMProgressCapture_OneRowPerActiveFollowAndPerTransition(t *testing.T) {
	w := newCapWorld(t)
	otherLane := w.dm.dm(t, testUserID)
	dbfx.Exec(t, `INSERT INTO dm_progress_follow (workspace_id, issue_id, lineage_id, owner_user_id, reasons, last_rendered_status, reporting_root_id) VALUES ($1, $2, $3, $4, '{manual}', 'todo', $2)`, testWorkspaceID, w.issue, otherLane, testUserID)

	for _, status := range []string{"in_progress", "in_review", "in_progress"} {
		updateIssueField(t, w.issue, map[string]any{"status": status})
	}
	for lane, n := range map[string]int{w.lane: 3, otherLane: 3} {
		if got := dbfx.Count(t, `SELECT count(*) FROM dm_progress_pending WHERE issue_id = $1 AND lineage_id = $2`, w.issue, lane); got != n {
			t.Errorf("lane %s has %d pending rows, want %d (one per transition)", lane, got, n)
		}
	}
	if got := dbfx.Count(t, `SELECT count(DISTINCT event_id) FROM dm_progress_pending WHERE issue_id = $1`, w.issue); got != 3 {
		t.Errorf("distinct event ids = %d, want 3 (one per change, shared across lanes)", got)
	}
}

func TestDMProgressCapture_ImmediateOnlyForRootEnteringBlockedOrTerminal(t *testing.T) {
	set := func(t *testing.T, issue, status string) { updateIssueField(t, issue, map[string]any{"status": status}) }
	immediate := func(t *testing.T, issue string) bool {
		t.Helper()
		rows := pendingRows(t, issue)
		if len(rows) != 1 {
			t.Fatalf("pending = %+v, want exactly one row", rows)
		}
		return rows[0].Immediate
	}
	for _, c := range []struct {
		status string
		want   bool
	}{{"blocked", true}, {"done", true}, {"cancelled", true}, {"in_progress", false}, {"in_review", false}} {
		t.Run(fmt.Sprintf("root to %s", c.status), func(t *testing.T) {
			w := newCapWorld(t)
			set(t, w.issue, c.status)
			if got := immediate(t, w.issue); got != c.want {
				t.Errorf("immediate = %v, want %v", got, c.want)
			}
		})
	}
	t.Run("root already rendered blocked is not immediate again", func(t *testing.T) {
		w := newCapWorld(t)
		dbfx.Exec(t, `UPDATE dm_progress_follow SET last_rendered_status = 'blocked' WHERE issue_id = $1`, w.issue)
		set(t, w.issue, "blocked")
		if immediate(t, w.issue) {
			t.Error("blocked after a rendered blocked was marked immediate")
		}
	})
	t.Run("inherited-only follow is never immediate", func(t *testing.T) {
		w := newCapWorld(t)
		dbfx.Exec(t, `UPDATE dm_progress_follow SET reasons = '{inherited:origin}' WHERE issue_id = $1`, w.issue)
		set(t, w.issue, "done")
		if immediate(t, w.issue) {
			t.Error("child follow marked immediate")
		}
	})
	t.Run("non-status events are never immediate", func(t *testing.T) {
		w := newCapWorld(t)
		if _, err := testHandler.Queries.LinkIssueToPullRequest(t.Context(), db.LinkIssueToPullRequestParams{IssueID: uuidOf(w.issue), PullRequestID: uuidOf(insertGitHubPR(t, "merged"))}); err != nil {
			t.Fatal(err)
		}
		if immediate(t, w.issue) {
			t.Error("PR event marked immediate")
		}
	})
}

func TestDMProgressCapture_RollbackLosesBothMutationAndEvent(t *testing.T) {
	w := newCapWorld(t)
	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE issue SET status = 'in_progress' WHERE id = $1`, w.issue); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	wantPending(t, w.issue, w.lane)
	if got := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND status = 'in_progress'`, w.issue); got != 0 {
		t.Error("rolled-back status write is visible")
	}
}

func TestDMProgressCapture_IssueDeleteSweepsPending(t *testing.T) {
	w := newCapWorld(t)
	updateIssueField(t, w.issue, map[string]any{"status": "in_progress"})
	wantPending(t, w.issue, w.lane, "status")
	if err := testHandler.Queries.DeleteDMProgressForIssue(t.Context(), db.DeleteDMProgressForIssueParams{IssueID: uuidOf(w.issue), WorkspaceID: uuidOf(testWorkspaceID)}); err != nil {
		t.Fatal(err)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM dm_progress_pending WHERE issue_id = $1`, w.issue); got != 0 {
		t.Errorf("pending rows left after the issue sweep: %d", got)
	}
}
