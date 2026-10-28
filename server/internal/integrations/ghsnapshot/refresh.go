package ghsnapshot

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/dbreader"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TxBeginner is the subset of a pgx pool the manager needs to open the
// snapshot-write transaction. *pgxpool.Pool satisfies it.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// address is the refresh unit and the dedup / single-in-flight key
// (acceptance criterion 3): one (installation, owner, repo, number) tuple, which
// may fan out to multiple github_pull_request rows across workspaces.
type address struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int32
}

// Default tuning. The chase-window backoff climbs 30s → 1m → 2m → 5m and holds
// at 5m; a chase stops when the snapshot is decided, the PR closes, or
// maxChaseAttempts is reached — never unbounded (acceptance criterion 7). The
// TTL sweep and page-visit refresh recover anything a stopped chase misses.
var (
	defaultChaseBackoff  = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
	defaultConcurrency   = 12
	defaultViewTTL       = 60 * time.Second
	defaultSweepTTL      = 10 * time.Minute
	defaultSweepInterval = 10 * time.Minute
	defaultSweepMaxRows  = int32(200)
	maxChaseAttempts     = 12
	queueBuffer          = 2048

	// CHE-1665: decided snapshots are re-checked once they are this old, and
	// the first sweep runs this long after start, so snapshots that went stale
	// while the server was down drain without waiting a whole interval.
	defaultRecheckTTL = time.Hour
	defaultCatchUp    = time.Minute
	// PRs watched by a "wake on merge conflict" rule stay in the sweep even
	// when decided, re-fetched once their snapshot is older than this.
	defaultWatchTTL = 5 * time.Minute
	// After a merge into a repository, GitHub recomputes the other PRs'
	// mergeability asynchronously and may still answer with the old verdict
	// at first, so watched PRs are fetched after each of these delays.
	defaultMergeWatchDelays = []time.Duration{30 * time.Second, 3 * time.Minute}
)

// Manager owns the outbound GitHub API refresh pipeline. A Manager whose client
// is nil/disabled is inert: every trigger method is a no-op, so a deployment
// without a GitHub App private key degrades the feature off without touching
// PR linking, merge→Done, or any other existing behavior (acceptance
// criterion 4).
type Manager struct {
	client       *Client
	queries      *db.Queries
	readSelector *dbreader.Selector
	pool         TxBeginner
	onApplied    func(ctx context.Context, prID pgtype.UUID)

	concurrency   int
	viewTTL       time.Duration
	sweepTTL      time.Duration
	sweepInterval time.Duration
	sweepMaxRows  int32
	recheckTTL    time.Duration
	catchUp       time.Duration
	watchTTL      time.Duration
	mergeDelays   []time.Duration
	chaseBackoff  []time.Duration
	now           func() time.Time
	jitter        func() time.Duration
	// fetch is the snapshot fetcher, a seam so tests can drive the queue /
	// backoff without a live GitHub. Defaults to FetchPRSnapshot.
	fetch func(ctx context.Context, c *Client, installationID int64, owner, repo string, number int32) (*PRSnapshot, error)
	// listRows is the row-selection seam used both to gate the outbound fetch
	// (CHE-374 review round 4, item 2) and to select rows for the write.
	// Defaults to listGitHubPRRowsByAddress. Tests that only exercise the
	// rate-limit/backoff machinery around process() (and do not wire a DB)
	// stub this directly, the same way they stub fetch.
	listRows func(ctx context.Context, params db.ListGitHubPRRowsByAddressParams) ([]db.ListGitHubPRRowsByAddressRow, error)

	queue chan address

	mu       sync.Mutex
	active   map[address]bool // queued OR in-flight → coalesce (single in-flight per PR)
	inFlight map[address]bool
	trailing map[address]bool // one event that arrived while active; replay once after the current fetch
	attempts map[address]int  // chase attempts for the current undecided window
	// Last address returned by the bounded TTL sweep. The query starts after
	// this cursor and wraps, preventing a fixed first page from starving later
	// installations when early addresses repeatedly fail.
	sweepAfter address
	// The same rotation for the conflict-watch sweep.
	watchAfter address
	// Secondary rate limits are scoped to the installation whose token incurred
	// them. One customer must never pause every other installation.
	rateUntil map[int64]time.Time

	ctx     context.Context
	started bool
}

// NewManager wires the pipeline. onApplied is called once per PR row whose
// snapshot was actually written (guard passed), so the handler can broadcast a
// realtime PR update. Passing a nil client yields a disabled (no-op) manager.
func NewManager(client *Client, queries *db.Queries, pool TxBeginner, onApplied func(ctx context.Context, prID pgtype.UUID)) *Manager {
	return newManager(client, queries, pool, FetchPRSnapshot, onApplied)
}

// NewManagerWithFetcher wires the normal refresh lifecycle with a supplied
// snapshot fetcher. Production callers should normally use NewManager.
func NewManagerWithFetcher(client *Client, queries *db.Queries, pool TxBeginner, fetch func(context.Context, *Client, int64, string, string, int32) (*PRSnapshot, error), onApplied func(ctx context.Context, prID pgtype.UUID)) *Manager {
	if fetch == nil {
		fetch = FetchPRSnapshot
	}
	return newManager(client, queries, pool, fetch, onApplied)
}

func newManager(client *Client, queries *db.Queries, pool TxBeginner, fetch func(context.Context, *Client, int64, string, string, int32) (*PRSnapshot, error), onApplied func(ctx context.Context, prID pgtype.UUID)) *Manager {
	manager := &Manager{
		client:        client,
		queries:       queries,
		readSelector:  dbreader.NewPrimaryOnly(queries),
		pool:          pool,
		onApplied:     onApplied,
		concurrency:   defaultConcurrency,
		viewTTL:       defaultViewTTL,
		sweepTTL:      defaultSweepTTL,
		sweepInterval: defaultSweepInterval,
		sweepMaxRows:  defaultSweepMaxRows,
		recheckTTL:    defaultRecheckTTL,
		catchUp:       defaultCatchUp,
		watchTTL:      defaultWatchTTL,
		mergeDelays:   defaultMergeWatchDelays,
		chaseBackoff:  defaultChaseBackoff,
		now:           time.Now,
		jitter:        func() time.Duration { return time.Duration(rand.Int63n(int64(250 * time.Millisecond))) },
		fetch:         fetch,
		queue:         make(chan address, queueBuffer),
		active:        map[address]bool{},
		inFlight:      map[address]bool{},
		trailing:      map[address]bool{},
		attempts:      map[address]int{},
		rateUntil:     map[int64]time.Time{},
	}
	manager.listRows = manager.listGitHubPRRowsByAddress
	return manager
}

// SetReadSelector opts the refresh worker into the API server's configured
// replica routing. It must be called before Start; managers used by tests and
// primary-only deployments retain the safe primary default from NewManager.
func (manager *Manager) SetReadSelector(selector *dbreader.Selector) {
	if selector != nil {
		manager.readSelector = selector
	}
}

// Enabled reports whether the pipeline will actually do anything.
func (manager *Manager) Enabled() bool { return manager != nil && manager.client.Enabled() }

// Start launches the worker pool and the TTL sweeper under ctx. No-op (and
// safe) when the manager is disabled.
func (manager *Manager) Start(ctx context.Context) {
	if !manager.Enabled() {
		return
	}
	manager.mu.Lock()
	if manager.started {
		manager.mu.Unlock()
		return
	}
	manager.started = true
	manager.ctx = ctx
	manager.mu.Unlock()

	for i := 0; i < manager.concurrency; i++ {
		go manager.worker(ctx)
	}
	go manager.sweepLoop(ctx)
}

// Enqueue schedules a refresh for a PR address. Repeated events coalesce, but
// an event that arrives while the address is queued or in flight leaves one
// trailing refresh behind. That trailing edge matters when a synchronize event
// advances the head while the old head's request is still running: the guarded
// old response is discarded, then the new head is fetched immediately. At most
// one request per address is in flight, and at most one trailing request is
// retained. Never blocks the caller.
func (manager *Manager) Enqueue(installationID int64, owner, repo string, number int32) {
	if !manager.Enabled() {
		return
	}
	addr := address{InstallationID: installationID, Owner: owner, Repo: repo, Number: number}
	manager.mu.Lock()
	if manager.active[addr] {
		if manager.inFlight[addr] {
			manager.trailing[addr] = true
		}
		manager.mu.Unlock()
		return
	}
	manager.active[addr] = true
	manager.mu.Unlock()

	select {
	case manager.queue <- addr:
	default:
		// Queue is saturated; drop and let the TTL sweep / next event recover
		// rather than block a webhook handler. Unmark so it can be re-enqueued.
		manager.mu.Lock()
		delete(manager.active, addr)
		delete(manager.inFlight, addr)
		delete(manager.trailing, addr)
		manager.mu.Unlock()
		slog.Warn("ghsnapshot: refresh queue full, dropping enqueue")
	}
}

// MaybeEnqueueOnView is the page-visit trigger: refresh only when the snapshot
// is missing or older than the view TTL, so opening a card that already has
// fresh data costs nothing.
func (manager *Manager) MaybeEnqueueOnView(installationID int64, owner, repo string, number int32, fetchedAt time.Time, hasFetched bool) {
	if !manager.Enabled() {
		return
	}
	if hasFetched && manager.now().Sub(fetchedAt) < manager.viewTTL {
		return
	}
	manager.Enqueue(installationID, owner, repo, number)
}

func (manager *Manager) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case addr := <-manager.queue:
			// A rate-limited installation waits outside the worker pool. Keep
			// the address active while a timer owns it, so events continue to
			// coalesce without letting one tenant occupy every global worker.
			if pause := manager.rateLimitPause(addr.InstallationID); pause > 0 {
				manager.deferActive(ctx, addr, pause)
				continue
			}
			manager.mu.Lock()
			manager.inFlight[addr] = true
			manager.mu.Unlock()
			manager.process(ctx, addr)
			manager.finish(addr)
		}
	}
}

func (manager *Manager) process(ctx context.Context, addr address) {
	// Per-request jitter smooths bursts. Installation-scoped rate-limit waits
	// are handled before this point so they never consume a worker slot.
	if j := manager.jitter(); j > 0 {
		if !sleepCtx(ctx, j) {
			return
		}
	}

	// CHE-374 review round 4, item 2: check row eligibility BEFORE the outbound
	// GitHub API call. ListGitHubPRRowsByAddress already excludes rows whose
	// workspace has explicitly turned GitHub off (github_enabled=false); if
	// every fan-out workspace sharing this address is disabled, there is
	// nothing to write, so the authenticated fetch — which every trigger path
	// (pull_request webhook, CI-event webhook, page-view, TTL sweep) funnels
	// into this one pipeline — must never be justified in the first place. A
	// workspace that re-enables GitHub between this check and the fetch simply
	// waits for the next trigger or the TTL sweep, same as any other missed
	// refresh window.
	preRows, err := manager.listRows(ctx, db.ListGitHubPRRowsByAddressParams{
		InstallationID: addr.InstallationID,
		RepoOwner:      addr.Owner,
		RepoName:       addr.Repo,
		PrNumber:       addr.Number,
	})
	if err != nil {
		slog.Warn("ghsnapshot: eligibility check failed", "err", err.Error())
		return
	}
	if len(preRows) == 0 {
		// No enabled workspace currently mirrors this address — either no row
		// exists at all, or every fan-out workspace has GitHub disabled. Either
		// way, skip the fetch: master-off must have zero footprint, including
		// zero outbound calls.
		return
	}

	snap, err := manager.fetch(ctx, manager.client, addr.InstallationID, addr.Owner, addr.Repo, addr.Number)
	if err != nil {
		var rl *RateLimitError
		if asRateLimit(err, &rl) {
			manager.extendRateLimit(addr.InstallationID, rl.RetryAfter)
			// Do not create an unbounded retry loop for a persistently limited
			// installation. The bounded TTL sweep (open/draft PRs only) or the
			// next webhook/view event hands the address back after Retry-After.
			return
		}
		// Transient/GitHub failure: keep the last-known snapshot (the row is
		// untouched, so the card shows stale data, never wrong data). No secret
		// is ever logged. The next trigger or the TTL sweep retries.
		slog.Warn("ghsnapshot: fetch failed", "owner", addr.Owner, "repo", addr.Repo, "number", addr.Number, "err", err.Error())
		return
	}

	// CHE-374 review round 4, item 2: re-select rows for the write, rather than
	// reusing preRows. This closes the flip-race window between the eligibility
	// check above and this write: a workspace whose github_enabled flipped to
	// false while the fetch was in flight is excluded here even though it
	// justified (or shared the address with a row that justified) the fetch
	// above.
	rows, err := manager.listRows(ctx, db.ListGitHubPRRowsByAddressParams{
		InstallationID: addr.InstallationID,
		RepoOwner:      addr.Owner,
		RepoName:       addr.Repo,
		PrNumber:       addr.Number,
	})
	if err != nil {
		slog.Warn("ghsnapshot: list rows failed", "err", err.Error())
		return
	}

	anyApplied := false
	anyOpenApplied := false
	for _, row := range rows {
		applied, err := manager.applySnapshot(ctx, row.ID, snap)
		if err != nil {
			slog.Warn("ghsnapshot: apply snapshot failed", "err", err.Error())
			continue
		}
		if !applied {
			continue
		}
		anyApplied = true
		if row.State == "open" || row.State == "draft" {
			anyOpenApplied = true
		}
		if manager.onApplied != nil {
			manager.onApplied(ctx, row.ID)
		}
	}

	// Chase only while the snapshot is undecided AND a write proved that an
	// open PR row still exists on this head. If the head advanced, its webhook
	// owns the next refresh; if the row was removed, nothing remains to refresh.
	// Replica lag can instead make rows empty; that omission is recovered by the
	// bounded TTL sweep for an open/draft PR or by a later page-view refresh, so
	// it must not start a chase without having observed current row state. A
	// logged write failure is likewise left to those later triggers.
	if anyApplied && anyOpenApplied && !snap.Decided() {
		manager.scheduleChase(addr)
	} else {
		manager.mu.Lock()
		delete(manager.attempts, addr)
		manager.mu.Unlock()
	}
}

// listGitHubPRRowsByAddress is eventual-consistency safe because the returned
// IDs are only candidates for primary writes. Each write checks the current
// head SHA on the primary, so replica lag can omit an update but cannot apply a
// snapshot for an old head. The TTL sweep and page-view trigger recover omitted
// rows.
func (manager *Manager) listGitHubPRRowsByAddress(ctx context.Context, params db.ListGitHubPRRowsByAddressParams) ([]db.ListGitHubPRRowsByAddressRow, error) {
	return dbreader.Read(
		ctx,
		manager.readSelector,
		dbreader.BusinessGitHubPRRefresh,
		dbreader.EventualConsistency,
		func(ctx context.Context, queries *db.Queries) ([]db.ListGitHubPRRowsByAddressRow, error) {
			return queries.ListGitHubPRRowsByAddress(ctx, params)
		},
	)
}

func (manager *Manager) rateLimitPause(installationID int64) time.Duration {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	until, ok := manager.rateUntil[installationID]
	if !ok {
		return 0
	}
	pause := until.Sub(manager.now())
	if pause <= 0 {
		delete(manager.rateUntil, installationID)
		return 0
	}
	return pause
}

func (manager *Manager) extendRateLimit(installationID int64, retryAfter time.Duration) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	until := manager.now().Add(retryAfter)
	if until.After(manager.rateUntil[installationID]) {
		manager.rateUntil[installationID] = until
	}
}

// deferActive returns a rate-limited address to the queue after delay without
// holding a worker. The active marker remains set while the timer owns the
// address, so duplicate triggers still coalesce into the scheduled fetch.
func (manager *Manager) deferActive(ctx context.Context, addr address, delay time.Duration) {
	time.AfterFunc(delay, func() {
		if ctx.Err() != nil {
			manager.release(addr)
			return
		}
		select {
		case manager.queue <- addr:
		default:
			manager.release(addr)
			slog.Warn("ghsnapshot: refresh queue full, dropping rate-limited enqueue")
		}
	})
}

func (manager *Manager) release(addr address) {
	manager.mu.Lock()
	delete(manager.active, addr)
	delete(manager.inFlight, addr)
	delete(manager.trailing, addr)
	manager.mu.Unlock()
}

// finish releases an address after a worker completes it, or turns the single
// coalesced trailing edge into the next queued refresh without ever allowing
// two workers to own the same address concurrently.
func (manager *Manager) finish(addr address) {
	manager.mu.Lock()
	if !manager.trailing[addr] {
		delete(manager.active, addr)
		delete(manager.inFlight, addr)
		manager.mu.Unlock()
		return
	}
	delete(manager.trailing, addr)
	delete(manager.inFlight, addr)
	manager.mu.Unlock()

	select {
	case manager.queue <- addr:
	default:
		// The worker just consumed one slot, so saturation is unlikely, but keep
		// the webhook path non-blocking and let the TTL sweep recover.
		manager.mu.Lock()
		delete(manager.active, addr)
		delete(manager.inFlight, addr)
		delete(manager.trailing, addr)
		manager.mu.Unlock()
		slog.Warn("ghsnapshot: refresh queue full, dropping trailing enqueue")
	}
}

// applySnapshot performs the head-SHA-guarded atomic batch replace for one PR
// row: guarded UPDATE of the snapshot columns, then a full DELETE + INSERT of
// the per-check rows — all in one transaction. Returns applied=false (and
// writes nothing) when the row's head has advanced past the snapshot's head
// (acceptance criterion 1).
func (manager *Manager) applySnapshot(ctx context.Context, prID pgtype.UUID, snap *PRSnapshot) (bool, error) {
	tx, err := manager.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := manager.queries.WithTx(tx)

	prev, err := q.GetGitHubPRSnapshotState(ctx, prID)
	if err != nil {
		return false, err
	}
	rollup := pgtype.Text{}
	if snap.HasChecks {
		rollup = textOrNull(snap.RollupState)
	}
	n, err := q.UpdateGitHubPRSnapshot(ctx, db.UpdateGitHubPRSnapshotParams{
		ApiMergeable:        textOrNull(snap.Mergeable),
		ApiMergeStateStatus: textOrNull(snap.MergeStateStatus),
		ChecksRollupState:   rollup,
		HeadSha:             snap.HeadSHA,
		BaseRef:             textOrNull(snap.BaseRef),
		FetchedAt:           tsFromTime(manager.now()),
		PrID:                prID,
	})
	if err != nil {
		return false, err
	}
	if n == 0 {
		// Head advanced — discard the entire response, including the per-check
		// rows. Nothing is written.
		return false, nil
	}
	if err := q.DeleteGitHubPRCheckRuns(ctx, prID); err != nil {
		return false, err
	}
	for i, c := range snap.Contexts {
		if err := q.InsertGitHubPRCheckRun(ctx, db.InsertGitHubPRCheckRunParams{
			PrID:            prID,
			HeadSha:         snap.HeadSHA,
			Ordinal:         int32(i),
			Name:            c.Name,
			Status:          c.Status,
			Conclusion:      textOrNull(c.Conclusion),
			DetailsUrl:      textOrNull(c.DetailsURL),
			IsStatusContext: c.IsStatusContext,
		}); err != nil {
			return false, err
		}
	}
	for _, event := range snapshotPREvents(prev, snap, manager.now()) {
		event.PRID = prID
		if _, err := CapturePREvent(ctx, q, event); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// scheduleChase re-enqueues the address after the current backoff step. Bounded
// by maxChaseAttempts so an endlessly-pending CI or a wedged mergeability
// verdict can never spin forever (acceptance criterion 7).
func (manager *Manager) scheduleChase(addr address) {
	manager.mu.Lock()
	attempt := manager.attempts[addr]
	if attempt >= maxChaseAttempts {
		delete(manager.attempts, addr)
		manager.mu.Unlock()
		return
	}
	manager.attempts[addr] = attempt + 1
	idx := attempt
	if idx >= len(manager.chaseBackoff) {
		idx = len(manager.chaseBackoff) - 1
	}
	delay := manager.chaseBackoff[idx]
	manager.mu.Unlock()
	manager.scheduleRetry(addr, delay)
}

// scheduleRetry re-enqueues the address after delay, unless the manager is
// shutting down.
func (manager *Manager) scheduleRetry(addr address, delay time.Duration) {
	ctx := manager.ctx
	time.AfterFunc(delay, func() {
		if ctx != nil && ctx.Err() != nil {
			return
		}
		manager.Enqueue(addr.InstallationID, addr.Owner, addr.Repo, addr.Number)
	})
}

func (manager *Manager) sweepLoop(ctx context.Context) {
	if !sleepCtx(ctx, manager.catchUp) {
		return
	}
	manager.sweepOnce(ctx)
	ticker := time.NewTicker(manager.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.sweepOnce(ctx)
		}
	}
}

// sweepOnce enqueues a refresh for every open PR whose snapshot is both stale
// and undecided, then re-checks decided ones older than recheckTTL. Each part
// is bounded by sweepMaxRows. This is the safety net for a PR whose base
// branch changes without a pull_request webhook, and for any webhook that was
// dropped during a deploy.
func (manager *Manager) sweepOnce(ctx context.Context) {
	manager.sweepUndecided(ctx)
	manager.sweepRecheck(ctx)
	manager.sweepConflictWatched(ctx)
}

func (manager *Manager) sweepUndecided(ctx context.Context) {
	manager.mu.Lock()
	after := manager.sweepAfter
	manager.mu.Unlock()

	rows, err := manager.queries.ListStaleUndecidedGitHubPRs(ctx, db.ListStaleUndecidedGitHubPRsParams{
		OlderThan:           tsFromTime(manager.now().Add(-manager.sweepTTL)),
		AfterInstallationID: after.InstallationID,
		AfterRepoOwner:      after.Owner,
		AfterRepoName:       after.Repo,
		AfterPrNumber:       after.Number,
		MaxRows:             manager.sweepMaxRows,
	})
	if err != nil {
		slog.Warn("ghsnapshot: sweep query failed", "err", err.Error())
		return
	}
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		manager.mu.Lock()
		manager.sweepAfter = address{
			InstallationID: last.InstallationID,
			Owner:          last.RepoOwner,
			Repo:           last.RepoName,
			Number:         last.PrNumber,
		}
		manager.mu.Unlock()
	}
	for _, r := range rows {
		manager.Enqueue(r.InstallationID, r.RepoOwner, r.RepoName, r.PrNumber)
	}
}

// sweepRecheck re-checks the least recently touched snapshots, at most a
// quarter of a batch per installation, and logs the oldest snapshot age so a
// stalled re-check shows. A failed fetch still counts as a pick, so it never
// pins a batch slot.
// ponytail: an installation re-checks at most sweepMaxRows/4 PRs per sweep
// interval (300 an hour at the defaults); with more open PRs than that it
// falls behind the TTL. Raise sweepMaxRows when the logged age keeps climbing.
func (manager *Manager) sweepRecheck(ctx context.Context) {
	now := manager.now()
	rows, err := manager.queries.ClaimGitHubPRsDueForRecheck(ctx, db.ClaimGitHubPRsDueForRecheckParams{
		OlderThan:       tsFromTime(now.Add(-manager.recheckTTL)),
		PerInstallation: int64(max(1, manager.sweepMaxRows/4)),
		MaxRows:         manager.sweepMaxRows,
		PickedAt:        tsFromTime(now),
	})
	if err != nil {
		slog.Warn("ghsnapshot: re-check query failed", "err", err.Error())
		return
	}
	if len(rows) == 0 {
		return
	}
	oldest := rows[0].OldestFetchedAt.Time
	for _, r := range rows {
		if r.OldestFetchedAt.Time.Before(oldest) {
			oldest = r.OldestFetchedAt.Time
		}
		manager.Enqueue(r.InstallationID, r.RepoOwner, r.RepoName, r.PrNumber)
	}
	slog.Info("ghsnapshot: re-checking decided snapshots", "count", len(rows), "oldest_snapshot_age", now.Sub(oldest).Round(time.Second).String())
}

// sweepConflictWatched keeps the PRs a "wake on merge conflict" rule watches
// fresh. A decided snapshot leaves the undecided sweep, yet a direct push to
// the base branch can make it conflicting with no event for the PR itself.
func (manager *Manager) sweepConflictWatched(ctx context.Context) {
	manager.mu.Lock()
	after := manager.watchAfter
	manager.mu.Unlock()

	rows, err := manager.queries.ListStaleConflictWatchedGitHubPRs(ctx, db.ListStaleConflictWatchedGitHubPRsParams{
		OlderThan:           tsFromTime(manager.now().Add(-manager.watchTTL)),
		AfterInstallationID: after.InstallationID,
		AfterRepoOwner:      after.Owner,
		AfterRepoName:       after.Repo,
		AfterPrNumber:       after.Number,
		MaxRows:             manager.sweepMaxRows,
	})
	if err != nil {
		slog.Warn("ghsnapshot: conflict-watch sweep query failed", "err", err.Error())
		return
	}
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		manager.mu.Lock()
		manager.watchAfter = address{InstallationID: last.InstallationID, Owner: last.RepoOwner, Repo: last.RepoName, Number: last.PrNumber}
		manager.mu.Unlock()
	}
	for _, r := range rows {
		manager.Enqueue(r.InstallationID, r.RepoOwner, r.RepoName, r.PrNumber)
	}
}

// RefreshConflictWatchers schedules refreshes for the open PRs in a repository
// that a "wake on merge conflict" rule watches, after the PR numbered merged
// merged there. That can move their base, which sends no event for them.
func (manager *Manager) RefreshConflictWatchers(ctx context.Context, installationID int64, owner, repo string, merged int32) {
	if !manager.Enabled() {
		return
	}
	numbers, err := manager.queries.ListConflictWatchedGitHubPRNumbersInRepo(ctx, db.ListConflictWatchedGitHubPRNumbersInRepoParams{
		InstallationID: installationID,
		RepoOwner:      owner,
		RepoName:       repo,
		PrNumber:       merged,
	})
	if err != nil {
		slog.Warn("ghsnapshot: conflict-watch lookup failed", "err", err.Error())
		return
	}
	for _, number := range numbers {
		addr := address{InstallationID: installationID, Owner: owner, Repo: repo, Number: number}
		for _, delay := range manager.mergeDelays {
			manager.scheduleRetry(addr, delay)
		}
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func asRateLimit(err error, target **RateLimitError) bool {
	if rl, ok := err.(*RateLimitError); ok {
		*target = rl
		return true
	}
	return false
}

func textOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func tsFromTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// sleepCtx sleeps for d or until ctx is cancelled; returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
