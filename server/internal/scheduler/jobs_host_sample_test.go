package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type hostFixture struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	daemon  string
}

// newHostFixture isolates one test by a unique daemon_id; host tables have no
// workspace column.
func newHostFixture(t *testing.T) *hostFixture {
	t.Helper()
	pool := integrationPool(t)
	f := &hostFixture{pool: pool, queries: db.New(pool), daemon: "host-test-" + uuid.NewString()[:8]}
	t.Cleanup(func() {
		bg := context.Background()
		pool.Exec(bg, `DELETE FROM host_sample WHERE daemon_id = $1`, f.daemon)
		pool.Exec(bg, `DELETE FROM host_sample_hourly WHERE daemon_id = $1`, f.daemon)
	})
	return f
}

func (f *hostFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *hostFixture) cpuRAM(t *testing.T, at time.Time, cpu, ram float32) {
	t.Helper()
	f.exec(t, `INSERT INTO host_sample (daemon_id, sampled_at, cpu_percent, ram_percent) VALUES ($1, $2, $3, $4)`, f.daemon, at, cpu, ram)
}

func (f *hostFixture) disk(t *testing.T, at time.Time, mount string, pct float32) {
	t.Helper()
	f.exec(t, `INSERT INTO host_sample (daemon_id, sampled_at, mount_path, disk_percent) VALUES ($1, $2, $3, $4)`, f.daemon, at, mount, pct)
}

func (f *hostFixture) rollup(t *testing.T, plan time.Time) {
	t.Helper()
	if _, err := makeHostRollupHandler(f.queries)(context.Background(), HandlerInput{PlanTime: plan}); err != nil {
		t.Fatalf("rollup: %v", err)
	}
}

type hourlyRow struct {
	cpuMax, cpuAvg, ramMax, diskMax *float32
	samples, expected               int
}

func (f *hostFixture) hourly(t *testing.T, hour time.Time, mount string) *hourlyRow {
	t.Helper()
	var r hourlyRow
	err := f.pool.QueryRow(context.Background(),
		`SELECT cpu_max, cpu_avg, ram_max, disk_max, sample_count, expected_count FROM host_sample_hourly
		 WHERE daemon_id = $1 AND bucket_ts = $2 AND mount_path = $3`, f.daemon, hour, mount).
		Scan(&r.cpuMax, &r.cpuAvg, &r.ramMax, &r.diskMax, &r.samples, &r.expected)
	if err != nil {
		return nil
	}
	return &r
}

// An hour folds into max and avg per (daemon, mount); a 1-minute spike survives
// in max; a half-covered hour reads 30/60; an hour with no raw row is a gap.
func TestHostRollupFoldsHour(t *testing.T) {
	f := newHostFixture(t)
	now := time.Now().UTC().Truncate(time.Hour)
	h := now.Add(-3 * time.Hour)
	for i := 0; i < 30; i++ {
		cpu := float32(10)
		if i == 7 {
			cpu = 95 // one-minute spike
		}
		f.cpuRAM(t, h.Add(time.Duration(i)*time.Minute), cpu, 40)
		f.disk(t, h.Add(time.Duration(i)*time.Minute), "/", 85)
		f.disk(t, h.Add(time.Duration(i)*time.Minute), "/mnt/ssd2", 50)
	}
	f.rollup(t, now.Add(2*time.Minute))

	cr := f.hourly(t, h, "")
	if cr == nil || cr.cpuMax == nil || *cr.cpuMax != 95 || *cr.ramMax != 40 || cr.diskMax != nil {
		t.Fatalf("cpu/ram row = %+v", cr)
	}
	if want := float32((29*10 + 95) / 30.0); *cr.cpuAvg < want-0.01 || *cr.cpuAvg > want+0.01 {
		t.Errorf("cpu avg = %v, want %v", *cr.cpuAvg, want)
	}
	if cr.samples != 30 || cr.expected != 60 {
		t.Errorf("coverage = %d/%d, want 30/60", cr.samples, cr.expected)
	}
	if d := f.hourly(t, h, "/"); d == nil || d.diskMax == nil || *d.diskMax != 85 || d.cpuMax != nil {
		t.Errorf("/ row = %+v", d)
	}
	if d := f.hourly(t, h, "/mnt/ssd2"); d == nil || *d.diskMax != 50 {
		t.Errorf("/mnt/ssd2 row = %+v", d)
	}
	if g := f.hourly(t, h.Add(time.Hour), ""); g != nil {
		t.Errorf("hour with no raw rows was written: %+v", g)
	}
}

// Only complete hours roll up; a rerun with a late sample updates the same row.
func TestHostRollupIdempotentAndCompleteHoursOnly(t *testing.T) {
	f := newHostFixture(t)
	now := time.Now().UTC().Truncate(time.Hour)
	h := now.Add(-2 * time.Hour)
	f.cpuRAM(t, h, 20, 30)
	f.cpuRAM(t, now.Add(time.Minute), 99, 99) // current, incomplete hour
	f.rollup(t, now.Add(2*time.Minute))
	f.rollup(t, now.Add(2*time.Minute))
	if r := f.hourly(t, h, ""); r == nil || r.samples != 1 {
		t.Fatalf("after rerun = %+v, want one sample", r)
	}
	if r := f.hourly(t, now, ""); r != nil {
		t.Errorf("incomplete hour rolled up: %+v", r)
	}
	f.cpuRAM(t, h.Add(time.Minute), 60, 30) // late sample
	f.rollup(t, now.Add(2*time.Minute))
	if r := f.hourly(t, h, ""); r == nil || r.samples != 2 || *r.cpuMax != 60 {
		t.Errorf("after late sample = %+v, want 2 samples, max 60", r)
	}
}

// Retention: raw 7 d, hourly rollups 90 d. Rows inside the window stay.
func TestHostRetentionDeletesPastCutoffs(t *testing.T) {
	f := newHostFixture(t)
	now := time.Now().UTC().Truncate(time.Minute)
	for _, age := range []time.Duration{6 * 24 * time.Hour, 8 * 24 * time.Hour} {
		f.cpuRAM(t, now.Add(-age), 10, 10)
	}
	for _, age := range []time.Duration{89 * 24 * time.Hour, 91 * 24 * time.Hour} {
		f.exec(t, `INSERT INTO host_sample_hourly (daemon_id, bucket_ts, cpu_max, cpu_avg, ram_max, ram_avg, sample_count)
			VALUES ($1, $2, 1, 1, 1, 1, 60)`, f.daemon, now.Truncate(time.Hour).Add(-age))
	}
	if _, err := makeOpsRetentionHandler(f.queries, func() time.Time { return now })(context.Background(), HandlerInput{}); err != nil {
		t.Fatalf("retention: %v", err)
	}
	count := func(table string) int {
		var n int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE daemon_id = $1`, f.daemon).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("host_sample"); n != 1 {
		t.Errorf("raw rows left = %d, want 1", n)
	}
	if n := count("host_sample_hourly"); n != 1 {
		t.Errorf("hourly rows left = %d, want 1", n)
	}
}

func TestHostRollupJobConfiguration(t *testing.T) {
	j := HostSampleRollupJob(nil)
	if j.Name != JobNameHostSampleRollup || j.Cadence != time.Hour || j.Handler == nil || j.CatchUpMode != CatchUpLatestOnly {
		t.Errorf("job misconfigured: %+v", j)
	}
}
