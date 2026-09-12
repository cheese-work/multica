package ghsnapshot

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func testDBPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Skipf("skipping DB test: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("skipping DB test: database not reachable: %v", err)
	}
	// Close via t.Cleanup, registered FIRST, so it runs LAST — after every
	// row-deleting cleanup the test registers later, and after the context
	// cancel that stops the manager's workers. The previous shape, a `defer
	// pool.Close()` at each call site, ran BEFORE any t.Cleanup: the row
	// deletions then executed against a closed pool and failed silently, so
	// every run leaked its PR rows into the shared database. The refresh
	// worker looks rows up by ADDRESS (installation/owner/repo/number), which
	// every test here shares — once enough leaked rows accumulate, a sweep
	// applies a snapshot to a leaked row first, the onApplied signal fires for
	// THAT row, and the test asserts on its own row before the worker reaches
	// it. That is the "trailing refresh did not replace old snapshot" failure,
	// and the "closed pool" WARN spam was the same ordering bug seen from the
	// worker's side.
	t.Cleanup(pool.Close)
	return pool
}

// seedWorkspace inserts a minimal workspace and registers its cleanup.
// github_pull_request carries a workspace_id foreign key (it predates the
// no-FK convention), so the row must reference a real workspace.
func seedWorkspace(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	slug := "ghsnap-" + randHex(t)
	var wsID pgtype.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ($1,$2,$3,$4) RETURNING id`,
		"ghsnap test", slug, "ghsnap test workspace", "GHS").Scan(&wsID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspace WHERE id=$1`, wsID)
	})
	return wsID
}

// seedWorkspaceWithSettings inserts a minimal workspace with an explicit
// settings JSON payload (e.g. `{"github_enabled": false}`) and registers its
// cleanup. Used by the CHE-374 review round 4, item 2 regression tests below
// to exercise the workspace-enablement gate on the ghsnapshot pipeline.
func seedWorkspaceWithSettings(t *testing.T, pool *pgxpool.Pool, settingsJSON string) pgtype.UUID {
	t.Helper()
	slug := "ghsnap-" + randHex(t)
	var wsID pgtype.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO workspace (name, slug, description, issue_prefix, settings) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		"ghsnap test", slug, "ghsnap test workspace", "GHS", settingsJSON).Scan(&wsID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspace WHERE id=$1`, wsID)
	})
	return wsID
}

// seedPRForWorkspace inserts a github_pull_request row bound to an explicit,
// caller-supplied workspace — unlike seedPRAt, which always creates its own
// fresh workspace. This lets a test put two PR rows that share the same
// (installation, owner, repo, number) address (the ghsnapshot fan-out key)
// across two DIFFERENT workspaces with different github_enabled settings.
func seedPRForWorkspace(t *testing.T, q *db.Queries, wsID pgtype.UUID, installationID int64, repoName string, prNumber int32, headSHA string) db.GithubPullRequest {
	t.Helper()
	ts := pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), Valid: true}
	pr, err := q.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
		WorkspaceID:    wsID,
		InstallationID: installationID,
		RepoOwner:      "o",
		RepoName:       repoName,
		PrNumber:       prNumber,
		Title:          "t",
		State:          "open",
		HtmlUrl:        "http://x",
		HeadSha:        headSHA,
		PrCreatedAt:    ts,
		PrUpdatedAt:    ts,
	})
	if err != nil {
		t.Fatalf("seed PR: %v", err)
	}
	return pr
}

func randHex(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 16)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0x0f]
	}
	return string(out)
}

func seedPR(t *testing.T, pool *pgxpool.Pool, q *db.Queries, headSHA string) db.GithubPullRequest {
	return seedPRAt(t, pool, q, 987654, "r", 4242, headSHA)
}

func seedPRAt(t *testing.T, pool *pgxpool.Pool, q *db.Queries, installationID int64, repoName string, prNumber int32, headSHA string) db.GithubPullRequest {
	t.Helper()
	ts := pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), Valid: true}
	pr, err := q.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
		WorkspaceID:    seedWorkspace(t, pool),
		InstallationID: installationID,
		RepoOwner:      "o",
		RepoName:       repoName,
		PrNumber:       prNumber,
		Title:          "t",
		State:          "open",
		HtmlUrl:        "http://x",
		HeadSha:        headSHA,
		PrCreatedAt:    ts,
		PrUpdatedAt:    ts,
	})
	if err != nil {
		t.Fatalf("seed PR: %v", err)
	}
	return pr
}

func checkRunCount(t *testing.T, pool *pgxpool.Pool, prID pgtype.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM github_pull_request_check_run WHERE pr_id=$1`, prID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestListStaleUndecidedGitHubPRsExcludesDecidedAndRotatesCursor(t *testing.T) {
	pool := testDBPool(t)
	q := db.New(pool)
	ctx := context.Background()
	now := time.Unix(1_700_010_000, 0)

	settled := seedPRAt(t, pool, q, 111, "settled", 1, "S")
	oldest := seedPRAt(t, pool, q, 111, "oldest", 2, "O")
	running := seedPRAt(t, pool, q, 222, "running", 3, "R")
	newer := seedPRAt(t, pool, q, 222, "newer", 4, "N")
	prs := []db.GithubPullRequest{settled, oldest, running, newer}
	t.Cleanup(func() {
		for _, pr := range prs {
			_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, pr.ID)
			_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr.ID)
		}
	})

	setSnapshot := func(pr db.GithubPullRequest, fetchedAt time.Time, mergeable, rollup string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			UPDATE github_pull_request
			SET snapshot_head_sha=head_sha, snapshot_fetched_at=$2,
			    api_mergeable=$3, checks_rollup_state=$4
			WHERE id=$1`,
			pr.ID, fetchedAt, mergeable, rollup); err != nil {
			t.Fatal(err)
		}
	}
	setSnapshot(settled, now.Add(-time.Hour), "MERGEABLE", "SUCCESS")
	setSnapshot(oldest, now.Add(-40*time.Minute), "UNKNOWN", "PENDING")
	setSnapshot(running, now.Add(-30*time.Minute), "MERGEABLE", "SUCCESS")
	setSnapshot(newer, now.Add(-20*time.Minute), "MERGEABLE", "PENDING")
	if _, err := pool.Exec(ctx, `
		INSERT INTO github_pull_request_check_run
		    (pr_id, head_sha, ordinal, name, status, is_status_context)
		VALUES ($1, 'R', 0, 'backend', 'in_progress', false)`,
		running.ID); err != nil {
		t.Fatal(err)
	}

	rows, err := q.ListStaleUndecidedGitHubPRs(ctx, db.ListStaleUndecidedGitHubPRsParams{
		OlderThan:           tsFromTime(now.Add(-10 * time.Minute)),
		AfterInstallationID: 0,
		AfterRepoOwner:      "",
		AfterRepoName:       "",
		AfterPrNumber:       0,
		MaxRows:             10,
	})
	if err != nil {
		t.Fatal(err)
	}
	gotRepos := make(map[string]bool, len(rows))
	for _, row := range rows {
		gotRepos[row.RepoName] = true
	}
	if gotRepos["settled"] {
		t.Fatal("decided snapshot remained in the periodic TTL sweep")
	}
	for _, repo := range []string{"oldest", "running", "newer"} {
		if !gotRepos[repo] {
			t.Fatalf("undecided repo %q missing from TTL sweep: %+v", repo, rows)
		}
	}

	first, err := q.ListStaleUndecidedGitHubPRs(ctx, db.ListStaleUndecidedGitHubPRsParams{
		OlderThan:           tsFromTime(now.Add(-10 * time.Minute)),
		AfterInstallationID: 0,
		AfterRepoOwner:      "",
		AfterRepoName:       "",
		AfterPrNumber:       0,
		MaxRows:             1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].RepoName != "oldest" {
		t.Fatalf("first bounded sweep = %+v, want first address", first)
	}

	// Advance from the last returned address without changing its stale data.
	// Even a perpetually failing first address cannot pin the LIMIT forever.
	second, err := q.ListStaleUndecidedGitHubPRs(ctx, db.ListStaleUndecidedGitHubPRsParams{
		OlderThan:           tsFromTime(now.Add(-10 * time.Minute)),
		AfterInstallationID: first[0].InstallationID,
		AfterRepoOwner:      first[0].RepoOwner,
		AfterRepoName:       first[0].RepoName,
		AfterPrNumber:       first[0].PrNumber,
		MaxRows:             1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].RepoName != "newer" {
		t.Fatalf("second bounded sweep = %+v, want next address", second)
	}
}

// TestApplySnapshotHeadSHAGuard is the acceptance-criterion-1 regression: a slow
// response for an old head must never overwrite a newer head's snapshot.
func TestApplySnapshotHeadSHAGuard(t *testing.T) {
	pool := testDBPool(t)
	q := db.New(pool)
	ctx := context.Background()
	now := time.Unix(1_700_000_100, 0)

	pr := seedPR(t, pool, q, "B")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, pr.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr.ID)
	})

	m := &Manager{queries: q, pool: pool, now: func() time.Time { return now }}

	// 1. A response for head "A" while the row is at "B" → discarded, nothing written.
	applied, err := m.applySnapshot(ctx, pr.ID, &PRSnapshot{HeadSHA: "A", Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY"})
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("mismatched-head snapshot must be discarded")
	}
	got, _ := q.GetGitHubPullRequestByID(ctx, pr.ID)
	if got.SnapshotHeadSha != "" || got.ApiMergeable.Valid {
		t.Fatalf("discarded write leaked into row: %+v", got)
	}

	// 2. Matching head "B" → applied; snapshot columns + per-check rows written.
	applied, err = m.applySnapshot(ctx, pr.ID, &PRSnapshot{
		HeadSHA: "B", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", HasChecks: true, RollupState: "FAILURE",
		Contexts: []CheckContext{
			{Name: "backend", Status: "completed", Conclusion: "failure"},
			{Name: "vercel", Status: "completed", Conclusion: "success", IsStatusContext: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("matching-head snapshot must apply")
	}
	got, _ = q.GetGitHubPullRequestByID(ctx, pr.ID)
	if got.SnapshotHeadSha != "B" || got.ApiMergeable.String != "MERGEABLE" || got.ChecksRollupState.String != "FAILURE" {
		t.Fatalf("snapshot not written: %+v", got)
	}
	if n := checkRunCount(t, pool, pr.ID); n != 2 {
		t.Fatalf("check runs = %d, want 2", n)
	}

	// 3. Head advances to "C" (a new push mirrored by the pull_request webhook);
	//    a late in-flight response for the old head "B" must be discarded and must
	//    NOT overwrite the stored snapshot.
	if _, err := pool.Exec(ctx, `UPDATE github_pull_request SET head_sha='C' WHERE id=$1`, pr.ID); err != nil {
		t.Fatal(err)
	}
	applied, err = m.applySnapshot(ctx, pr.ID, &PRSnapshot{HeadSHA: "B", Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY"})
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("late response for the old head must be discarded once head advanced")
	}
	got, _ = q.GetGitHubPullRequestByID(ctx, pr.ID)
	if got.SnapshotHeadSha != "B" || got.ApiMergeable.String != "MERGEABLE" {
		t.Fatalf("stale late write corrupted the snapshot: %+v", got)
	}
	if n := checkRunCount(t, pool, pr.ID); n != 2 {
		t.Fatalf("check runs after stale late write = %d, want 2 (unchanged)", n)
	}
}

// TestApplySnapshotDiscardsWriteWhenDisabledBetweenSelectionAndWrite is the
// CHE-374 review round 5, item 2 regression: Manager.process re-selects
// eligible rows once before its per-row apply loop, but a workspace can still
// flip github_enabled to false in the window between that re-select and this
// specific row's applySnapshot call (e.g. another row in the same fan-out
// batch is slow, or the flip lands mid-loop). Row selection filtering
// eligibility is not enough — the write itself must re-check it. This drives
// applySnapshot directly (the actual UPDATE ... EXISTS guard added to
// UpdateGitHubPRSnapshot), bypassing process()'s own pre-fetch eligibility
// check entirely, so it isolates the write-time guard from the two
// process()-level regressions already covered by
// TestProcessSkipsFetchWhenAllFanOutWorkspacesDisabled and
// TestProcessAppliesOnlyToEnabledWorkspaceInSharedInstallation.
func TestApplySnapshotDiscardsWriteWhenDisabledBetweenSelectionAndWrite(t *testing.T) {
	pool := testDBPool(t)
	q := db.New(pool)
	ctx := context.Background()

	ws := seedWorkspaceWithSettings(t, pool, `{}`) // enabled at selection time
	pr := seedPRForWorkspace(t, q, ws, 555333, "toctou-repo", 12, "A")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, pr.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr.ID)
	})

	m := &Manager{queries: q, pool: pool, now: func() time.Time { return time.Unix(1_700_000_200, 0) }}

	// Row selection (ListGitHubPRRowsByAddress) would have returned this row
	// here — the workspace is still enabled. Now simulate the flip landing in
	// the window between that selection and this row's write.
	if _, err := pool.Exec(ctx, `UPDATE workspace SET settings = '{"github_enabled": false}' WHERE id=$1`, ws); err != nil {
		t.Fatal(err)
	}

	applied, err := m.applySnapshot(ctx, pr.ID, &PRSnapshot{HeadSHA: "A", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"})
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("write must be discarded once the workspace disabled GitHub, even though the row was eligible at selection time")
	}

	got, err := q.GetGitHubPullRequestByID(ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SnapshotHeadSha != "" || got.ApiMergeable.Valid {
		t.Fatalf("snapshot was written despite the workspace disabling GitHub before the write: %+v", got)
	}
}

// TestInFlightOldHeadKeepsTrailingRefresh covers the synchronize race from the
// PR review: while head A is fetching, a webhook advances the mirrored row to B
// and enqueues again. A is discarded by the head guard, but the coalesced
// trailing edge must still fetch and apply B immediately.
func TestInFlightOldHeadKeepsTrailingRefresh(t *testing.T) {
	pool := testDBPool(t)
	q := db.New(pool)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	pr := seedPR(t, pool, q, "A")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, pr.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr.ID)
	})

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondFetched := make(chan struct{})
	applied := make(chan struct{}, 1)
	fetchCalls := 0

	m := NewManager(enabledClient(t), q, pool, func(context.Context, pgtype.UUID) {
		select {
		case applied <- struct{}{}:
		default:
		}
	})
	m.concurrency = 2
	m.sweepInterval = time.Hour
	m.jitter = func() time.Duration { return 0 }
	m.fetch = func(context.Context, *Client, int64, string, string, int32) (*PRSnapshot, error) {
		fetchCalls++
		if fetchCalls == 1 {
			close(firstStarted)
			<-releaseFirst
			return &PRSnapshot{
				HeadSHA: "A", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN",
				HasChecks: true, RollupState: "SUCCESS",
			}, nil
		}
		close(secondFetched)
		return &PRSnapshot{
			HeadSHA: "B", Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY",
			HasChecks: true, RollupState: "FAILURE",
			Contexts: []CheckContext{{Name: "backend", Status: "completed", Conclusion: "failure"}},
		}, nil
	}

	m.Start(ctx)
	m.Enqueue(pr.InstallationID, pr.RepoOwner, pr.RepoName, pr.PrNumber)
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first head fetch did not start")
	}

	if _, err := pool.Exec(ctx, `UPDATE github_pull_request SET head_sha='B' WHERE id=$1`, pr.ID); err != nil {
		t.Fatal(err)
	}
	m.Enqueue(pr.InstallationID, pr.RepoOwner, pr.RepoName, pr.PrNumber)

	select {
	case <-secondFetched:
		t.Fatal("second fetch started concurrently; single-PR in-flight guard failed")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)

	select {
	case <-secondFetched:
	case <-time.After(2 * time.Second):
		t.Fatal("new-head trailing refresh was swallowed")
	}
	select {
	case <-applied:
	case <-time.After(2 * time.Second):
		t.Fatal("new-head snapshot was not applied")
	}

	got, err := q.GetGitHubPullRequestByID(context.Background(), pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SnapshotHeadSha != "B" || got.ApiMergeable.String != "CONFLICTING" {
		t.Fatalf("trailing refresh did not replace old snapshot: %+v", got)
	}
	if n := checkRunCount(t, pool, pr.ID); n != 1 {
		t.Fatalf("new-head check runs = %d, want 1", n)
	}
}

// TestApplySnapshotReplacesRuns proves each successful apply is an atomic batch
// replace, not an accumulation.
func TestApplySnapshotReplacesRuns(t *testing.T) {
	pool := testDBPool(t)
	q := db.New(pool)
	ctx := context.Background()
	now := time.Unix(1_700_000_200, 0)

	pr := seedPR(t, pool, q, "H")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, pr.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr.ID)
	})
	m := &Manager{queries: q, pool: pool, now: func() time.Time { return now }}

	three := &PRSnapshot{HeadSHA: "H", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", HasChecks: true, RollupState: "PENDING",
		Contexts: []CheckContext{{Name: "a", Status: "in_progress"}, {Name: "b", Status: "in_progress"}, {Name: "c", Status: "in_progress"}}}
	if _, err := m.applySnapshot(ctx, pr.ID, three); err != nil {
		t.Fatal(err)
	}
	if n := checkRunCount(t, pool, pr.ID); n != 3 {
		t.Fatalf("after first apply: %d runs, want 3", n)
	}
	one := &PRSnapshot{HeadSHA: "H", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", HasChecks: true, RollupState: "SUCCESS",
		Contexts: []CheckContext{{Name: "a", Status: "completed", Conclusion: "success"}}}
	if _, err := m.applySnapshot(ctx, pr.ID, one); err != nil {
		t.Fatal(err)
	}
	if n := checkRunCount(t, pool, pr.ID); n != 1 {
		t.Fatalf("after replace: %d runs, want 1 (old runs deleted)", n)
	}
}

// TestProcessSkipsFetchWhenAllFanOutWorkspacesDisabled is the CHE-374 review
// round 4, item 2 regression: master-off (github_enabled=false) must give the
// ghsnapshot pipeline zero footprint, including zero outbound GitHub API
// calls, not just a discarded write. A single workspace with GitHub disabled
// mirrors the address; process() must never call fetch.
func TestProcessSkipsFetchWhenAllFanOutWorkspacesDisabled(t *testing.T) {
	pool := testDBPool(t)
	q := db.New(pool)
	ctx := context.Background()

	ws := seedWorkspaceWithSettings(t, pool, `{"github_enabled": false}`)
	pr := seedPRForWorkspace(t, q, ws, 555111, "off-repo", 91, "A")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, pr.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr.ID)
	})

	m := NewManager(enabledClient(t), q, pool, nil)
	m.jitter = func() time.Duration { return 0 }
	m.ctx = ctx
	fetchCalled := false
	m.fetch = func(context.Context, *Client, int64, string, string, int32) (*PRSnapshot, error) {
		fetchCalled = true
		return &PRSnapshot{HeadSHA: "A", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}, nil
	}

	m.process(ctx, address{InstallationID: 555111, Owner: "o", Repo: "off-repo", Number: 91})

	if fetchCalled {
		t.Fatal("outbound fetch was called even though every fan-out workspace has github_enabled=false")
	}
	got, err := q.GetGitHubPullRequestByID(ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SnapshotHeadSha != "" || got.ApiMergeable.Valid {
		t.Fatalf("row was written despite the workspace being disabled: %+v", got)
	}
}

// TestProcessAppliesOnlyToEnabledWorkspaceInSharedInstallation covers the
// mixed shared-installation case (CHE-374 review round 4, item 2): one GitHub
// installation fans out to two workspaces mirroring the SAME (installation,
// owner, repo, number) address (#4823/#4855 fan-out). One workspace still has
// GitHub enabled, the other explicitly disabled. The fetch is justified by the
// enabled workspace, but the disabled workspace's row must never receive the
// snapshot write — even though it shares the fetched address.
func TestProcessAppliesOnlyToEnabledWorkspaceInSharedInstallation(t *testing.T) {
	pool := testDBPool(t)
	q := db.New(pool)
	ctx := context.Background()

	const installationID = 555222
	const repoName = "shared-repo"
	const prNumber = int32(77)

	enabledWS := seedWorkspaceWithSettings(t, pool, `{}`)
	disabledWS := seedWorkspaceWithSettings(t, pool, `{"github_enabled": false}`)
	enabledPR := seedPRForWorkspace(t, q, enabledWS, installationID, repoName, prNumber, "A")
	disabledPR := seedPRForWorkspace(t, q, disabledWS, installationID, repoName, prNumber, "A")
	t.Cleanup(func() {
		for _, pr := range []db.GithubPullRequest{enabledPR, disabledPR} {
			_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, pr.ID)
			_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr.ID)
		}
	})

	m := NewManager(enabledClient(t), q, pool, nil)
	m.jitter = func() time.Duration { return 0 }
	m.ctx = ctx
	fetchCalls := 0
	m.fetch = func(context.Context, *Client, int64, string, string, int32) (*PRSnapshot, error) {
		fetchCalls++
		return &PRSnapshot{HeadSHA: "A", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}, nil
	}

	m.process(ctx, address{InstallationID: installationID, Owner: "o", Repo: repoName, Number: prNumber})

	if fetchCalls != 1 {
		t.Fatalf("fetch calls = %d, want 1 (justified by the enabled workspace's row)", fetchCalls)
	}

	gotEnabled, err := q.GetGitHubPullRequestByID(ctx, enabledPR.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotEnabled.SnapshotHeadSha != "A" || !gotEnabled.ApiMergeable.Valid {
		t.Fatalf("enabled workspace's row was not written: %+v", gotEnabled)
	}

	gotDisabled, err := q.GetGitHubPullRequestByID(ctx, disabledPR.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotDisabled.SnapshotHeadSha != "" || gotDisabled.ApiMergeable.Valid {
		t.Fatalf("disabled workspace's row was written despite github_enabled=false: %+v", gotDisabled)
	}
}

// TestSweepExcludesAddressWhenAllFanOutWorkspacesDisabled covers the 4th
// trigger path (CHE-374 review round 4, item 2): the TTL/safety-net sweep
// (ListStaleUndecidedGitHubPRs) must never surface an address whose only
// fan-out workspace has GitHub disabled, so a master-off PR never even enters
// the refresh queue via the periodic sweep — independent of the per-process
// eligibility check exercised by the two tests above.
func TestSweepExcludesAddressWhenAllFanOutWorkspacesDisabled(t *testing.T) {
	pool := testDBPool(t)
	q := db.New(pool)
	ctx := context.Background()
	now := time.Unix(1_700_020_000, 0)

	disabledWS := seedWorkspaceWithSettings(t, pool, `{"github_enabled": false}`)
	enabledWS := seedWorkspaceWithSettings(t, pool, `{}`)
	offPR := seedPRForWorkspace(t, q, disabledWS, 555333, "off-sweep", 5, "S")
	onPR := seedPRForWorkspace(t, q, enabledWS, 555333, "on-sweep", 6, "S")
	prs := []db.GithubPullRequest{offPR, onPR}
	t.Cleanup(func() {
		for _, pr := range prs {
			_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, pr.ID)
			_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr.ID)
		}
	})

	rows, err := q.ListStaleUndecidedGitHubPRs(ctx, db.ListStaleUndecidedGitHubPRsParams{
		OlderThan:           tsFromTime(now),
		AfterInstallationID: 0,
		AfterRepoOwner:      "",
		AfterRepoName:       "",
		AfterPrNumber:       0,
		MaxRows:             50,
	})
	if err != nil {
		t.Fatal(err)
	}
	gotRepos := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.InstallationID == 555333 {
			gotRepos[row.RepoName] = true
		}
	}
	if gotRepos["off-sweep"] {
		t.Fatal("sweep surfaced an address whose only fan-out workspace has github_enabled=false")
	}
	if !gotRepos["on-sweep"] {
		t.Fatal("sweep must still surface an address backed by an enabled workspace")
	}
}
