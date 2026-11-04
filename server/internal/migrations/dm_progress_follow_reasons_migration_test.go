package migrations

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Migration 931 backfills reasons from the old source + parent_follow_id chain.
// A grandchild must inherit from the root at the top of the chain, so the root's
// unsubscribe removes it too.
func TestDMProgressFollowReasonsBackfillUsesTheChainRoot(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	schema := "dm_progress_reasons_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
	if _, err := conn.Exec(ctx, `SELECT set_config('search_path', $1, false)`, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, readMigrationFile(t, "922_dm_progress_follow.up.sql")); err != nil {
		t.Fatal(err)
	}

	lane, ws, owner := uuid.New(), uuid.New(), uuid.New()
	root, child, grandchild, orphan, manual, gone := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	followID := map[uuid.UUID]uuid.UUID{}
	insert := func(issue uuid.UUID, source, state string, parent *uuid.UUID) {
		t.Helper()
		id := uuid.New()
		followID[issue] = id
		if _, err := conn.Exec(ctx, `INSERT INTO dm_progress_follow (id, workspace_id, issue_id, lineage_id, owner_user_id, source, state, last_rendered_status, parent_follow_id) VALUES ($1,$2,$3,$4,$5,$6,$7,'todo',$8)`,
			id, ws, issue, lane, owner, source, state, parent); err != nil {
			t.Fatal(err)
		}
	}
	rootFollow := func() *uuid.UUID { id := followID[root]; return &id }
	insert(root, "origin", "active", nil)
	insert(child, "child", "active", rootFollow())
	childFollow := followID[child]
	insert(grandchild, "child", "active", &childFollow)
	insert(manual, "manual", "unfollowed", nil)
	danglingParent := uuid.New()
	insert(orphan, "child", "active", &danglingParent)
	insert(gone, "child", "unfollowed", rootFollow())

	if _, err := conn.Exec(ctx, readMigrationFile(t, "931_dm_progress_follow_reasons.up.sql")); err != nil {
		t.Fatal(err)
	}

	want := map[string][]string{
		root.String():       {"origin"},
		child.String():      {"inherited:" + root.String()},
		grandchild.String(): {"inherited:" + root.String()},
		manual.String():     {},
		orphan.String():     {"inherited:" + orphan.String()},
		gone.String():       {},
	}
	rows, err := conn.Query(ctx, `SELECT issue_id::text, reasons FROM dm_progress_follow`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var issue string
		var reasons []string
		if err := rows.Scan(&issue, &reasons); err != nil {
			t.Fatal(err)
		}
		seen++
		if !slices.Equal(reasons, want[issue]) {
			t.Errorf("issue %s reasons = %v, want %v", issue, reasons, want[issue])
		}
	}
	if seen != len(want) {
		t.Errorf("read %d follows, want %d", seen, len(want))
	}
}
