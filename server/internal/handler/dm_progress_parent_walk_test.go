package handler

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// walkCounter counts the ancestor-walk statements a Queries sends.
type walkCounter struct {
	db.DBTX
	walks atomic.Int32
}

func (c *walkCounter) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	if strings.Contains(sql, "WITH RECURSIVE walk") {
		c.walks.Add(1)
	}
	return c.DBTX.QueryRow(ctx, sql, args...)
}

func (c *walkCounter) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	if strings.Contains(sql, "WITH RECURSIVE walk") {
		c.walks.Add(1)
	}
	return c.DBTX.Query(ctx, sql, args...)
}

func (c *walkCounter) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	return c.DBTX.Exec(ctx, sql, args...)
}

// SEC-1: a child of an unfollowed parent has nothing to inherit, so creating it
// must not walk the ancestry (the walk ran under the workspace counter lock).
func TestDMProgress_ChildOfUnfollowedParentRunsNoAncestorWalk(t *testing.T) {
	ctx := context.Background()
	parentID := dbfx.Issue(t, "unfollowed parent")
	childID := dbfx.Issue(t, "child", map[string]any{"parent_issue_id": parentID})
	counter := &walkCounter{DBTX: testPool}
	q := db.New(counter)
	parent, err := q.GetIssue(ctx, parseUUID(parentID))
	if err != nil {
		t.Fatal(err)
	}
	child, err := q.GetIssue(ctx, parseUUID(childID))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.InheritProgressFollows(ctx, q, parent, child); err != nil {
		t.Fatal(err)
	}
	if n := counter.walks.Load(); n != 0 {
		t.Fatalf("%d ancestor walks for a parent with no follows, want 0", n)
	}
}

// SEC-1: the walk is linear in chain depth. A deep acyclic chain finishes well
// inside the deadline; a deep chain closed into a loop still reports the loop.
func TestDMProgress_AncestorWalkIsLinearInDepth(t *testing.T) {
	const depth = 12000
	var leaf, top string
	dbfx.QueryRow(t, `
		WITH ids AS (SELECT g, gen_random_uuid() AS id FROM generate_series(1, $3::int) g),
		base AS (SELECT COALESCE(MAX(number), 0) AS n FROM issue WHERE workspace_id = $1),
		ins AS (
			INSERT INTO issue (id, workspace_id, title, status, priority, creator_type, creator_id, number, parent_issue_id)
			SELECT ids.id, $1, 'deep chain', 'todo', 'medium', 'member', $2, base.n + ids.g, p.id
			FROM ids CROSS JOIN base LEFT JOIN ids p ON p.g = ids.g - 1
		)
		SELECT (SELECT id::text FROM ids WHERE g = $3::int), (SELECT id::text FROM ids WHERE g = 1)`,
		testWorkspaceID, testUserID, depth).Scan(&leaf, &top)
	dbfx.Cleanup(t, `DELETE FROM issue WHERE workspace_id = $1 AND title = 'deep chain'`, testWorkspaceID)

	loops := func(id string) bool {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		got, err := db.New(testPool).IssueParentChainLoops(ctx, parseUUID(id))
		if err != nil {
			t.Fatalf("walk from depth %d: %v", depth, err)
		}
		return got.Bool
	}
	if loops(leaf) {
		t.Fatal("acyclic deep chain reported as a loop")
	}
	dbfx.Exec(t, `UPDATE issue SET parent_issue_id = $2 WHERE id = $1`, top, leaf)
	if !loops(leaf) {
		t.Fatal("deep chain closed into a loop reported as acyclic")
	}
}
