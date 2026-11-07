package handler

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

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

// planWork sums actual rows x loops over every node of an EXPLAIN ANALYZE plan:
// rows the executor touched, which does not depend on how busy the machine is.
func planWork(node map[string]any) float64 {
	rows, _ := node["Actual Rows"].(float64)
	loops, _ := node["Actual Loops"].(float64)
	work := rows * loops
	subplans, _ := node["Plans"].([]any)
	for _, sub := range subplans {
		work += planWork(sub.(map[string]any))
	}
	return work
}

// sqlSpy records the statement a Queries sends, so the test explains the
// production query rather than a copy of it.
type sqlSpy struct {
	db.DBTX
	sql string
}

func (s *sqlSpy) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	s.sql = sql
	return s.DBTX.QueryRow(ctx, sql, args...)
}

// SEC-1: the walk is linear in chain depth. The check counts rows the executor
// touches (EXPLAIN ANALYZE), not seconds: a wall-clock deadline failed on a
// loaded CI database (CHE-4266). A deep acyclic chain stays within a small
// multiple of its depth (a quadratic walk would touch depth^2/2 rows); a deep
// chain closed into a loop still reports the loop. A walk that never ends is
// caught by the go test timeout.
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

	ctx := context.Background()
	spy := &sqlSpy{DBTX: testPool}
	loops := func(id string) bool {
		t.Helper()
		got, err := db.New(spy).IssueParentChainLoops(ctx, parseUUID(id))
		if err != nil {
			t.Fatalf("walk from depth %d: %v", depth, err)
		}
		return got.Bool
	}
	// work is what the walk from id touched, as the planner executes it.
	work := func(id string) float64 {
		t.Helper()
		var plan []byte
		if err := testPool.QueryRow(ctx, "EXPLAIN (ANALYZE, TIMING OFF, FORMAT JSON) "+spy.sql, parseUUID(id)).Scan(&plan); err != nil {
			t.Fatalf("explain walk from depth %d: %v", depth, err)
		}
		var out []struct{ Plan map[string]any }
		if err := json.Unmarshal(plan, &out); err != nil || len(out) != 1 {
			t.Fatalf("parse plan: %v", err)
		}
		return planWork(out[0].Plan)
	}

	if loops(leaf) {
		t.Fatal("acyclic deep chain reported as a loop")
	}
	w, limit := work(leaf), float64(10*depth)
	t.Logf("acyclic walk touched %.0f rows for depth %d (limit %.0f)", w, depth, limit)
	if w > limit {
		t.Fatalf("walk touched %.0f rows for depth %d, want <= %.0f (linear)", w, depth, limit)
	}
	dbfx.Exec(t, `UPDATE issue SET parent_issue_id = $2 WHERE id = $1`, top, leaf)
	if !loops(leaf) {
		t.Fatal("deep chain closed into a loop reported as acyclic")
	}
	if w := work(leaf); w > limit {
		t.Fatalf("looping walk touched %.0f rows for depth %d, want <= %.0f (linear)", w, depth, limit)
	}
}
