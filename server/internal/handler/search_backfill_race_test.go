package handler

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The backfill runs while the app is live. A delete that races it must not
// leave a side row behind: search reads the side tables without joining the
// source row, so an orphan would keep returning deleted text.
func TestSearchBackfill_ConcurrentDeleteLeavesNoOrphanSideRow(t *testing.T) {
	backfill, err := os.ReadFile("../../migrations/887_search_folded_text_backfill.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	type source struct {
		name, deleteSQL, sideCount string
		create                     func(t *testing.T, workspaceID string) (id string)
	}
	sources := []source{
		{
			name:      "comment",
			deleteSQL: "DELETE FROM comment WHERE id = $1",
			sideCount: "SELECT count(*) FROM comment_search_text WHERE comment_id = $1",
			create: func(t *testing.T, workspaceID string) string {
				issueID := dbfx.Issue(t, "Race", testutil.Cols{"workspace_id": workspaceID})
				return dbfx.Comment(t, issueID, "Lỗi đăng nhập", testutil.Cols{"workspace_id": workspaceID})
			},
		},
		{
			name:      "issue",
			deleteSQL: "DELETE FROM issue WHERE id = $1",
			sideCount: "SELECT count(*) FROM issue_search_text WHERE issue_id = $1",
			create: func(t *testing.T, workspaceID string) string {
				return dbfx.Issue(t, "Lỗi đăng nhập", testutil.Cols{"workspace_id": workspaceID})
			},
		},
	}

	// run executes sql on another connection; finished closes when it returns.
	type running struct {
		finished chan struct{}
		err      error
	}
	run := func(sql string, args ...any) *running {
		r := &running{finished: make(chan struct{})}
		go func() {
			defer close(r.finished)
			_, r.err = testPool.Exec(ctx, sql, args...)
		}()
		return r
	}
	// blocked reports whether r is still waiting after a short pause.
	blocked := func(r *running) bool {
		select {
		case <-r.finished:
			return false
		case <-time.After(500 * time.Millisecond):
			return true
		}
	}

	for _, src := range sources {
		// A row that predates migration 886 has no side row yet.
		prepare := func(t *testing.T) string {
			f := newDiacriticWorkspace(t)
			id := src.create(t, f.workspaceID)
			table, col := "comment_search_text", "comment_id"
			if src.name == "issue" {
				table, col = "issue_search_text", "issue_id"
			}
			if _, err := testPool.Exec(ctx, "DELETE FROM "+table+" WHERE "+col+" = $1", id); err != nil {
				t.Fatal(err)
			}
			return id
		}
		sideRows := func(t *testing.T, id string) int {
			var n int
			if err := testPool.QueryRow(ctx, src.sideCount, id).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}

		t.Run(src.name+"/delete in flight when backfill starts", func(t *testing.T) {
			id := prepare(t)
			deleteTx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer deleteTx.Rollback(ctx)
			if _, err := deleteTx.Exec(ctx, src.deleteSQL, id); err != nil {
				t.Fatal(err)
			}
			backfilling := run(string(backfill))
			if !blocked(backfilling) {
				t.Log("backfill did not wait for the in-flight delete")
			}
			if err := deleteTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			<-backfilling.finished
			if backfilling.err != nil {
				t.Fatal(backfilling.err)
			}
			if n := sideRows(t, id); n != 0 {
				t.Errorf("%d side rows outlive the deleted %s", n, src.name)
			}
		})

		t.Run(src.name+"/delete arrives while backfill is open", func(t *testing.T) {
			id := prepare(t)
			backfillTx, err := testPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer backfillTx.Rollback(ctx)
			if _, err := backfillTx.Exec(ctx, string(backfill)); err != nil {
				t.Fatal(err)
			}
			deleting := run(src.deleteSQL, id)
			if !blocked(deleting) {
				t.Log("delete did not wait for the open backfill")
			}
			if err := backfillTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			<-deleting.finished
			if deleting.err != nil {
				t.Fatal(deleting.err)
			}
			if n := sideRows(t, id); n != 0 {
				t.Errorf("%d side rows outlive the deleted %s", n, src.name)
			}
		})
	}
}
