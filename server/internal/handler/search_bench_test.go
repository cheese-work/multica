package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

// Search latency harness (CHE-3164). Skipped unless MULTICA_SEARCH_BENCH=1, so
// it never runs in CI. It seeds one workspace in the scratch database that
// DATABASE_URL names, runs the issue search the way SearchIssues does (the
// normal query, then the typo-tolerant fallback when nothing matched), and
// appends the wall-clock time of every run to a JSON file. Run it once per
// revision, then compare.
//
//	MULTICA_SEARCH_BENCH=1 BENCH_LABEL=before BENCH_OUT=/tmp/bench.json \
//	  go test ./internal/handler -run TestSearchBench -count=1 -v
//	MULTICA_SEARCH_BENCH=1 BENCH_COMPARE=before,after BENCH_OUT=/tmp/bench.json \
//	  go test ./internal/handler -run TestSearchBenchReport -count=1 -v
//
// Alternate before/after invocations (before, after, before, after) with a
// smaller BENCH_RUNS: samples with the same label accumulate in the file, so
// slow host load lands on both sides. Never point DATABASE_URL at a shared or
// production database: the seed inserts BENCH_ISSUES issues and BENCH_COMMENTS
// comments, and BENCH_RESET=1 deletes the benchmark workspace's rows first.
//
// Seed knobs: BENCH_ISSUES (default 6000), BENCH_COMMENTS (default 4x issues),
// BENCH_RUNS (default 60). The production-shape corpus of CHE-3164 (3,195 issues,
// 61,842 comments in one workspace on C00, read 2 Nov 2026) is
// BENCH_ISSUES=3200 BENCH_COMMENTS=62000. The corpus
// is deterministic: filler words come from a fixed vocabulary by row number,
// and the needles below sit at fixed row-number intervals.
const (
	benchWorkspaceSlug = "che3164-search-bench"
	benchDefaultIssues = 6000
	benchDefaultRuns   = 60
)

// benchVocabulary is the filler. It holds no needle word: none of "login",
// "bug", "dang", "nhap", "đăng" or "nhập" appears in it, and none of them is a
// substring of a word in it.
var benchVocabulary = strings.Fields(`
	server client request response timeout retry cache queue worker thread
	deploy rollback release branch commit review merge pipeline artifact
	database migration index column table query planner vacuum replica
	webhook payload header cookie session token refresh expire policy
	mobile android desktop browser render layout scroll gesture animation
	người dùng hệ_thống lỗi tài_khoản mật_khẩu thông_báo cài_đặt màn_hình
	kiểm_tra triển_khai hiệu_năng bộ_nhớ kết_nối đồng_bộ dữ_liệu
	improve update remove create delete rename split extract inline format
	network latency bandwidth throughput capacity monitor alert metric trace
	feature flag rollout experiment variant control metric dashboard report
	billing invoice payment refund currency tax region locale language
	upload download stream buffer chunk encode decode compress archive
	notification inbox digest schedule reminder calendar timezone holiday
`)

type benchQuery struct {
	class string
	query string
}

var benchQueries = []benchQuery{
	{"login bug", "login bug"},
	{"dang nhap", "dang nhap"},
	{"đăng nhập", "đăng nhập"},
	{"zzzznomatch", "zzzznomatch"},
	{"fallback typo", "logni"},
}

type benchSample struct {
	Label string  `json:"label"`
	Class string  `json:"class"`
	Ms    float64 `json:"ms"`
}

func benchEnvInt(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return fallback
}

func benchPercentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(float64(len(sorted)-1)*p+0.5)]
}

// seedSearchBench creates the benchmark workspace when it is missing and returns
// its id. The needle layout (row n, 1-based):
//   - n%50==0 title "login bug ..."; n%37==1 description holds "login";
//     n%29==2 description holds "bug"; n%45==3 title "Đăng nhập ...";
//     n%60==4 title "dang nhap ...".
//   - comment m: m%200==0 "login bug", m%150==1 "login", m%130==2 "bug",
//     m%180==3 "đăng nhập", m%240==4 "dang nhap".
func seedSearchBench(t *testing.T, ctx context.Context, slug string, issues int) string {
	t.Helper()
	return seedSearchBenchShaped(t, ctx, slug, issues, issues*4)
}

// seedSearchBenchShaped is seedSearchBench with an explicit comment count, for a
// corpus shaped like production (few issues, many comments).
func seedSearchBenchShaped(t *testing.T, ctx context.Context, slug string, issues, comments int) string {
	t.Helper()
	pool := testPool
	var wsID string
	err := pool.QueryRow(ctx, `SELECT id::text FROM workspace WHERE slug = $1`, slug).Scan(&wsID)
	if err == nil && os.Getenv("BENCH_RESET") != "1" {
		var haveIssues, haveComments int
		if err := pool.QueryRow(ctx,
			`SELECT (SELECT count(*) FROM issue WHERE workspace_id = $1), (SELECT count(*) FROM comment WHERE workspace_id = $1)`,
			wsID).Scan(&haveIssues, &haveComments); err != nil {
			t.Fatal(err)
		}
		if haveIssues == issues && haveComments == comments {
			return wsID
		}
	}
	if err == nil {
		for _, stmt := range []string{
			`DELETE FROM comment WHERE workspace_id = $1`,
			`DELETE FROM issue WHERE workspace_id = $1`,
		} {
			if _, err := pool.Exec(ctx, stmt, wsID); err != nil {
				t.Fatal(err)
			}
		}
	} else if err := pool.QueryRow(ctx,
		`INSERT INTO workspace (name, slug, issue_prefix) VALUES ('Search bench', $1, 'SB') RETURNING id::text`,
		slug).Scan(&wsID); err != nil {
		t.Fatal(err)
	}

	// words(mul, count) builds count vocabulary words per row n, picked by a fixed
	// arithmetic walk so the corpus is the same on every run.
	words := func(mul, count int) string {
		return fmt.Sprintf(`(SELECT string_agg(($2::text[])[1 + ((n::bigint * 7919 + %d * 104729 + k::bigint * 15485863) %% %d)], ' ')
			FROM generate_series(1, %d) k)`, mul, len(benchVocabulary), count)
	}
	issueSQL := fmt.Sprintf(`
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number, created_at, updated_at)
		SELECT $1::uuid,
			CASE WHEN n %% 50 = 0 THEN 'login bug ' WHEN n %% 45 = 3 THEN 'Đăng nhập ' WHEN n %% 60 = 4 THEN 'dang nhap ' ELSE '' END || %s,
			CASE WHEN n %% 37 = 1 THEN 'login ' ELSE '' END || CASE WHEN n %% 29 = 2 THEN 'bug ' ELSE '' END || %s,
			(ARRAY['backlog','todo','in_progress','in_review','done','cancelled'])[1 + n %% 6],
			'none', 'member', gen_random_uuid(), n,
			now() - make_interval(mins => n), now() - make_interval(mins => n)
		FROM generate_series(1, %d) n`,
		words(1, 6), words(2, 110), issues)
	// Bulk rows skip the per-row triggers (they cost ~3 ms each on a GIN-indexed
	// table); the search side tables are filled set-based below instead.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// Runs before Release, so a failed seed never returns a trigger-less conn to the pool.
	defer conn.Exec(context.WithoutCancel(ctx), "RESET session_replication_role")
	if _, err := conn.Exec(ctx, "SET session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, issueSQL, wsID, benchVocabulary); err != nil {
		t.Fatalf("seed issues: %v", err)
	}

	commentSQL := fmt.Sprintf(`
		INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content, created_at, updated_at)
		SELECT $1::uuid, i.id,
			'member', gen_random_uuid(),
			CASE WHEN n %% 200 = 0 THEN 'login bug ' WHEN n %% 150 = 1 THEN 'login ' WHEN n %% 130 = 2 THEN 'bug '
				WHEN n %% 180 = 3 THEN 'đăng nhập ' WHEN n %% 240 = 4 THEN 'dang nhap ' ELSE '' END || %s,
			now() - make_interval(secs => n), now() - make_interval(secs => n)
		FROM generate_series(1, %d) n
		JOIN issue i ON i.workspace_id = $1 AND i.number = 1 + (n %% %d)`,
		words(3, 10), comments, issues)
	if _, err := conn.Exec(ctx, commentSQL, wsID, benchVocabulary); err != nil {
		t.Fatalf("seed comments: %v", err)
	}
	if _, err := conn.Exec(ctx, "RESET session_replication_role"); err != nil {
		t.Fatal(err)
	}
	// Same projection as issue_search_text_sync / comment_search_text_sync.
	if _, err := conn.Exec(ctx, `
		INSERT INTO issue_search_text (issue_id, workspace_id, title_folded, description_folded, title_accents, description_accents)
		WITH f AS MATERIALIZED (
			SELECT id, workspace_id, title, description, search_fold(title) AS t, search_fold(description) AS d
			FROM issue WHERE workspace_id = $1)
		SELECT id, workspace_id, t, d,
			CASE WHEN lower(COALESCE(title, '')) <> t THEN lower(COALESCE(title, '')) END,
			CASE WHEN lower(COALESCE(description, '')) <> d THEN lower(COALESCE(description, '')) END
		FROM f
		ON CONFLICT (issue_id) DO NOTHING`, wsID); err != nil {
		t.Fatalf("seed issue search text: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO comment_search_text (comment_id, workspace_id, issue_id, created_at, content_folded, content_accents)
		WITH f AS MATERIALIZED (
			SELECT id, workspace_id, issue_id, created_at, content, search_fold(content) AS c
			FROM comment WHERE workspace_id = $1)
		SELECT id, workspace_id, issue_id, created_at, c,
			CASE WHEN lower(COALESCE(content, '')) <> c THEN lower(COALESCE(content, '')) END
		FROM f
		ON CONFLICT (comment_id) DO NOTHING`, wsID); err != nil {
		t.Fatalf("seed comment search text: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE issue; ANALYZE comment; ANALYZE issue_search_text; ANALYZE comment_search_text`); err != nil {
		t.Fatal(err)
	}
	return wsID
}

// runBenchSearch mirrors SearchIssues: the normal query, then the typo fallback
// when the first page is empty. It returns the issue ids it found.
func runBenchSearch(ctx context.Context, wsUUID pgtype.UUID, q string) ([]string, error) {
	terms := splitSearchTerms(q)
	queryNum, hasNum := parseQueryNumber(q)
	terminal := []string{"done", "cancelled"}
	var ids []string
	run := func(sql string, args []any) error {
		return runSearchQuery(ctx, testPool, sql, args, func(rows pgx.Rows) error {
			for rows.Next() {
				sr, err := scanSearchIssue(rows)
				if err != nil {
					return err
				}
				ids = append(ids, util.UUIDToString(sr.issue.ID))
			}
			return rows.Err()
		})
	}
	sql, args := buildSearchQuery(q, terms, queryNum, hasNum, false, terminal, pgtype.UUID{})
	args[3] = wsUUID
	args[len(args)-2], args[len(args)-1] = 20, 0
	if err := run(sql, args); err != nil {
		return nil, err
	}
	if shouldRunFuzzyFallback(q, hasNum, 0, len(ids)) {
		fsql, fargs := buildFuzzySearchQuery(foldDiacritics(q), false, terminal)
		fargs[1] = wsUUID
		fargs[len(fargs)-2], fargs[len(fargs)-1] = 20, 0
		if err := run(fsql, fargs); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func TestSearchBench(t *testing.T) {
	if os.Getenv("MULTICA_SEARCH_BENCH") != "1" {
		t.Skip("set MULTICA_SEARCH_BENCH=1 to run the search latency harness")
	}
	ctx := context.Background()
	issues := benchEnvInt("BENCH_ISSUES", benchDefaultIssues)
	runs := benchEnvInt("BENCH_RUNS", benchDefaultRuns)
	label := os.Getenv("BENCH_LABEL")
	out := os.Getenv("BENCH_OUT")
	if label == "" || out == "" {
		t.Fatal("set BENCH_LABEL and BENCH_OUT")
	}
	wsID := seedSearchBenchShaped(t, ctx, benchWorkspaceSlug, issues, benchEnvInt("BENCH_COMMENTS", issues*4))
	var counts [2]int
	if err := testPool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM issue WHERE workspace_id = $1), (SELECT count(*) FROM comment WHERE workspace_id = $1)`,
		wsID).Scan(&counts[0], &counts[1]); err != nil {
		t.Fatal(err)
	}
	t.Logf("corpus: %d issues, %d comments", counts[0], counts[1])
	wsUUID := util.MustParseUUID(wsID)

	// Warm up once per query, then interleave: every round runs each class once.
	for _, bq := range benchQueries {
		if _, err := runBenchSearch(ctx, wsUUID, bq.query); err != nil {
			t.Fatalf("warm-up %q: %v", bq.class, err)
		}
	}
	var samples []benchSample
	for round := 0; round < runs; round++ {
		for _, bq := range benchQueries {
			start := time.Now()
			ids, err := runBenchSearch(ctx, wsUUID, bq.query)
			if err != nil {
				t.Fatalf("%q: %v", bq.class, err)
			}
			if round == 0 {
				t.Logf("%-14s %d results", bq.class, len(ids))
			}
			samples = append(samples, benchSample{label, bq.class, float64(time.Since(start).Microseconds()) / 1000})
		}
	}
	appendBenchSamples(t, out, samples)
}

func appendBenchSamples(t *testing.T, path string, samples []benchSample) {
	t.Helper()
	var all []benchSample
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &all); err != nil {
			t.Fatal(err)
		}
	}
	all = append(all, samples...)
	raw, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSearchBenchReport prints median, p25 and p95 per query class for two
// labels from BENCH_OUT, and the change of each statistic.
func TestSearchBenchReport(t *testing.T) {
	pair := strings.Split(os.Getenv("BENCH_COMPARE"), ",")
	if len(pair) != 2 || os.Getenv("BENCH_OUT") == "" {
		t.Skip("set BENCH_COMPARE=before,after and BENCH_OUT to print the comparison")
	}
	raw, err := os.ReadFile(os.Getenv("BENCH_OUT"))
	if err != nil {
		t.Fatal(err)
	}
	var all []benchSample
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	stat := func(label, class string) (n int, p25, med, p95 float64) {
		var ms []float64
		for _, s := range all {
			if s.Label == label && s.Class == class {
				ms = append(ms, s.Ms)
			}
		}
		sort.Float64s(ms)
		return len(ms), benchPercentile(ms, .25), benchPercentile(ms, .5), benchPercentile(ms, .95)
	}
	pct := func(before, after float64) float64 { return (after - before) / before * 100 }
	for _, bq := range benchQueries {
		n0, a25, a50, a95 := stat(pair[0], bq.class)
		n1, b25, b50, b95 := stat(pair[1], bq.class)
		t.Logf("q=%-14q runs %d/%d  median %8.2fms -> %8.2fms (%+6.1f%%)  p25 %8.2f -> %8.2f (%+6.1f%%)  p95 %8.2f -> %8.2f (%+6.1f%%)",
			bq.class, n0, n1, a50, b50, pct(a50, b50), a25, b25, pct(a25, b25), a95, b95, pct(a95, b95))
	}
}

// TestSearchBenchExplain prints EXPLAIN (ANALYZE, BUFFERS) of the normal search
// statement for every query class (BENCH_EXPLAIN=1), against the corpus that
// TestSearchBench seeded.
func TestSearchBenchExplain(t *testing.T) {
	if os.Getenv("BENCH_EXPLAIN") != "1" {
		t.Skip("set BENCH_EXPLAIN=1 to print query plans")
	}
	ctx := context.Background()
	var wsID string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM workspace WHERE slug = $1`, benchWorkspaceSlug).Scan(&wsID); err != nil {
		t.Fatalf("benchmark workspace missing; run TestSearchBench first: %v", err)
	}
	for _, bq := range benchQueries {
		sql, args := buildSearchQuery(bq.query, splitSearchTerms(bq.query), 0, false, false, []string{"done", "cancelled"}, pgtype.UUID{})
		args[3] = util.MustParseUUID(wsID)
		args[len(args)-2], args[len(args)-1] = 20, 0
		var plan []string
		err := runSearchQuery(ctx, testPool, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args, func(rows pgx.Rows) error {
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					return err
				}
				plan = append(plan, line)
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatalf("%q: %v", bq.class, err)
		}
		t.Logf("q=%q\n%s", bq.class, strings.Join(plan, "\n"))
	}
}
