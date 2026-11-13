package ghsnapshot

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"slices"
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

	// closedReplayTimeout bounds one owed closed-PR replay in the sweep, so a
	// hung GitHub call cannot hold up the other sweeps (CHE-3402).
	closedReplayTimeout = 30 * time.Second

	// CHE-1665: decided snapshots are re-checked once they are this old, and
	// the first sweep runs this long after start, so snapshots that went stale
	// while the server was down drain without waiting a whole interval.
	defaultRecheckTTL = time.Hour
	defaultCatchUp    = time.Minute
	// PRs watched by a "wake on merge conflict" rule stay in the sweep even
	// when decided, re-fetched once their snapshot is older than this.
	defaultWatchTTL = 5 * time.Minute
	// A PR a checks_finished rule waits on is re-fetched after a minute, and
	// the sweep runs every 30s, so a missed webhook costs about a minute.
	defaultChecksWatchTTL      = time.Minute
	defaultChecksWatchInterval = 30 * time.Second
	// One refresh makes a few GitHub calls and a short write; two minutes is
	// far above that and frees a worker stuck on a hung call or lock.
	defaultProcessTimeout = 2 * time.Minute
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
	onClosed     func(ctx context.Context, installationID int64, owner, repo string, number int32) error

	concurrency   int
	viewTTL       time.Duration
	sweepTTL      time.Duration
	sweepInterval time.Duration
	sweepMaxRows  int32
	recheckTTL    time.Duration
	catchUp       time.Duration
	watchTTL      time.Duration
	// checksWatchTTL / checksWatchInterval drive the fast sweep for PRs a
	// "wake when checks finish" rule waits on (CHE-5313).
	checksWatchTTL      time.Duration
	checksWatchInterval time.Duration
	// processTimeout bounds one refresh so a hung fetch or write cannot hold a
	// worker forever (CHE-5313). Zero means unbounded.
	processTimeout time.Duration
	mergeDelays    []time.Duration
	chaseBackoff   []time.Duration
	now            func() time.Time
	jitter         func() time.Duration
	// fetch is the snapshot fetcher, a seam so tests can drive the queue /
	// backoff without a live GitHub. Defaults to FetchPRSnapshot.
	fetch func(ctx context.Context, c *Client, installationID int64, owner, repo string, number int32) (*PRSnapshot, error)
	// listRows is the row-selection seam used both to gate the outbound fetch
	// (CHE-374 review round 4, item 2) and to select rows for the write.
	// Defaults to listGitHubPRRowsByAddress. Tests that only exercise the
	// rate-limit/backoff machinery around process() (and do not wire a DB)
	// stub this directly, the same way they stub fetch.
	listRows func(ctx context.Context, params db.ListGitHubPRRowsByAddressParams) ([]db.ListGitHubPRRowsByAddressRow, error)

	// workspaceFilter, when set, returns the workspaces allowed to receive a
	// snapshot for an installation; rows of other workspaces are never fetched
	// for or written. It runs on the given queries so the write can check
	// inside its transaction.
	workspaceFilter WorkspaceFilter

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
	// The same rotation for the conflict-watch and checks-watch sweeps.
	watchAfter       address
	checksWatchAfter address
	// Secondary rate limits are scoped to the installation whose token incurred
	// them. One customer must never pause every other installation.
	rateUntil map[int64]time.Time

	// allow, when set, vets an installation before any refresh is queued;
	// deniedLogged rate-limits the denial warning per installation.
	allow        func(installationID int64) bool
	deniedLogged map[int64]time.Time

	ctx     context.Context
	started bool
}

// NewManager wires the pipeline. onApplied is called once per PR row whose
// snapshot was actually written (guard passed), so the handler can broadcast a
// realtime PR update. Passing a nil client yields a disabled (no-op) manager.
func NewManager(client *Client, queries *db.Queries, pool TxBeginner, onApplied func(ctx context.Context, prID pgtype.UUID)) *Manager {
	return newManager(client, queries, pool, fetchEnrichedSnapshot, onApplied)
}

// NewManagerWithFetcher wires the normal refresh lifecycle with a supplied
// snapshot fetcher. Production callers should normally use NewManager.
func NewManagerWithFetcher(client *Client, queries *db.Queries, pool TxBeginner, fetch func(context.Context, *Client, int64, string, string, int32) (*PRSnapshot, error), onApplied func(ctx context.Context, prID pgtype.UUID)) *Manager {
	if fetch == nil {
		fetch = fetchEnrichedSnapshot
	}
	return newManager(client, queries, pool, fetch, onApplied)
}

func newManager(client *Client, queries *db.Queries, pool TxBeginner, fetch func(context.Context, *Client, int64, string, string, int32) (*PRSnapshot, error), onApplied func(ctx context.Context, prID pgtype.UUID)) *Manager {
	manager := &Manager{
		client:              client,
		queries:             queries,
		readSelector:        dbreader.NewPrimaryOnly(queries),
		pool:                pool,
		onApplied:           onApplied,
		concurrency:         defaultConcurrency,
		viewTTL:             defaultViewTTL,
		sweepTTL:            defaultSweepTTL,
		sweepInterval:       defaultSweepInterval,
		sweepMaxRows:        defaultSweepMaxRows,
		recheckTTL:          defaultRecheckTTL,
		catchUp:             defaultCatchUp,
		watchTTL:            defaultWatchTTL,
		checksWatchTTL:      defaultChecksWatchTTL,
		checksWatchInterval: defaultChecksWatchInterval,
		processTimeout:      defaultProcessTimeout,
		mergeDelays:         defaultMergeWatchDelays,
		chaseBackoff:        defaultChaseBackoff,
		now:                 time.Now,
		jitter:              func() time.Duration { return time.Duration(rand.Int63n(int64(250 * time.Millisecond))) },
		fetch:               fetch,
		queue:               make(chan address, queueBuffer),
		active:              map[address]bool{},
		inFlight:            map[address]bool{},
		trailing:            map[address]bool{},
		attempts:            map[address]int{},
		rateUntil:           map[int64]time.Time{},
	}
	manager.listRows = manager.listGitHubPRRowsByAddress
	return manager
}

// WorkspaceFilter lists the workspaces allowed to receive a snapshot for an
// installation. Called inside the write transaction with that transaction's
// queries, it must lock what it reads so the answer holds until commit.
type WorkspaceFilter func(ctx context.Context, q *db.Queries, installationID int64) (map[pgtype.UUID]bool, error)

// SetWorkspaceFilter restricts the refresh pipeline to the workspaces the
// filter allows for each installation. Call it before Start. Nil-safe: a
// disabled manager ignores it.
func (manager *Manager) SetWorkspaceFilter(filter WorkspaceFilter) {
	if manager != nil {
		manager.workspaceFilter = filter
	}
}

// selectRows lists the rows of an address and drops those the workspace filter
// denies.
func (manager *Manager) selectRows(ctx context.Context, addr address) ([]db.ListGitHubPRRowsByAddressRow, error) {
	rows, err := manager.listRows(ctx, db.ListGitHubPRRowsByAddressParams{
		InstallationID: addr.InstallationID,
		RepoOwner:      addr.Owner,
		RepoName:       addr.Repo,
		PrNumber:       addr.Number,
	})
	if err != nil || manager.workspaceFilter == nil {
		return rows, err
	}
	allowed, err := manager.workspaceFilter(ctx, manager.queries, addr.InstallationID)
	if err != nil {
		return nil, err
	}
	kept := rows[:0:0]
	for _, row := range rows {
		if allowed[row.WorkspaceID] {
			kept = append(kept, row)
		}
	}
	return kept, nil
}

// SetOnClosed registers the callback for a PR that GitHub reports closed while
// a mirrored row is still open or draft. Call it before Start.
func (manager *Manager) SetOnClosed(onClosed func(ctx context.Context, installationID int64, owner, repo string, number int32) error) {
	manager.onClosed = onClosed
}

// SetReadSelector opts the refresh worker into the API server's configured
// replica routing. It must be called before Start; managers used by tests and
// primary-only deployments retain the safe primary default from NewManager.
func (manager *Manager) SetReadSelector(selector *dbreader.Selector) {
	if selector != nil {
		manager.readSelector = selector
	}
}

// SetInstallationFilter installs the allowlist policy check. Enqueue drops an
// installation the filter rejects, logging it once per sweep interval. Call it
// before Start.
func (manager *Manager) SetInstallationFilter(allow func(installationID int64) bool) {
	if manager != nil {
		manager.allow = allow
	}
}

// Enabled reports whether the pipeline will actually do anything.
func (manager *Manager) Enabled() bool { return manager != nil && manager.client.Enabled() }

// Client returns the shared GitHub App client (nil-safe: check Enabled first).
func (manager *Manager) Client() *Client { return manager.client }

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
	go manager.checksWatchLoop(ctx)
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
	if manager.allow != nil && !manager.allow(installationID) {
		manager.warnDenied(installationID)
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

func (manager *Manager) warnDenied(installationID int64) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now()
	if last, ok := manager.deniedLogged[installationID]; ok && now.Sub(last) < manager.sweepInterval {
		return
	}
	if manager.deniedLogged == nil {
		manager.deniedLogged = map[int64]time.Time{}
	}
	manager.deniedLogged[installationID] = now
	slog.Warn("ghsnapshot: installation denied by allowlist; refresh skipped", "installation_id", installationID)
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
			manager.processBounded(ctx, addr)
			manager.finish(addr)
		}
	}
}

// processBounded runs one refresh under processTimeout, so a hung GitHub call
// or database lock cannot hold a worker forever (CHE-5313).
func (manager *Manager) processBounded(ctx context.Context, addr address) {
	if manager.processTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, manager.processTimeout)
		defer cancel()
	}
	manager.process(ctx, addr)
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
	preRows, err := manager.selectRows(ctx, addr)
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
	rows, err := manager.selectRows(ctx, addr)
	if err != nil {
		slog.Warn("ghsnapshot: list rows failed", "err", err.Error())
		return
	}

	anyApplied := false
	anyOpenApplied := false
	staleOpen := false
	for _, row := range rows {
		staleOpen = staleOpen || (snap.Closed && (row.State == "open" || row.State == "draft"))
		applied, err := manager.applySnapshotGuarded(ctx, row.ID, snap, manager.bindingGuard(addr.InstallationID, row.WorkspaceID))
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

	// The pull_request delivery that closed this PR never landed (CHE-3402).
	// GitHub does not redeliver it on its own, so the row would stay open.
	if staleOpen && manager.onClosed != nil {
		var rl *RateLimitError
		if err := manager.onClosed(ctx, addr.InstallationID, addr.Owner, addr.Repo, addr.Number); errors.As(err, &rl) {
			manager.extendRateLimit(addr.InstallationID, rl.RetryAfter)
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
	return manager.applySnapshotGuarded(ctx, prID, snap, nil)
}

// bindingGuard re-checks, inside the write transaction, that the workspace is
// still allowed. The row was selected before the fetch; the binding may have
// been removed or denied since. Nil when no filter is set.
func (manager *Manager) bindingGuard(installationID int64, workspaceID pgtype.UUID) func(ctx context.Context, q *db.Queries) (bool, error) {
	if manager.workspaceFilter == nil {
		return nil
	}
	return func(ctx context.Context, q *db.Queries) (bool, error) {
		allowed, err := manager.workspaceFilter(ctx, q, installationID)
		return allowed[workspaceID], err
	}
}

// applySnapshotGuarded is applySnapshot with an optional guard run first in the
// transaction; a false guard writes nothing.
func (manager *Manager) applySnapshotGuarded(ctx context.Context, prID pgtype.UUID, snap *PRSnapshot, guard func(ctx context.Context, q *db.Queries) (bool, error)) (bool, error) {
	tx, err := manager.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := manager.queries.WithTx(tx)

	if guard != nil {
		ok, err := guard(ctx, q)
		if err != nil || !ok {
			return false, err
		}
	}

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
	redBefore, err := q.ListGitHubPRRedCheckNames(ctx, db.ListGitHubPRRedCheckNamesParams{PrID: prID, HeadSha: snap.HeadSHA})
	if err != nil {
		return false, err
	}
	if err := q.DeleteGitHubPRCheckRuns(ctx, prID); err != nil {
		return false, err
	}
	for i, c := range snap.Contexts {
		if !isFailing(c) && slices.Contains(redBefore, c.Name) {
			// Red earlier on this same head, rerunning or green now (CHE-3257).
			// Carried through a pending rerun so the mark is still there on green.
			c.RedCause = CauseFlaky
		}
		if err := q.InsertGitHubPRCheckRun(ctx, db.InsertGitHubPRCheckRunParams{
			PrID:            prID,
			HeadSha:         snap.HeadSHA,
			Ordinal:         int32(i),
			Name:            c.Name,
			Status:          c.Status,
			Conclusion:      textOrNull(c.Conclusion),
			DetailsUrl:      textOrNull(c.DetailsURL),
			IsStatusContext: c.IsStatusContext,
			RedCause:        textOrNull(c.RedCause),
			AppID:           pgtype.Int8{Int64: c.AppID, Valid: c.AppID > 0},
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
	manager.sweepClosedReplays(ctx)
}

// sweepClosedReplays hands every closed-PR replay still owed back to the
// callback (CHE-3402). A replay is written down before it starts and removed
// when it finishes, so one that failed after the mirror row committed, or died
// with the process, is retried here: that PR is no longer open, so the refresh
// would not report it again.
func (manager *Manager) sweepClosedReplays(ctx context.Context) {
	if manager.onClosed == nil {
		return
	}
	owed, err := manager.queries.ClaimGitHubPRClosedReplays(ctx, int32(manager.sweepMaxRows))
	if err != nil {
		slog.Warn("ghsnapshot: claim owed closed replays failed", "err", err.Error())
		return
	}
	for _, r := range owed {
		addr := address{InstallationID: r.InstallationID, Owner: r.RepoOwner, Repo: r.RepoName, Number: r.PrNumber}
		manager.replayOwedClosed(ctx, addr)
	}
}

// replayOwedClosed runs one owed replay under the refresh pipeline's outbound
// rules: skipped while its installation is rate limited, skipped (marker kept)
// when no workspace that mirrors the PR has GitHub enabled, bounded by
// closedReplayTimeout, and a rate limit it hits starts the installation cooldown.
func (manager *Manager) replayOwedClosed(ctx context.Context, addr address) {
	if manager.rateLimitPause(addr.InstallationID) > 0 {
		return
	}
	rows, err := manager.listRows(ctx, db.ListGitHubPRRowsByAddressParams{
		InstallationID: addr.InstallationID,
		RepoOwner:      addr.Owner,
		RepoName:       addr.Repo,
		PrNumber:       addr.Number,
	})
	if err != nil {
		slog.Warn("ghsnapshot: owed closed replay eligibility check failed", "err", err.Error())
		return
	}
	if len(rows) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, closedReplayTimeout)
	defer cancel()
	err = manager.onClosed(ctx, addr.InstallationID, addr.Owner, addr.Repo, addr.Number)
	var rl *RateLimitError
	if errors.As(err, &rl) {
		manager.extendRateLimit(addr.InstallationID, rl.RetryAfter)
	}
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

// checksWatchLoop runs the fast sweep for PRs a checks_finished rule waits on.
func (manager *Manager) checksWatchLoop(ctx context.Context) {
	if !sleepCtx(ctx, manager.catchUp) {
		return
	}
	ticker := time.NewTicker(manager.checksWatchInterval)
	defer ticker.Stop()
	for {
		manager.sweepChecksWatched(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepChecksWatched re-fetches open PRs with running checks that an enabled
// "wake when checks finish" rule waits on, once their snapshot is checksWatchTTL
// old. It is the safety net for a check_suite webhook that never arrived or a
// refresh that stalled (CHE-5313): the snapshot catches up on its own and the
// rule fires on its next poll.
func (manager *Manager) sweepChecksWatched(ctx context.Context) {
	manager.mu.Lock()
	after := manager.checksWatchAfter
	manager.mu.Unlock()

	rows, err := manager.queries.ListStaleChecksWatchedGitHubPRs(ctx, db.ListStaleChecksWatchedGitHubPRsParams{
		OlderThan:           tsFromTime(manager.now().Add(-manager.checksWatchTTL)),
		AfterInstallationID: after.InstallationID,
		AfterRepoOwner:      after.Owner,
		AfterRepoName:       after.Repo,
		AfterPrNumber:       after.Number,
		MaxRows:             manager.sweepMaxRows,
	})
	if err != nil {
		slog.Warn("ghsnapshot: checks-watch sweep query failed", "err", err.Error())
		return
	}
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		manager.mu.Lock()
		manager.checksWatchAfter = address{InstallationID: last.InstallationID, Owner: last.RepoOwner, Repo: last.RepoName, Number: last.PrNumber}
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
