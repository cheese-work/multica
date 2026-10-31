package handler

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// GET /api/ops?window=1h|24h|7d|30d — operator view of queue and machine
// health (CHE-2586 L1). Owner/admin only; the router applies the role gate.
//
// One payload feeds the KPI strip and charts 1-4. Chart 1 and 4 read the
// 30 s ops_snapshot rows (5-minute rollups for 30d); chart 1 falls back to a
// task-row replay for the part of the window before the first snapshot.
// Times are RFC3339 UTC on the wire; the client renders them in ICT/viewer tz.

const (
	opsSnapshotIntervalS = 30
	// Queue-wait p95 above this is the alert line on chart 2.
	opsThresholdP95S = 900
)

var opsWindows = map[string]struct {
	span    time.Duration
	bucketS int32
}{
	"1h":  {time.Hour, 60},
	"24h": {24 * time.Hour, 300},
	"7d":  {7 * 24 * time.Hour, 3600},
	"30d": {30 * 24 * time.Hour, 14400},
}

type OpsNowMachine struct {
	Machine          string `json:"machine"`
	Queued           int32  `json:"queued"`
	Dispatched       int32  `json:"dispatched"`
	WaitingDir       int32  `json:"waiting_dir"`
	Running          int32  `json:"running"`
	AgentsUnbound    int32  `json:"agents_unbound"`
	AgentsOffline    int32  `json:"agents_offline"`
	AgentsBusy       int32  `json:"agents_busy"`
	AgentsWaitingDir int32  `json:"agents_waiting_dir"`
	AgentsAvailable  int32  `json:"agents_available"`
	OldestUnstartedS *int32 `json:"oldest_unstarted_s"`
}

type OpsNow struct {
	Machines         []OpsNowMachine `json:"machines"`
	QueueWaitP95S    float64         `json:"queue_wait_p95_s"`
	QueueWaitSamples int32           `json:"queue_wait_samples"`
}

// OpsRunsPoint is one (bucket, machine) cell of chart 1. `_max` feeds lines,
// `_last` feeds stacks. Source "history" points come from the task-row replay:
// running is exact, queued is approximate and dispatched/waiting_dir are 0.
// SampleCount < ExpectedCount marks a partly covered bucket (n/expected).
type OpsRunsPoint struct {
	Ts             time.Time `json:"ts"`
	Machine        string    `json:"machine"`
	Source         string    `json:"source"`
	QueuedMax      int32     `json:"queued_max"`
	QueuedLast     int32     `json:"queued_last"`
	DispatchedMax  int32     `json:"dispatched_max"`
	DispatchedLast int32     `json:"dispatched_last"`
	WaitingDirMax  int32     `json:"waiting_dir_max"`
	WaitingDirLast int32     `json:"waiting_dir_last"`
	RunningMax     int32     `json:"running_max"`
	RunningLast    int32     `json:"running_last"`
	SampleCount    int32     `json:"sample_count"`
	ExpectedCount  int32     `json:"expected_count"`
}

type OpsRequeuePoint struct {
	Ts            time.Time `json:"ts"`
	ClaimFailures int32     `json:"claim_failures"`
	StaleReclaims int32     `json:"stale_reclaims"`
}

// OpsQueueWaitPoint: Machine "" is the all-machines row.
type OpsQueueWaitPoint struct {
	Ts      time.Time `json:"ts"`
	Machine string    `json:"machine"`
	Samples int32     `json:"samples"`
	P50S    float64   `json:"p50_s"`
	P95S    float64   `json:"p95_s"`
}

type OpsOldestPoint struct {
	Ts   time.Time `json:"ts"`
	MaxS int32     `json:"max_s"`
}

// OpsFailurePoint: FailureReason "" is the succeeded bucket.
type OpsFailurePoint struct {
	Ts            time.Time `json:"ts"`
	Machine       string    `json:"machine"`
	FailureReason string    `json:"failure_reason"`
	TaskCount     int32     `json:"task_count"`
}

type OpsAgentStatePoint struct {
	Ts         time.Time `json:"ts"`
	Machine    string    `json:"machine"`
	Unbound    int32     `json:"unbound"`
	Offline    int32     `json:"offline"`
	Busy       int32     `json:"busy"`
	WaitingDir int32     `json:"waiting_dir"`
	Available  int32     `json:"available"`
}

type OpsResponse struct {
	Window        string    `json:"window"`
	BucketS       int32     `json:"bucket_s"`
	Since         time.Time `json:"since"`
	Until         time.Time `json:"until"`
	ThresholdP95S int32     `json:"threshold_p95_s"`
	// SnapshotSince is the first snapshot (nil before the first tick); runs
	// points older than it are "history". LastSnapshotAt drives the freshness line.
	SnapshotSince  *time.Time           `json:"snapshot_since"`
	LastSnapshotAt *time.Time           `json:"last_snapshot_at"`
	Now            OpsNow               `json:"now"`
	Runs           []OpsRunsPoint       `json:"runs"`
	Requeues       []OpsRequeuePoint    `json:"requeues"`
	QueueWait      []OpsQueueWaitPoint  `json:"queue_wait"`
	OldestUnstart  []OpsOldestPoint     `json:"oldest_unstarted"`
	Failures       []OpsFailurePoint    `json:"failures"`
	AgentStates    []OpsAgentStatePoint `json:"agent_states"`
}

func tsPtr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}

func (h *Handler) GetOps(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "24h"
	}
	spec, ok := opsWindows[window]
	if !ok {
		writeError(w, http.StatusBadRequest, "window must be one of 1h, 24h, 7d, 30d")
		return
	}
	ctx := r.Context()
	ws := parseUUID(workspaceID)
	until := time.Now().UTC()
	since := until.Add(-spec.span)
	pgSince := pgtype.Timestamptz{Time: since, Valid: true}
	pgUntil := pgtype.Timestamptz{Time: until, Valid: true}
	fail := func(what string) {
		writeError(w, http.StatusInternalServerError, "failed to load ops "+what)
	}

	resp := OpsResponse{
		Window: window, BucketS: spec.bucketS, Since: since, Until: until,
		ThresholdP95S: opsThresholdP95S,
		Runs:          []OpsRunsPoint{}, Requeues: []OpsRequeuePoint{}, QueueWait: []OpsQueueWaitPoint{},
		OldestUnstart: []OpsOldestPoint{}, Failures: []OpsFailurePoint{}, AgentStates: []OpsAgentStatePoint{},
		Now: OpsNow{Machines: []OpsNowMachine{}},
	}

	bounds, err := h.Queries.GetOpsSnapshotBounds(ctx, ws)
	if err != nil {
		fail("bounds")
		return
	}
	resp.SnapshotSince, resp.LastSnapshotAt = tsPtr(bounds.FirstTs), tsPtr(bounds.LastTs)

	latest, err := h.Queries.ListOpsSnapshotLatest(ctx, ws)
	if err != nil {
		fail("snapshot")
		return
	}
	for _, l := range latest {
		m := OpsNowMachine{
			Machine: l.DaemonID, Queued: l.Queued, Dispatched: l.Dispatched, WaitingDir: l.WaitingDir, Running: l.Running,
			AgentsUnbound: l.AgentsUnbound, AgentsOffline: l.AgentsOffline, AgentsBusy: l.AgentsBusy,
			AgentsWaitingDir: l.AgentsWaitingDir, AgentsAvailable: l.AgentsAvailable,
		}
		if l.OldestUnstartedS.Valid {
			m.OldestUnstartedS = &l.OldestUnstartedS.Int32
		}
		resp.Now.Machines = append(resp.Now.Machines, m)
	}
	recent, err := h.Queries.GetOpsQueueWaitRecent(ctx, db.GetOpsQueueWaitRecentParams{
		WorkspaceID: ws, Since: pgtype.Timestamptz{Time: until.Add(-15 * time.Minute), Valid: true},
	})
	if err != nil {
		fail("queue wait")
		return
	}
	resp.Now.QueueWaitP95S, resp.Now.QueueWaitSamples = recent.P95S, recent.Samples

	// Charts 1 and 4: snapshots, with a task-row replay before the first one.
	expected := spec.bucketS / opsSnapshotIntervalS
	var snaps []db.ListOpsSnapshotSeriesRow
	if window == "30d" {
		rows, err := h.Queries.ListOpsSnapshot5mSeries(ctx, db.ListOpsSnapshot5mSeriesParams{
			BucketS: spec.bucketS, WorkspaceID: ws, Since: pgSince, Until: pgUntil,
		})
		if err != nil {
			fail("snapshot series")
			return
		}
		for _, row := range rows {
			snaps = append(snaps, db.ListOpsSnapshotSeriesRow(row))
		}
	} else {
		snaps, err = h.Queries.ListOpsSnapshotSeries(ctx, db.ListOpsSnapshotSeriesParams{
			BucketS: spec.bucketS, WorkspaceID: ws, Since: pgSince, Until: pgUntil,
		})
		if err != nil {
			fail("snapshot series")
			return
		}
	}
	histUntil := pgUntil
	if resp.SnapshotSince != nil && resp.SnapshotSince.Before(until) {
		histUntil = pgtype.Timestamptz{Time: *resp.SnapshotSince, Valid: true}
	}
	if histUntil.Time.After(since) {
		hist, err := h.Queries.ListOpsRunsHistory(ctx, db.ListOpsRunsHistoryParams{
			BucketS: spec.bucketS, Since: pgSince, Until: histUntil, WorkspaceID: ws,
		})
		if err != nil {
			fail("runs history")
			return
		}
		for _, row := range hist {
			resp.Runs = append(resp.Runs, OpsRunsPoint{
				Ts: row.Ts.Time.UTC(), Machine: row.Machine, Source: "history",
				QueuedMax: row.QueuedApprox, QueuedLast: row.QueuedApprox,
				RunningMax: row.Running, RunningLast: row.Running,
			})
		}
	}
	for _, s := range snaps {
		ts := s.Ts.Time.UTC()
		resp.Runs = append(resp.Runs, OpsRunsPoint{
			Ts: ts, Machine: s.DaemonID, Source: "snapshot",
			QueuedMax: s.QueuedMax, QueuedLast: s.QueuedLast,
			DispatchedMax: s.DispatchedMax, DispatchedLast: s.DispatchedLast,
			WaitingDirMax: s.WaitingDirMax, WaitingDirLast: s.WaitingDirLast,
			RunningMax: s.RunningMax, RunningLast: s.RunningLast,
			SampleCount: s.SampleCount, ExpectedCount: expected,
		})
		resp.AgentStates = append(resp.AgentStates, OpsAgentStatePoint{
			Ts: ts, Machine: s.DaemonID, Unbound: s.AgentsUnboundLast, Offline: s.AgentsOfflineLast,
			Busy: s.AgentsBusyLast, WaitingDir: s.AgentsWaitingDirLast, Available: s.AgentsAvailableLast,
		})
		// Rows arrive ordered by ts, so one line per bucket is the max across machines.
		if n := len(resp.OldestUnstart); n > 0 && resp.OldestUnstart[n-1].Ts.Equal(ts) {
			resp.OldestUnstart[n-1].MaxS = max(resp.OldestUnstart[n-1].MaxS, s.OldestUnstartedSMax)
		} else {
			resp.OldestUnstart = append(resp.OldestUnstart, OpsOldestPoint{Ts: ts, MaxS: s.OldestUnstartedSMax})
		}
	}

	requeues, err := h.Queries.ListOpsRequeues(ctx, db.ListOpsRequeuesParams{
		BucketS: spec.bucketS, WorkspaceID: ws, Since: pgSince, Until: pgUntil,
	})
	if err != nil {
		fail("requeues")
		return
	}
	for _, q := range requeues {
		resp.Requeues = append(resp.Requeues, OpsRequeuePoint{Ts: q.Ts.Time.UTC(), ClaimFailures: q.ClaimFailures, StaleReclaims: q.StaleReclaims})
	}

	waits, err := h.Queries.ListOpsQueueWait(ctx, db.ListOpsQueueWaitParams{
		BucketS: spec.bucketS, WorkspaceID: ws, Since: pgSince, Until: pgUntil,
	})
	if err != nil {
		fail("queue wait series")
		return
	}
	for _, q := range waits {
		resp.QueueWait = append(resp.QueueWait, OpsQueueWaitPoint{Ts: q.Ts.Time.UTC(), Machine: q.Machine, Samples: q.Samples, P50S: q.P50S, P95S: q.P95S})
	}

	failures, err := h.Queries.ListOpsFailures(ctx, db.ListOpsFailuresParams{
		BucketS: spec.bucketS, WorkspaceID: ws, Since: pgSince, Until: pgUntil,
	})
	if err != nil {
		fail("failures")
		return
	}
	for _, f := range failures {
		resp.Failures = append(resp.Failures, OpsFailurePoint{Ts: f.Ts.Time.UTC(), Machine: f.Machine, FailureReason: f.FailureReason, TaskCount: f.TaskCount})
	}

	writeJSON(w, http.StatusOK, resp)
}
