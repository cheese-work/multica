package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type opsFixture struct {
	pool        *pgxpool.Pool
	queries     *db.Queries
	workspaceID pgtype.UUID
}

func (f *opsFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *opsFixture) runtime(t *testing.T, daemonID *string, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, `INSERT INTO agent_runtime (id, workspace_id, daemon_id, name, runtime_mode, provider, status)
		VALUES ($1, $2, $3, $4, 'local', 'claude', $5)`, id, f.workspaceID, daemonID, "rt-"+id.String()[:8], status)
	return id
}

func (f *opsFixture) agent(t *testing.T, runtimeID *uuid.UUID, archived bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, `INSERT INTO agent (id, workspace_id, name, runtime_mode, runtime_id, archived_at)
		VALUES ($1, $2, $3, 'local', $4, CASE WHEN $5 THEN now() ELSE NULL END)`,
		id, f.workspaceID, "ag-"+id.String()[:8], runtimeID, archived)
	return id
}

func (f *opsFixture) task(t *testing.T, agentID uuid.UUID, runtimeID *uuid.UUID, status string, createdAgo time.Duration) {
	t.Helper()
	f.exec(t, `INSERT INTO agent_task_queue (agent_id, runtime_id, status, created_at, fire_at)
		VALUES ($1, $2, $3, now() - $4::interval, CASE WHEN $3 = 'deferred' THEN now() + interval '1 hour' END)`,
		agentID, runtimeID, status, createdAgo.String())
}

// newOpsFixture builds a fresh workspace: an online machine with one busy and
// one queued-only agent, an offline machine with one offline agent that still
// holds a running task, an unbound agent, a cloud runtime with one available
// agent, one archived agent, and one deferred task.
func newOpsFixture(t *testing.T) *opsFixture {
	t.Helper()
	pool := integrationPool(t)
	ctx := context.Background()
	f := &opsFixture{pool: pool, queries: db.New(pool)}
	suffix := uuid.NewString()[:8]
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ($1, $2, '', 'OPS') RETURNING id`,
		"Ops Test "+suffix, "ops-test-"+suffix).Scan(&f.workspaceID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM agent_task_queue WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1)`, f.workspaceID)
		pool.Exec(bg, `DELETE FROM agent WHERE workspace_id = $1`, f.workspaceID)
		pool.Exec(bg, `DELETE FROM agent_runtime WHERE workspace_id = $1`, f.workspaceID)
		pool.Exec(bg, `DELETE FROM ops_snapshot WHERE workspace_id = $1`, f.workspaceID)
		pool.Exec(bg, `DELETE FROM ops_snapshot_5m WHERE workspace_id = $1`, f.workspaceID)
		pool.Exec(bg, `DELETE FROM agent_task_event WHERE workspace_id = $1`, f.workspaceID)
		pool.Exec(bg, `DELETE FROM workspace WHERE id = $1`, f.workspaceID)
	})

	d1, d2 := "host-online-"+suffix, "host-offline-"+suffix
	online := f.runtime(t, &d1, "online")
	offline := f.runtime(t, &d2, "offline")
	cloud := f.runtime(t, nil, "online")

	busy := f.agent(t, &online, false)
	queuedOnly := f.agent(t, &online, false)
	orphan := f.agent(t, &offline, false)
	f.agent(t, nil, false)
	f.agent(t, &cloud, false)
	f.agent(t, &online, true)

	f.task(t, busy, &online, "running", 20*time.Minute)
	f.task(t, queuedOnly, &online, "queued", 10*time.Minute)
	f.task(t, orphan, &offline, "running", 30*time.Minute)
	f.task(t, queuedOnly, &online, "deferred", time.Hour)
	f.task(t, busy, &online, "completed", time.Hour)
	return f
}

func (f *opsFixture) sample(t *testing.T, planTime time.Time) int64 {
	t.Helper()
	res, err := makeOpsSamplerHandler(f.queries)(context.Background(), HandlerInput{PlanTime: planTime})
	if err != nil {
		t.Fatalf("sampler: %v", err)
	}
	return res.RowsAffected
}

type opsSums struct{ unbound, offline, busy, waitingDir, available, queued, dispatched, waitingTasks, running int }

func (f *opsFixture) sums(t *testing.T, table, suffix string, at time.Time) (opsSums, int) {
	t.Helper()
	var s opsSums
	var keys int
	q := `SELECT count(*), COALESCE(sum(agents_unbound` + suffix + `),0), COALESCE(sum(agents_offline` + suffix + `),0),
		COALESCE(sum(agents_busy` + suffix + `),0), COALESCE(sum(agents_waiting_dir` + suffix + `),0), COALESCE(sum(agents_available` + suffix + `),0),
		COALESCE(sum(queued` + suffix + `),0), COALESCE(sum(dispatched` + suffix + `),0), COALESCE(sum(waiting_dir` + suffix + `),0), COALESCE(sum(running` + suffix + `),0)
		FROM ` + table + ` WHERE workspace_id = $1 AND bucket_ts = $2`
	if err := f.pool.QueryRow(context.Background(), q, f.workspaceID, at).Scan(
		&keys, &s.unbound, &s.offline, &s.busy, &s.waitingDir, &s.available, &s.queued, &s.dispatched, &s.waitingTasks, &s.running); err != nil {
		t.Fatalf("sums: %v", err)
	}
	return s, keys
}

var wantOpsSums = opsSums{unbound: 1, offline: 1, busy: 1, waitingDir: 0, available: 2, queued: 1, dispatched: 0, waitingTasks: 0, running: 2}

// Per tick, the five agent-state counts summed over machine keys equal the
// non-archived agent count, and each task count equals the workspace count of
// tasks in that status. A deferred task is in no count.
func TestOpsSamplerCountConservation(t *testing.T) {
	f := newOpsFixture(t)
	tick := time.Now().UTC().Truncate(30 * time.Second)
	f.sample(t, tick)

	got, keys := f.sums(t, "ops_snapshot", "", tick)
	if keys != 4 {
		t.Fatalf("machine keys = %d, want 4 (online, offline, __cloud__, __unbound__)", keys)
	}
	if got != wantOpsSums {
		t.Fatalf("sums = %+v, want %+v", got, wantOpsSums)
	}

	// The offline machine still reports its offline agent and its orphaned running task.
	var offline, running int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT agents_offline, running FROM ops_snapshot WHERE workspace_id = $1 AND daemon_id LIKE 'host-offline-%'`,
		f.workspaceID).Scan(&offline, &running); err != nil {
		t.Fatal(err)
	}
	if offline != 1 || running != 1 {
		t.Fatalf("offline machine = offline %d running %d, want 1 and 1", offline, running)
	}

	// Oldest unstarted age is the queued task's age (about 10 min) on the online machine.
	var oldest *int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT oldest_unstarted_s FROM ops_snapshot WHERE workspace_id = $1 AND daemon_id LIKE 'host-online-%'`,
		f.workspaceID).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if oldest == nil || *oldest < 560 || *oldest > 660 {
		t.Fatalf("oldest_unstarted_s = %v, want 560..660 (task is 10 min old, tick up to 30 s earlier)", oldest)
	}
}

// Two server instances that sample the same tick write the rows once.
func TestOpsSamplerDropsDuplicateTick(t *testing.T) {
	f := newOpsFixture(t)
	tick := time.Now().UTC().Truncate(30 * time.Second)
	if first := f.sample(t, tick); first != 4 {
		t.Fatalf("first sample rows = %d, want 4", first)
	}
	if second := f.sample(t, tick); second != 0 {
		t.Fatalf("second sample rows = %d, want 0", second)
	}
	if _, keys := f.sums(t, "ops_snapshot", "", tick); keys != 4 {
		t.Fatalf("rows after duplicate = %d, want 4", keys)
	}
}

func (f *opsFixture) rollup(t *testing.T, planTime time.Time) {
	t.Helper()
	if _, err := makeOpsRollupHandler(f.queries)(context.Background(), HandlerInput{PlanTime: planTime}); err != nil {
		t.Fatalf("rollup: %v", err)
	}
}

// The rollup `last` columns keep the conservation sums, `max` keeps a spike
// that lasted one 30 s sample, and a half-covered interval reads 5/10 after
// the raw rows are gone.
func TestOpsRollupKeepsSpikeLastAndPartialCoverage(t *testing.T) {
	f := newOpsFixture(t)
	interval := time.Now().UTC().Add(-3 * time.Hour).Truncate(5 * time.Minute)
	// Five ticks in the interval's first 2.5 min; the last five are missing.
	for i := 0; i < 5; i++ {
		f.sample(t, interval.Add(time.Duration(i)*30*time.Second))
	}
	// One 30 s spike of 9 queued tasks on the online machine, tick 2.
	f.exec(t, `UPDATE ops_snapshot SET queued = 9 WHERE workspace_id = $1 AND bucket_ts = $2 AND daemon_id LIKE 'host-online-%'`,
		f.workspaceID, interval.Add(60*time.Second))

	f.rollup(t, interval.Add(10*time.Minute))
	f.rollup(t, interval.Add(15*time.Minute)) // idempotent rerun

	got, _ := f.sums(t, "ops_snapshot_5m", "_last", interval)
	if got != wantOpsSums {
		t.Fatalf("rollup last sums = %+v, want %+v", got, wantOpsSums)
	}
	f.exec(t, `DELETE FROM ops_snapshot WHERE workspace_id = $1`, f.workspaceID)
	var maxQueued, lastQueued, samples, expected int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT queued_max, queued_last, sample_count, expected_count FROM ops_snapshot_5m
		 WHERE workspace_id = $1 AND bucket_ts = $2 AND daemon_id LIKE 'host-online-%'`,
		f.workspaceID, interval).Scan(&maxQueued, &lastQueued, &samples, &expected); err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	if maxQueued != 9 || lastQueued != 1 || samples != 5 || expected != 10 {
		t.Fatalf("rollup = max %d last %d samples %d/%d, want max 9 last 1 samples 5/10", maxQueued, lastQueued, samples, expected)
	}

	// A rerun after the raw rows were deleted must not erase the coverage row.
	f.rollup(t, interval.Add(20*time.Minute))
	if err := f.pool.QueryRow(context.Background(),
		`SELECT sample_count FROM ops_snapshot_5m WHERE workspace_id = $1 AND bucket_ts = $2 AND daemon_id LIKE 'host-online-%'`,
		f.workspaceID, interval).Scan(&samples); err != nil || samples != 5 {
		t.Fatalf("after rerun samples = %d err %v, want 5", samples, err)
	}
}

// Retention: raw 7 d, rollups and events 35 d, and the sampler's own lease rows 7 d.
func TestOpsRetentionDeletesPastCutoffs(t *testing.T) {
	f := newOpsFixture(t)
	now := time.Now().UTC().Truncate(time.Minute)
	for _, age := range []time.Duration{6 * 24 * time.Hour, 8 * 24 * time.Hour} {
		f.exec(t, `INSERT INTO ops_snapshot (workspace_id, daemon_id, bucket_ts) VALUES ($1, 'd', $2)`, f.workspaceID, now.Add(-age))
	}
	for _, age := range []time.Duration{34 * 24 * time.Hour, 36 * 24 * time.Hour} {
		f.exec(t, `INSERT INTO ops_snapshot_5m (workspace_id, daemon_id, bucket_ts, sample_count) VALUES ($1, 'd', $2, 10)`, f.workspaceID, now.Add(-age))
		f.exec(t, `INSERT INTO agent_task_event (workspace_id, task_id, kind, at) VALUES ($1, $2, 'stale_reclaim', $3)`, f.workspaceID, uuid.New(), now.Add(-age))
	}
	lease := now.Add(-8 * 24 * time.Hour)
	f.exec(t, `DELETE FROM sys_cron_executions WHERE job_name = $1 AND plan_time = $2`, JobNameOpsSnapshotSampler, lease)
	f.exec(t, `INSERT INTO sys_cron_executions (job_name, plan_time, status) VALUES ($1, $2, 'SUCCESS')`, JobNameOpsSnapshotSampler, lease)

	if _, err := makeOpsRetentionHandler(f.queries, func() time.Time { return now })(context.Background(), HandlerInput{}); err != nil {
		t.Fatalf("retention: %v", err)
	}
	count := func(sql string, args ...any) int {
		var n int
		if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM ops_snapshot WHERE workspace_id = $1`, f.workspaceID); n != 1 {
		t.Errorf("raw rows left = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM ops_snapshot_5m WHERE workspace_id = $1`, f.workspaceID); n != 1 {
		t.Errorf("rollup rows left = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM agent_task_event WHERE workspace_id = $1`, f.workspaceID); n != 1 {
		t.Errorf("event rows left = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM sys_cron_executions WHERE job_name = $1 AND plan_time = $2`, JobNameOpsSnapshotSampler, lease); n != 0 {
		t.Errorf("old sampler lease rows left = %d, want 0", n)
	}
}

// The sampler runs every 30 s; the rollup every 5 min; retention hourly.
func TestOpsJobConfiguration(t *testing.T) {
	cases := []struct {
		job     JobSpec
		name    string
		cadence time.Duration
	}{
		{OpsSnapshotSamplerJob(nil), JobNameOpsSnapshotSampler, 30 * time.Second},
		{OpsSnapshotRollupJob(nil), JobNameOpsSnapshotRollup, 5 * time.Minute},
		{OpsRetentionJob(nil), JobNameOpsRetention, time.Hour},
	}
	for _, c := range cases {
		if c.job.Name != c.name || c.job.Cadence != c.cadence || c.job.Handler == nil || c.job.CatchUpMode != CatchUpLatestOnly {
			t.Errorf("job %q misconfigured: %+v", c.name, c.job)
		}
	}
}
