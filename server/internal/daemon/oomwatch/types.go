package oomwatch

import "time"

// Class is the outcome of a kill that did not latch the hold.
type Class string

const (
	// ClassContained: CONSTRAINT_MEMCG with oom_memcg under <service>/runs/. The
	// kill is bounded by a run leaf; the runs aggregate itself is not contained.
	ClassContained Class = "contained"
	// ClassForeignMemcg: CONSTRAINT_MEMCG with oom_memcg outside the service
	// (a Docker scope, another unit). Bounded by that cgroup's own limit.
	ClassForeignMemcg Class = "foreign_memcg"
)

// HoldCause names why a record or condition latches the hold.
type HoldCause string

const (
	HoldConstraintNotMemcg HoldCause = "constraint_not_memcg"    // CONSTRAINT_NONE, CPUSET, MEMORY_POLICY, unknown
	HoldServiceOutsideRuns HoldCause = "service_outside_runs"    // multica.service itself, runs, daemon, prep/*, .control
	HoldAncestorMemcg      HoldCause = "ancestor_memcg"          // oom_memcg is an ancestor of the service
	HoldNoConstraintLine   HoldCause = "no_constraint_line"      // headerless record: last OOM-killer line is not a constraint line
	HoldConstraintPIDMiss  HoldCause = "constraint_pid_mismatch" // constraint line names another PID
	HoldConstraintRejoined HoldCause = "constraint_rejoined"     // constraint line already joined to a record (cannot happen: the record replaces it)
	HoldOOMGroup           HoldCause = "oom_group"               // any memory.oom.group line
	HoldUnparsable         HoldCause = "unparsable_record"       // record or constraint line cannot be parsed
	HoldHeartbeatMissed    HoldCause = "heartbeat_missed"        // a nonce was not seen within 10 s
	HoldLiveGap            HoldCause = "live_gap"                // same boot, cursor lost
	HoldCheckpointTorn     HoldCause = "checkpoint_torn"         // checkpoint file exists but cannot be read
)

// Verdict is the classifier's decision for one `Killed process` record or one
// memory.oom.group line. Exactly one of Class and Hold is set.
type Verdict struct {
	Line       string // the kernel message the verdict is about
	PID        int    // victim PID from the record (0 when unparsable)
	Class      Class
	Hold       bool
	Cause      HoldCause
	Constraint string // e.g. CONSTRAINT_MEMCG (empty when no constraint line joined)
	OOMMemcg   string // cgroup that hit its limit (empty when none)
}

// HoldEvent asks the hold core to latch.
type HoldEvent struct {
	Cause  HoldCause
	Detail string
	At     time.Time
}

// GapReason names why a telemetry gap opened. Every gap is NOT RUN for C2 and A2.
type GapReason string

const (
	GapBootChange      GapReason = "boot_change"
	GapCursorInvalid   GapReason = "cursor_invalid"
	GapHeartbeatMissed GapReason = "heartbeat_missed"
	GapResume          GapReason = "resume_gap" // more than 2 heartbeat periods between the last heartbeat and restart
	GapCheckpointTorn  GapReason = "checkpoint_torn"
)

// GapEvent opens a telemetry gap.
type GapEvent struct {
	Reason GapReason
	From   time.Time // last persisted heartbeat (zero when unknown)
	To     time.Time
}

// Sinks receives what the reader decides. Every field may be nil.
type Sinks struct {
	Hold    func(HoldEvent)
	Gap     func(GapEvent)
	Verdict func(Verdict)                // each classified kill (for S0m and verdict lists)
	Ignored func(reason string, e Entry) // history, other boot, unknown nonce, duplicate: logged, never a trigger
}
