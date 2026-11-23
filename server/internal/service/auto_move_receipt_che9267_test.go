package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-9267 (plan CHE-9122, step 2): receipts for server status moves. The
// handler package drives the handler-side writers; this file owns the cause
// registry, the service-side writers (task_reset, qoder) and the readers that
// select system comments.

// (c) Registry: every cause kind a status_changed row can carry, with whether a
// move of that kind owes a receipt. A kind that is not in this table fails the
// test, so a new writer must decide its answer here.
func TestStatusCauseKindsReceiptEligibility(t *testing.T) {
	want := map[string]bool{
		StatusCauseUIOrAPI:       false,
		StatusCauseWorkflowRemap: false,
		StatusCauseTaskReset:     true,
		StatusCauseRunBudget:     true,
		StatusCausePRMerged:      true,
		StatusCauseQoder:         true,
		StatusCauseDecision:      true,
		"workflow_edge":          true,
		"ci_advance":             true,
		"watchdog":               true,
		"step_watchdog_budget":   true,
		"system":                 true,
	}
	kinds := StatusCauseKinds()
	sort.Strings(kinds)
	seen := map[string]bool{}
	for _, kind := range kinds {
		seen[kind] = true
		expected, ok := want[kind]
		if !ok {
			t.Errorf("cause kind %q is not in the eligibility table: decide whether its move owes a receipt", kind)
			continue
		}
		if got := StatusCauseOwesReceipt(kind); got != expected {
			t.Errorf("StatusCauseOwesReceipt(%q) = %v, want %v", kind, got, expected)
		}
	}
	for kind := range want {
		if !seen[kind] {
			t.Errorf("cause kind %q is in the table but not in StatusCauseKinds()", kind)
		}
	}
	if StatusCauseOwesReceipt("a_kind_nobody_registered") {
		t.Error("an unregistered cause kind owes a receipt; unknown kinds must fail closed")
	}
}

func setReceiptModeSvc(t *testing.T, f *delegatedFailureFixture, mode string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE workspace SET settings = jsonb_set(COALESCE(settings,'{}'), '{auto_move_receipt}', jsonb_build_object('mode', $2::text)) WHERE id = $1`, f.workspaceID, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.pool.Exec(context.Background(), `UPDATE workspace SET settings = settings - 'auto_move_receipt' WHERE id = $1`, f.workspaceID)
	})
}

// receiptState reads the status_changed ids and the would-post and posted
// move ids of an issue.
func receiptState(t *testing.T, f *delegatedFailureFixture, issueID string) (moveIDs, wouldPost, posted []string) {
	t.Helper()
	collect := func(query string, args ...any) []string {
		rows, err := f.pool.Query(context.Background(), query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	moveIDs = collect(`SELECT id::text FROM activity_log WHERE issue_id = $1 AND action = 'status_changed' ORDER BY created_at, id`, issueID)
	wouldPost = collect(`SELECT details->>'move_id' FROM activity_log WHERE issue_id = $1 AND action = 'auto_move_receipt_would_post' ORDER BY created_at, id`, issueID)
	posted = collect(`SELECT details->>'move_id' FROM activity_log WHERE issue_id = $1 AND action = 'auto_move_receipt_posted' ORDER BY created_at, id`, issueID)
	return
}

type serviceWriter struct {
	name  string
	drive func(t *testing.T, f *delegatedFailureFixture, svc *TaskService)
	cause string
	from  string
	to    string
}

func serviceWriters() []serviceWriter {
	return []serviceWriter{
		{name: "task_reset", cause: StatusCauseTaskReset, from: "in_progress", to: "todo",
			drive: func(t *testing.T, f *delegatedFailureFixture, svc *TaskService) {
				ctx := context.Background()
				if _, err := f.pool.Exec(ctx, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, f.issueID); err != nil {
					t.Fatal(err)
				}
				var taskID string
				if err := f.pool.QueryRow(ctx, `
					INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, error, failure_reason, completed_at)
					VALUES ($1, $2, $3, 'failed', 0, 'boom', 'agent_error.process_failure', now()) RETURNING id::text`,
					f.coordinator, f.runtimeID, f.issueID).Scan(&taskID); err != nil {
					t.Fatal(err)
				}
				failed, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
				if err != nil {
					t.Fatal(err)
				}
				svc.HandleFailedTasks(ctx, []db.AgentTaskQueue{failed})
			}},
		{name: "qoder", cause: StatusCauseQoder, from: "todo", to: "in_progress",
			drive: func(t *testing.T, f *delegatedFailureFixture, svc *TaskService) {
				ctx := context.Background()
				for _, stmt := range []string{
					`UPDATE agent_runtime SET provider = 'qoder_cloud' WHERE id = '` + f.runtimeID + `'`,
					`UPDATE issue SET status = 'todo' WHERE id = '` + f.issueID + `'`,
				} {
					if _, err := f.pool.Exec(ctx, stmt); err != nil {
						t.Fatal(err)
					}
				}
				var taskID string
				if err := f.pool.QueryRow(ctx, `
					INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, context)
					VALUES ($1, $2, $3, 'running', 0, '{}'::jsonb) RETURNING id::text`,
					f.coordinator, f.runtimeID, f.issueID).Scan(&taskID); err != nil {
					t.Fatal(err)
				}
				task, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
				if err != nil {
					t.Fatal(err)
				}
				if change, err := syncQoderIssue(ctx, svc.Queries, task, false); err != nil || change == nil {
					t.Fatalf("syncQoderIssue = %v, %v", change, err)
				}
			}},
	}
}

// (a) for the service writers: warn writes one would-post row naming the move,
// block writes one receipt comment and one posted row, off writes nothing.
func TestAutoMoveReceiptServiceWriters(t *testing.T) {
	for _, w := range serviceWriters() {
		for _, mode := range []string{"warn", "block", "off"} {
			t.Run(w.name+"/"+mode, func(t *testing.T) {
				f, svc := seedDelegatedFailureFixture(t)
				setReceiptModeSvc(t, f, mode)
				w.drive(t, f, svc)

				moves, wouldPost, posted := receiptState(t, f, f.issueID)
				if len(moves) != 1 {
					t.Fatalf("status_changed rows = %d, want 1", len(moves))
				}
				var commentIDs, contents []string
				rows, err := f.pool.Query(context.Background(), `SELECT id::text, content FROM comment WHERE issue_id = $1 AND author_type = 'system' AND type = 'system' AND position($2 IN content) > 0`, f.issueID, moves[0])
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					var id, content string
					if err := rows.Scan(&id, &content); err != nil {
						t.Fatal(err)
					}
					commentIDs, contents = append(commentIDs, id), append(contents, content)
				}
				rows.Close()

				switch mode {
				case "warn":
					if len(wouldPost) != 1 || wouldPost[0] != moves[0] || len(posted) != 0 || len(commentIDs) != 0 {
						t.Fatalf("warn: would-post %v, posted %v, comments %v; want one would-post naming %s only", wouldPost, posted, commentIDs, moves[0])
					}
				case "block":
					if len(wouldPost) != 0 || len(posted) != 1 || posted[0] != moves[0] || len(commentIDs) != 1 {
						t.Fatalf("block: would-post %v, posted %v, comments %v; want one receipt and one posted row naming %s", wouldPost, posted, commentIDs, moves[0])
					}
					for _, want := range []string{w.cause, w.from, w.to, moves[0]} {
						if !strings.Contains(contents[0], want) {
							t.Fatalf("receipt lacks %q:\n%s", want, contents[0])
						}
					}
					var commentID string
					if err := f.pool.QueryRow(context.Background(), `SELECT details->>'comment_id' FROM activity_log WHERE issue_id = $1 AND action = 'auto_move_receipt_posted'`, f.issueID).Scan(&commentID); err != nil || commentID != commentIDs[0] {
						t.Fatalf("posted row names comment %q (%v), want %s", commentID, err, commentIDs[0])
					}
				case "off":
					if len(wouldPost)+len(posted)+len(commentIDs) != 0 {
						t.Fatalf("off wrote would-post %v, posted %v, comments %v; want none", wouldPost, posted, commentIDs)
					}
				}
			})
		}
	}
}

// (e) A receipt is a system comment of type system with no source_task_id and
// no github_notify_only mark. The readers that select system comments return
// the same rows with receipts present as without.
func TestAutoMoveReceiptDoesNotChangeSystemCommentReaders(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	setReceiptModeSvc(t, f, "block")
	ctx := context.Background()
	q := svc.Queries
	issueID, workspaceID := util.MustParseUUID(f.issueID), util.MustParseUUID(f.workspaceID)
	agentID := util.MustParseUUID(f.coordinator)

	readers := func() (planned, recovery, exhaustion int, notifyOnly int) {
		rows, err := q.ListReconcilableCommentsForIssueSince(ctx, db.ListReconcilableCommentsForIssueSinceParams{
			IssueID: issueID, AgentID: agentID,
			Since: pgtype.Timestamptz{Time: time.Unix(0, 0), Valid: true},
			PlannedCommentIds: func() []pgtype.UUID {
				var ids []pgtype.UUID
				r, err := f.pool.Query(ctx, `SELECT id FROM comment WHERE issue_id = $1 AND author_type = 'system'`, f.issueID)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				for r.Next() {
					var id pgtype.UUID
					if err := r.Scan(&id); err != nil {
						t.Fatal(err)
					}
					ids = append(ids, id)
				}
				return ids
			}(),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range rows {
			if c.AuthorType == "system" {
				planned++
			}
		}
		taskID := util.MustParseUUID(f.sourceTask)
		if _, err := q.GetDelegatedFailureRecoveryComment(ctx, db.GetDelegatedFailureRecoveryCommentParams{IssueID: issueID, WorkspaceID: workspaceID, SourceTaskID: taskID}); err == nil {
			recovery++
		} else if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if _, err := q.GetDelegatedFailureRecoveryExhaustionComment(ctx, db.GetDelegatedFailureRecoveryExhaustionCommentParams{IssueID: issueID, WorkspaceID: workspaceID, SourceTaskID: taskID}); err == nil {
			exhaustion++
		} else if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND github_notify_only`, f.issueID).Scan(&notifyOnly); err != nil {
			t.Fatal(err)
		}
		return
	}
	p0, r0, e0, n0 := readers()

	serviceWriters()[0].drive(t, f, svc)
	var receipts int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND type = 'system' AND source_task_id IS NULL AND NOT github_notify_only`, f.issueID).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("receipt comments = %d, %v; want 1 system comment with no source_task_id and no github_notify_only", receipts, err)
	}

	p1, r1, e1, n1 := readers()
	if p0 != p1 || r0 != r1 || e0 != e1 || n0 != n1 {
		t.Fatalf("readers changed with a receipt present: reconcilable system comments %d->%d, recovery %d->%d, exhaustion %d->%d, notify-only %d->%d", p0, p1, r0, r1, e0, e1, n0, n1)
	}
}
