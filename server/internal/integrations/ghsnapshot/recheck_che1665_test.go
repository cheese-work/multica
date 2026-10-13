package ghsnapshot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1665: a decided snapshot is re-checked on a slow TTL, so a PR whose base
// moved by a direct push (no pull_request event) is still seen turning DIRTY.

// recheckManager is a stopped manager over the real database: sweepOnce fills
// its queue, and drain plays the worker so the test controls each batch.
func recheckManager(t *testing.T, pool *pgxpool.Pool, now time.Time) *Manager {
	t.Helper()
	m := NewManagerWithFetcher(enabledClient(t), db.New(pool), pool,
		func(_ context.Context, _ *Client, _ int64, _, _ string, _ int32) (*PRSnapshot, error) {
			return nil, nil // replaced per drain
		}, nil)
	m.now = func() time.Time { return now }
	m.jitter = func() time.Duration { return 0 }
	return m
}

// drain pops every queued address, keeps the ones under installs, and
// refreshes them with a decided CLEAN snapshot for their current head. An
// address with an empty head fails its fetch, like a repository the app can
// no longer read.
func drain(t *testing.T, m *Manager, heads map[address]string, installs ...int64) []address {
	t.Helper()
	keep := map[int64]bool{}
	for _, id := range installs {
		keep[id] = true
	}
	m.fetch = func(_ context.Context, _ *Client, inst int64, owner, repo string, n int32) (*PRSnapshot, error) {
		head := heads[address{InstallationID: inst, Owner: owner, Repo: repo, Number: n}]
		if head == "" {
			return nil, errors.New("not found")
		}
		return &PRSnapshot{HeadSHA: head, BaseRef: "main", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}, nil
	}
	var got []address
	for len(m.queue) > 0 {
		addr := <-m.queue
		if keep[addr.InstallationID] {
			got = append(got, addr)
			m.process(context.Background(), addr)
		}
		m.finish(addr)
	}
	return got
}

func setSnapshotAt(t *testing.T, pool *pgxpool.Pool, pr db.GithubPullRequest, fetchedAt time.Time, mergeable, rollup string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE github_pull_request
		SET snapshot_head_sha=head_sha, snapshot_fetched_at=$2, snapshot_base_ref='main',
		    api_mergeable=$3, api_merge_state_status='CLEAN', checks_rollup_state=$4
		WHERE id=$1`, pr.ID, fetchedAt, mergeable, rollup); err != nil {
		t.Fatal(err)
	}
}

func cleanupPRs(t *testing.T, pool *pgxpool.Pool, prs ...db.GithubPullRequest) {
	t.Cleanup(func() {
		for _, pr := range prs {
			_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, pr.ID)
			_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr.ID)
		}
	})
}

func addrOf(pr db.GithubPullRequest) address {
	return address{InstallationID: pr.InstallationID, Owner: pr.RepoOwner, Repo: pr.RepoName, Number: pr.PrNumber}
}

// The sweep returns a decided snapshot older than the 1-hour re-check TTL, not
// a fresh decided one, and still returns a stale undecided one as before.
func TestSweepRechecksDecidedSnapshotsOlderThanTTL(t *testing.T) {
	pool := testDBPool(t)
	q := db.New(pool)
	now := time.Unix(1_200_000_000, 0)
	const inst int64 = 1665001

	oldDecided := seedPRAt(t, pool, q, inst, "old-decided", 1, "A")
	freshDecided := seedPRAt(t, pool, q, inst, "fresh-decided", 2, "B")
	staleUndecided := seedPRAt(t, pool, q, inst, "stale-undecided", 3, "C")
	cleanupPRs(t, pool, oldDecided, freshDecided, staleUndecided)
	setSnapshotAt(t, pool, oldDecided, now.Add(-2*time.Hour), "MERGEABLE", "SUCCESS")
	setSnapshotAt(t, pool, freshDecided, now.Add(-30*time.Minute), "MERGEABLE", "SUCCESS")
	setSnapshotAt(t, pool, staleUndecided, now.Add(-20*time.Minute), "UNKNOWN", "PENDING")

	m := recheckManager(t, pool, now)
	m.sweepOnce(context.Background())
	got := map[address]bool{}
	for len(m.queue) > 0 {
		addr := <-m.queue
		got[addr] = true
		m.finish(addr)
	}
	if !got[addrOf(oldDecided)] {
		t.Error("decided snapshot older than the re-check TTL was not re-checked")
	}
	if got[addrOf(freshDecided)] {
		t.Error("decided snapshot younger than the re-check TTL was re-checked")
	}
	if !got[addrOf(staleUndecided)] {
		t.Error("stale undecided snapshot left the fast sweep")
	}
}

// Snapshots that are already old when the server starts drain over several
// sweeps, each bounded per installation, without any new event. No
// installation takes more than its share of a batch, so a busy installation
// cannot starve a quiet one.
func TestSweepCatchUpDrainsOldSnapshotsInBoundedFairBatches(t *testing.T) {
	pool := testDBPool(t)
	q := db.New(pool)
	now := time.Unix(1_200_100_000, 0)
	const busy, quiet int64 = 1665002, 1665003

	busyPRs := []db.GithubPullRequest{
		seedPRAt(t, pool, q, busy, "busy", 1, "B1"),
		seedPRAt(t, pool, q, busy, "busy", 2, "B2"),
		seedPRAt(t, pool, q, busy, "busy", 3, "B3"),
	}
	quietPR := seedPRAt(t, pool, q, quiet, "quiet", 1, "Q1")
	cleanupPRs(t, pool, append(busyPRs, quietPR)...)
	heads := map[address]string{addrOf(quietPR): "Q1"}
	for i, pr := range busyPRs {
		heads[addrOf(pr)] = pr.HeadSha
		// Oldest first: busy #1 is the oldest snapshot overall.
		setSnapshotAt(t, pool, pr, now.Add(-time.Duration(72-i)*time.Hour), "MERGEABLE", "SUCCESS")
	}
	setSnapshotAt(t, pool, quietPR, now.Add(-48*time.Hour), "MERGEABLE", "SUCCESS")

	m := recheckManager(t, pool, now)
	m.sweepMaxRows = 4 // one address per installation per batch

	m.sweepOnce(context.Background())
	first := drain(t, m, heads, busy, quiet)
	if len(first) != 2 {
		t.Fatalf("first catch-up batch = %+v, want one busy and one quiet address", first)
	}
	firstSet := map[address]bool{first[0]: true, first[1]: true}
	if !firstSet[addrOf(busyPRs[0])] || !firstSet[addrOf(quietPR)] {
		t.Fatalf("first catch-up batch = %+v, want the oldest busy address and the quiet one", first)
	}

	for i, want := range []db.GithubPullRequest{busyPRs[1], busyPRs[2]} {
		m.sweepOnce(context.Background())
		got := drain(t, m, heads, busy, quiet)
		if len(got) != 1 || got[0] != addrOf(want) {
			t.Fatalf("catch-up batch %d = %+v, want only %+v", i+2, got, addrOf(want))
		}
	}

	m.sweepOnce(context.Background())
	if rest := drain(t, m, heads, busy, quiet); len(rest) != 0 {
		t.Fatalf("drained snapshots were re-checked again before the TTL: %+v", rest)
	}
}

// Old addresses whose fetch keeps failing must not hold the per-installation
// or the global batch cap: selection moves past them, so a healthy decided PR
// in the same installation, and one in another installation, still get
// re-checked.
func TestSweepRecheckMovesPastFailingAddresses(t *testing.T) {
	pool := testDBPool(t)
	q := db.New(pool)
	now := time.Unix(1_200_200_000, 0)
	installs := []int64{1665011, 1665012, 1665013, 1665014}
	const other int64 = 1665015

	var failing []db.GithubPullRequest
	for _, inst := range installs {
		failing = append(failing, seedPRAt(t, pool, q, inst, "gone", 1, "F"))
	}
	healthySame := seedPRAt(t, pool, q, installs[0], "live", 2, "H1")
	healthyOther := seedPRAt(t, pool, q, other, "live", 3, "H2")
	cleanupPRs(t, pool, append(failing, healthySame, healthyOther)...)
	heads := map[address]string{addrOf(healthySame): "H1", addrOf(healthyOther): "H2"}
	for _, pr := range failing {
		heads[addrOf(pr)] = "" // fetch fails
		setSnapshotAt(t, pool, pr, now.Add(-72*time.Hour), "MERGEABLE", "SUCCESS")
	}
	setSnapshotAt(t, pool, healthySame, now.Add(-24*time.Hour), "MERGEABLE", "SUCCESS")
	setSnapshotAt(t, pool, healthyOther, now.Add(-24*time.Hour), "MERGEABLE", "SUCCESS")

	m := recheckManager(t, pool, now)
	m.sweepMaxRows = 4 // one address per installation, four per batch
	all := append([]int64{other}, installs...)

	m.sweepOnce(context.Background())
	first := drain(t, m, heads, all...)
	if len(first) != 4 {
		t.Fatalf("first batch = %+v, want the four oldest (failing) addresses", first)
	}

	m.sweepOnce(context.Background())
	second := map[address]bool{}
	for _, a := range drain(t, m, heads, all...) {
		second[a] = true
	}
	if !second[addrOf(healthySame)] {
		t.Errorf("healthy PR behind a failing address in its installation was not re-checked: %+v", second)
	}
	if !second[addrOf(healthyOther)] {
		t.Errorf("healthy PR in another installation was not re-checked while failing addresses filled the batch: %+v", second)
	}
	for _, pr := range failing {
		if second[addrOf(pr)] {
			t.Errorf("failing address %+v was selected again before its re-check TTL", addrOf(pr))
		}
	}
}
