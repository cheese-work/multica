package queuerank

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sandbox gives a test its own schema holding only the columns queue_rank
// reads, then applies the real migration file on top.
func sandbox(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL required; run through make env-exec")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "queuerank_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",pg_catalog"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	exec(t, pool, `
		CREATE TABLE workspace (id uuid PRIMARY KEY, settings jsonb NOT NULL DEFAULT '{}');
		CREATE TABLE project (id uuid PRIMARY KEY, priority text NOT NULL DEFAULT 'none');
		CREATE TABLE issue (
			id uuid PRIMARY KEY,
			workspace_id uuid NOT NULL REFERENCES workspace(id),
			project_id uuid REFERENCES project(id),
			parent_issue_id uuid REFERENCES issue(id),
			priority text NOT NULL DEFAULT 'none'
		)`)
	return pool
}

func migration(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

var priorities = []string{"none", "low", "medium", "high", "urgent"}

type issueFix struct {
	ws          string // workspace label
	own         string
	project     *string
	parent      *string
	projectName string
}

// TestSQLMatchesGo: queue_rank_at equals Go Score on every fixture row.
func TestSQLMatchesGo(t *testing.T) {
	pool := sandbox(t)
	ctx := context.Background()
	exec(t, pool, migration(t, "99d_queue_rank.up.sql"))

	// One workspace per setting shape.
	type wsFix struct {
		settings string
		mode     string
		aging    string
	}
	wsFixes := map[string]wsFix{
		"rank":      {`{"queue_order":"project_rank"}`, "project_rank", ""},
		"rank5":     {`{"queue_order":"project_rank","queue_aging_minutes":5}`, "project_rank", "5"},
		"rank720":   {`{"queue_order":"project_rank","queue_aging_minutes":720}`, "project_rank", "720"},
		"rank1":     {`{"queue_order":"project_rank","queue_aging_minutes":1}`, "project_rank", "1"},
		"rank9999":  {`{"queue_order":"project_rank","queue_aging_minutes":9999}`, "project_rank", "9999"},
		"rankText":  {`{"queue_order":"project_rank","queue_aging_minutes":"45"}`, "project_rank", "45"},
		"rankBad":   {`{"queue_order":"project_rank","queue_aging_minutes":"abc"}`, "project_rank", "abc"},
		"rankFloat": {`{"queue_order":"project_rank","queue_aging_minutes":45.5}`, "project_rank", "45.5"},
		"priority":  {`{"queue_order":"priority","queue_aging_minutes":5}`, "priority", "5"},
		"unset":     {`{}`, "", ""},
		"bogus":     {`{"queue_order":"PROJECT_RANK"}`, "PROJECT_RANK", ""},
	}
	wsID := map[string]string{}
	for label, f := range wsFixes {
		id := uuid.NewString()
		wsID[label] = id
		exec(t, pool, `INSERT INTO workspace (id, settings) VALUES ($1, $2::jsonb)`, id, f.settings)
	}
	projectID := map[string]string{}
	for _, p := range priorities {
		id := uuid.NewString()
		projectID[p] = id
		exec(t, pool, `INSERT INTO project (id, priority) VALUES ($1, $2)`, id, p)
	}
	// Issues: every project option (none, or a project of each priority) x every
	// parent option (no parent, or a parent of each priority) x every own band.
	type issue struct {
		id          string
		ws          string
		own         string
		project     *string
		parent      *string
		hasProject  bool
		hasParent   bool
		projectPrio string
		parentPrio  string
	}
	var issues []issue
	projectOptions := append([]string{""}, priorities...)
	parentOptions := append([]string{""}, priorities...)
	for label := range wsFixes {
		// Full grid in the rank workspace, a sample grid elsewhere.
		for pi, pp := range projectOptions {
			for ai, ap := range parentOptions {
				for _, own := range priorities {
					if label != "rank" && (pi+ai)%3 != 0 {
						continue
					}
					is := issue{id: uuid.NewString(), ws: label, own: own}
					var projArg, parArg any
					if pp != "" {
						is.hasProject, is.projectPrio = true, pp
						is.project = &pp
						projArg = projectID[pp]
					}
					if ap != "" {
						is.hasParent, is.parentPrio = true, ap
						is.parent = &ap
						parentID := uuid.NewString()
						exec(t, pool, `INSERT INTO issue (id, workspace_id, priority) VALUES ($1, $2, $3)`, parentID, wsID[label], ap)
						parArg = parentID
					}
					exec(t, pool, `INSERT INTO issue (id, workspace_id, project_id, parent_issue_id, priority) VALUES ($1, $2, $3, $4, $5)`,
						is.id, wsID[label], projArg, parArg, own)
					issues = append(issues, is)
				}
			}
		}
	}

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	waits := []time.Duration{
		-time.Hour, 0, time.Microsecond, 5*time.Minute - time.Microsecond, 5 * time.Minute, 30*time.Minute - time.Microsecond, 30 * time.Minute,
		45 * time.Minute, 90 * time.Minute, 150 * time.Minute, 12 * time.Hour, 720 * time.Minute, 5 * 720 * time.Minute,
	}

	checked := 0
	check := func(label string, got int, mode, aging string, k Keys, wait time.Duration) {
		t.Helper()
		want := Score(ModeFromSetting(mode), k, wait, AgingFromSetting(aging))
		checked++
		if got != want {
			t.Errorf("ws %s keys %+v wait %v: SQL %d, Go %d", label, k, wait, got, want)
		}
	}

	for _, is := range issues {
		f := wsFixes[is.ws]
		keys := IssueKeys(is.own, is.project, is.parent)
		for _, w := range waits {
			var got int
			if err := pool.QueryRow(ctx, `SELECT queue_rank_at($1, $2, 4, $3, $4)`, wsID[is.ws], is.id, now.Add(-w), now).Scan(&got); err != nil {
				t.Fatal(err)
			}
			check(is.ws, got, f.mode, f.aging, keys, w)
		}
	}

	// No issue: fallback priority gives all three keys. Also an unknown issue,
	// an issue of another workspace, and a NULL fallback.
	other := uuid.NewString()
	var otherWS string
	for _, is := range issues {
		if is.ws == "priority" {
			otherWS = is.id
			break
		}
	}
	_ = other
	for _, label := range []string{"rank", "rank5", "priority", "unset"} {
		f := wsFixes[label]
		for fb := 0; fb <= 4; fb++ {
			for _, w := range waits {
				for name, issueArg := range map[string]any{"nil": nil, "unknown": uuid.NewString(), "foreign": otherWS} {
					if name == "foreign" && label == "priority" {
						continue // the foreign issue belongs to this workspace
					}
					var got int
					if err := pool.QueryRow(ctx, `SELECT queue_rank_at($1, $2, $3, $4, $5)`, wsID[label], issueArg, fb, now.Add(-w), now).Scan(&got); err != nil {
						t.Fatal(err)
					}
					check(label+"/"+name, got, f.mode, f.aging, FallbackKeys(fb), w)
				}
			}
		}
	}
	// NULL since means no wait; NULL fallback means band 0.
	var got int
	if err := pool.QueryRow(ctx, `SELECT queue_rank_at($1, NULL, NULL, NULL, $2)`, wsID["rank"], now).Scan(&got); err != nil || got != 0 {
		t.Errorf("NULL fallback and since: got %d err %v, want 0", got, err)
	}
	// Unknown workspace ranks 0.
	if err := pool.QueryRow(ctx, `SELECT queue_rank_at($1, NULL, 4, $2, $2)`, uuid.NewString(), now).Scan(&got); err != nil || got != 0 {
		t.Errorf("unknown workspace: got %d err %v, want 0", got, err)
	}
	// The public signature reads the clock: a 95 minute wait is 3 steps of 30.
	if err := pool.QueryRow(ctx, `SELECT queue_rank($1, NULL, 2, now() - interval '95 minutes')`, wsID["rank"]).Scan(&got); err != nil || got != 62+75 {
		t.Errorf("queue_rank with clock: got %d err %v, want %d", got, err, 62+75)
	}
	// Priority mode returns 0 for every row, however old or urgent.
	if err := pool.QueryRow(ctx, `SELECT queue_rank($1, NULL, 4, now() - interval '30 days')`, wsID["priority"]).Scan(&got); err != nil || got != 0 {
		t.Errorf("priority mode: got %d err %v, want 0", got, err)
	}
	t.Logf("compared %d fixture rows", checked)
	if checked < 2000 {
		t.Fatalf("only %d rows compared; fixture grid shrank", checked)
	}
}

// TestSlotPreservingRerank (plan r4 Approach 8): workspace P (priority) keeps its
// rows and positions; workspace R (project_rank) keeps its position count.
func TestSlotPreservingRerank(t *testing.T) {
	pool := sandbox(t)
	ctx := context.Background()
	exec(t, pool, migration(t, "99d_queue_rank.up.sql"))
	exec(t, pool, `CREATE TABLE cand (id uuid PRIMARY KEY, workspace_id uuid, issue_id uuid, class int, prio int, created_at timestamptz)`)

	p, r := uuid.NewString(), uuid.NewString()
	exec(t, pool, `INSERT INTO workspace (id, settings) VALUES ($1, '{}'), ($2, '{"queue_order":"project_rank"}')`, p, r)
	high, none := uuid.NewString(), uuid.NewString()
	exec(t, pool, `INSERT INTO project (id, priority) VALUES ($1, 'high'), ($2, 'none')`, high, none)

	base := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	type row struct {
		ws, project string
		prio        string
		class       int
		age         int // minutes after base
	}
	// Interleaved in today's order (priority DESC, created_at): P and R rows alternate.
	rows := []row{
		{p, none, "urgent", 0, 0}, {r, none, "urgent", 0, 1}, {p, high, "high", 0, 2}, {r, none, "high", 0, 3},
		{p, none, "high", 0, 4}, {r, high, "medium", 0, 5}, {r, high, "low", 0, 6}, {p, high, "low", 0, 7},
		{p, none, "none", 1, 8}, {r, high, "none", 1, 9}, {r, none, "medium", 1, 10}, {p, high, "medium", 1, 11},
	}
	for i, rw := range rows {
		issueID, id := uuid.NewString(), uuid.NewString()
		exec(t, pool, `INSERT INTO issue (id, workspace_id, project_id, priority) VALUES ($1, $2, $3, $4)`, issueID, rw.ws, rw.project, rw.prio)
		exec(t, pool, `INSERT INTO cand VALUES ($1, $2, $3, $4, $5, $6)`, id, rw.ws, issueID, rw.class, Band(rw.prio), base.Add(time.Duration(rw.age)*time.Minute))
		_ = i
	}

	// Today's order: class first (one-shot lane), then priority DESC, created_at, id.
	todayOrder := func() []string {
		return queryIDs(t, pool, `SELECT id::text FROM cand ORDER BY class, prio DESC, created_at, id`)
	}
	reranked := queryIDs(t, pool, `
		WITH c AS (
			SELECT id, workspace_id, class,
			       row_number() OVER (ORDER BY class, prio DESC, created_at, id) AS today_pos,
			       queue_rank_at(workspace_id, issue_id, prio, created_at, $1) AS rank
			FROM cand)
		SELECT cand_id::text FROM queue_rerank_slots(ARRAY(SELECT ROW(id, workspace_id, class, today_pos, rank)::queue_rank_cand FROM c)) ORDER BY pos`,
		base.Add(3*time.Hour))
	today := todayOrder()
	if len(reranked) != len(today) {
		t.Fatalf("rerank returned %d rows, want %d", len(reranked), len(today))
	}

	wsOf := map[string]string{}
	classOf := map[string]int{}
	rankOf := map[string]int{}
	rs, err := pool.Query(ctx, `SELECT id::text, workspace_id::text, class, queue_rank_at(workspace_id, issue_id, prio, created_at, $1) FROM cand`, base.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var id, ws string
		var class, rank int
		if err := rs.Scan(&id, &ws, &class, &rank); err != nil {
			t.Fatal(err)
		}
		wsOf[id], classOf[id], rankOf[id] = ws, class, rank
	}
	rs.Close()

	for pos := range today {
		// Same workspace and class at every position: the slot is kept.
		if wsOf[today[pos]] != wsOf[reranked[pos]] || classOf[today[pos]] != classOf[reranked[pos]] {
			t.Fatalf("position %d: base row ws/class %s/%d, reranked row %s/%d", pos, wsOf[today[pos]], classOf[today[pos]], wsOf[reranked[pos]], classOf[reranked[pos]])
		}
		// Priority workspace: the very same row.
		if wsOf[today[pos]] == p && today[pos] != reranked[pos] {
			t.Errorf("priority workspace position %d changed: %s -> %s", pos, today[pos], reranked[pos])
		}
	}
	// Rank workspace: within each class its rows come out by rank DESC, and it
	// holds exactly the same set of rows.
	for class := 0; class <= 1; class++ {
		var got []int
		for _, id := range reranked {
			if wsOf[id] == r && classOf[id] == class {
				got = append(got, rankOf[id])
			}
		}
		sorted := slices.Clone(got)
		slices.SortFunc(sorted, func(a, b int) int { return b - a })
		if !slices.Equal(got, sorted) {
			t.Errorf("rank workspace class %d ranks %v not descending", class, got)
		}
	}
	if fmt.Sprint(slices.Sorted(slices.Values(reranked))) != fmt.Sprint(slices.Sorted(slices.Values(today))) {
		t.Errorf("rerank changed the row set")
	}
	// Under the rank rule the rank workspace actually moved something, so the
	// fixture proves more than identity.
	moved := false
	for pos := range today {
		if wsOf[today[pos]] == r && today[pos] != reranked[pos] {
			moved = true
		}
	}
	if !moved {
		t.Error("fixture never moved a rank-workspace row")
	}
}

func queryIDs(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) []string {
	t.Helper()
	rs, err := pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMigrationDown removes everything the up file created.
func TestMigrationDown(t *testing.T) {
	pool := sandbox(t)
	exec(t, pool, migration(t, "99d_queue_rank.up.sql"))
	exec(t, pool, migration(t, "99d_queue_rank.down.sql"))
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = current_schema() AND p.proname LIKE 'queue_r%')
		     + (SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname = current_schema() AND t.typname = 'queue_rank_cand')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("down left %d objects behind", n)
	}
}
