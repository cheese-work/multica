package handler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	hostSampleMaxFuture = time.Minute
	hostSampleMaxAge    = 24 * time.Hour
	hostSampleMaxMount  = 4096

	// hostSampleIngestTimeout bounds metric storage so a stuck insert cannot
	// hold the heartbeat ack (and, on WS, the read pump).
	hostSampleIngestTimeout = 2 * time.Second
)

// hostSampleSeen maps daemon_id to the newest host sample time already stored.
// Package-level because Handler is copied by value.
var hostSampleSeen sync.Map

// ingestHostSample stores a daemon host sample. A bad or failed sample is
// logged and dropped: it never fails the heartbeat that carried it.
func (h *Handler) ingestHostSample(ctx context.Context, daemonID string, s *protocol.DaemonHostSample) {
	if s == nil || daemonID == "" {
		return
	}
	if err := validHostSample(s, time.Now()); err != nil {
		slog.Warn("host sample dropped", "daemon_id", daemonID, "error", err)
		return
	}
	// One row set per daemon per minute: a sampler restart inside a minute must
	// land on the same key.
	minute := s.SampledAt.UTC().Truncate(time.Minute)
	// Every runtime of the daemon repeats the same sample on every heartbeat;
	// the unique key makes that harmless, this skips the round trips.
	// ponytail: per-process memory, a restart re-sends once and the key dedupes it.
	if seen, ok := hostSampleSeen.Load(daemonID); ok {
		if at, isTime := seen.(time.Time); isTime && !minute.After(at) {
			return
		}
	}
	ctx, cancel := context.WithTimeout(ctx, hostSampleIngestTimeout)
	defer cancel()
	at := pgtype.Timestamptz{Time: minute, Valid: true}
	if err := h.Queries.InsertHostSampleCPURAM(ctx, db.InsertHostSampleCPURAMParams{
		DaemonID: daemonID, SampledAt: at,
		CpuPercent: pgtype.Float4{Float32: float32(s.CPUPercent), Valid: true},
		RamPercent: pgtype.Float4{Float32: float32(s.RAMPercent), Valid: true},
	}); err != nil {
		slog.Warn("host sample insert failed", "daemon_id", daemonID, "error", err)
		return
	}
	for _, d := range s.Disks {
		if err := h.Queries.InsertHostSampleDisk(ctx, db.InsertHostSampleDiskParams{
			DaemonID: daemonID, SampledAt: at, MountPath: d.MountPath,
			DiskPercent: pgtype.Float4{Float32: float32(d.Percent), Valid: true},
		}); err != nil {
			slog.Warn("host sample disk insert failed", "daemon_id", daemonID, "mount_path", d.MountPath, "error", err)
			return
		}
	}
	hostSampleSeen.Store(daemonID, minute)
}

func validHostSample(s *protocol.DaemonHostSample, now time.Time) error {
	if s.SampledAt.IsZero() || s.SampledAt.After(now.Add(hostSampleMaxFuture)) || s.SampledAt.Before(now.Add(-hostSampleMaxAge)) {
		return errHostSample("sampled_at out of range")
	}
	if !validPercent(s.CPUPercent) || !validPercent(s.RAMPercent) {
		return errHostSample("cpu/ram percent out of range")
	}
	if len(s.Disks) > protocol.MaxHostSampleDisks {
		return errHostSample("too many disks")
	}
	seen := make(map[string]bool, len(s.Disks))
	for _, d := range s.Disks {
		if d.MountPath == "" || len(d.MountPath) > hostSampleMaxMount || seen[d.MountPath] || !validPercent(d.Percent) {
			return errHostSample("bad disk entry")
		}
		seen[d.MountPath] = true
	}
	return nil
}

// validPercent is false for NaN as well as out-of-range values.
func validPercent(p float64) bool { return p >= 0 && p <= 100 }

type errHostSample string

func (e errHostSample) Error() string { return string(e) }
