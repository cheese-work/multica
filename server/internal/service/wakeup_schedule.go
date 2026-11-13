package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Scheduled custom rules (CHE-1082 L11). A schedule trigger is a fixed instant
// (at), a delay from each issue's own activation (relative), a fixed interval
// (every) or a cron expression in an IANA zone (cron). Each issue the rule
// reaches gets an instance that the ordinary scheduler tick fires; the
// instance's kind is the legacy one (at, every, cron), so firing, receipts,
// coalescing, max fires, expiry and the capacity and aggregate gates are the
// ones every other instance already has.
//
// Missed occurrences are never replayed. An issue that enters after a one-shot
// instant gets no instance. An instance that was down, held by a full pool or
// retired resumes at the next future occurrence. Downtime of a running instance
// is a single coalesced run, as for any scheduled wakeup.
const (
	wakeupTriggerKindAt       = "at"
	wakeupTriggerKindRelative = "relative"
	wakeupTriggerKindEvery    = "every"
	wakeupTriggerKindCron     = "cron"
)

var wakeupScheduleKinds = []string{wakeupTriggerKindAt, wakeupTriggerKindRelative, wakeupTriggerKindEvery, wakeupTriggerKindCron}

func isScheduleTriggerKind(kind string) bool { return slices.Contains(wakeupScheduleKinds, kind) }

// wakeupScheduleSpec is the shape of a definition's schedule field; the member
// set must be the one its trigger kind names.
type wakeupScheduleSpec struct {
	At              *time.Time `json:"at,omitempty"`
	AfterSeconds    *int64     `json:"after_seconds,omitempty"`
	IntervalSeconds *int64     `json:"interval_seconds,omitempty"`
	Cron            string     `json:"cron,omitempty"`
	Timezone        string     `json:"timezone,omitempty"`
}

// kind is the trigger kind the spec's one member belongs to.
func (s wakeupScheduleSpec) kind() (string, error) {
	var kinds []string
	for kind, set := range map[string]bool{
		wakeupTriggerKindAt: s.At != nil, wakeupTriggerKindRelative: s.AfterSeconds != nil,
		wakeupTriggerKindEvery: s.IntervalSeconds != nil, wakeupTriggerKindCron: s.Cron != "",
	} {
		if set {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) != 1 {
		return "", wakeupDefinitionBad("a schedule takes exactly one of at, after_seconds, interval_seconds or cron")
	}
	if (s.Timezone != "") != (kinds[0] == wakeupTriggerKindCron) {
		return "", wakeupDefinitionBad("a cron schedule takes a timezone, and no other schedule does")
	}
	switch kinds[0] {
	case wakeupTriggerKindRelative:
		if *s.AfterSeconds < 60 || *s.AfterSeconds > maxWakeupSeconds {
			return "", wakeupDefinitionBad("schedule after_seconds must be 60–%d", maxWakeupSeconds)
		}
	case wakeupTriggerKindEvery:
		if *s.IntervalSeconds < 60 || *s.IntervalSeconds > maxWakeupSeconds {
			return "", wakeupDefinitionBad("schedule interval_seconds must be 60–%d", maxWakeupSeconds)
		}
	}
	return kinds[0], nil
}

// validate checks the spec as a write: an instant still ahead and a cron
// expression that parses in its zone and has an occurrence, on top of the
// shape and ranges kind checks for every read.
func (s wakeupScheduleSpec) validate(now time.Time) (string, error) {
	kind, err := s.kind()
	if err != nil {
		return "", err
	}
	switch kind {
	case wakeupTriggerKindAt:
		if !s.At.After(now) || s.At.Sub(now) > maxWakeupSeconds*time.Second {
			return "", wakeupDefinitionBad("schedule at must be in the next year")
		}
	case wakeupTriggerKindCron:
		if strings.Contains(s.Cron, "TZ=") {
			return "", wakeupDefinitionBad("cron takes its zone in timezone, not inline")
		}
		if err := ValidateTimezone(s.Timezone); err != nil {
			return "", wakeupDefinitionBad("unknown timezone %q", s.Timezone)
		}
		if next, err := NextOccurrenceAfterUTC(s.Cron, s.Timezone, now); err != nil || next.IsZero() {
			return "", wakeupDefinitionBad("cron must be a valid expression with a future occurrence")
		}
	}
	return kind, nil
}

// wakeupSchedule is a resolved rule's schedule: the trigger kind and its spec.
type wakeupSchedule struct {
	Kind string
	Spec wakeupScheduleSpec
}

// customSchedule reads a resolved rule's schedule. nil with no error for a
// rule that has none (every other trigger kind); an error for a schedule
// trigger whose schedule is missing or unreadable, which makes the rule
// invalid. Time-dependent checks are the write's; a stored instant that has
// since passed is still a readable schedule.
func customSchedule(c WakeupConfigPatch) (*wakeupSchedule, error) {
	trigger, ok := triggerSpec(c.Trigger)
	if !ok || !isScheduleTriggerKind(trigger.Kind) {
		return nil, nil
	}
	var spec wakeupScheduleSpec
	if !c.Schedule.Set || c.Schedule.Null || decodeWakeupSpec(c.Schedule.Value, &spec) != nil {
		return nil, errors.New("unreadable schedule")
	}
	kind, err := spec.kind()
	if err != nil || kind != trigger.Kind {
		return nil, errors.New("schedule does not fit its trigger")
	}
	if kind == wakeupTriggerKindCron {
		if _, _, err := parseCronSchedule(spec.Cron, spec.Timezone); err != nil {
			return nil, err
		}
	}
	return &wakeupSchedule{Kind: kind, Spec: spec}, nil
}

// oneShot: the rule fires once per issue. Safe on a nil schedule.
func (s *wakeupSchedule) oneShot() bool {
	return s != nil && (s.Kind == wakeupTriggerKindAt || s.Kind == wakeupTriggerKindRelative)
}

// instanceKind is the kind of the issue_wakeup row; a delay is a fixed instant
// once it is anchored to the instance.
func (s *wakeupSchedule) instanceKind() string {
	switch {
	case s == nil:
		return wakeupTriggerKindEvent
	case s.Kind == wakeupTriggerKindRelative:
		return wakeupTriggerKindAt
	}
	return s.Kind
}

// intervalSeconds, cronExpression and timezone are what an instance row records
// of its schedule; safe on a nil schedule.
func (s *wakeupSchedule) intervalSeconds() pgtype.Int8 {
	if s == nil || s.Kind != wakeupTriggerKindEvery {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *s.Spec.IntervalSeconds, Valid: true}
}

func (s *wakeupSchedule) cronExpression() pgtype.Text {
	if s == nil || s.Kind != wakeupTriggerKindCron {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s.Spec.Cron, Valid: true}
}

func (s *wakeupSchedule) timezone() string {
	if s == nil || s.Kind != wakeupTriggerKindCron {
		return "UTC"
	}
	return s.Spec.Timezone
}

// wakeupScheduled is where a schedule stands: its next occurrence strictly after
// now, and Elapsed for a one-shot whose instant has passed (Next is then that
// instant) or a cron with no occurrence left (Next is invalid).
type wakeupScheduled struct {
	Next    pgtype.Timestamptz
	Elapsed bool
}

// scheduleOf places a schedule on the clock. anchor is when the instance began:
// a delay counts from it, and an interval counts its ticks from it.
func scheduleOf(kind string, spec wakeupScheduleSpec, anchor, now time.Time) (wakeupScheduled, error) {
	at := func(t time.Time) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: t.Truncate(time.Microsecond), Valid: true}
	}
	switch kind {
	case wakeupTriggerKindAt:
		return wakeupScheduled{Next: at(*spec.At), Elapsed: !spec.At.After(now)}, nil
	case wakeupTriggerKindRelative:
		due := anchor.Add(time.Duration(*spec.AfterSeconds) * time.Second)
		return wakeupScheduled{Next: at(due), Elapsed: !due.After(now)}, nil
	case wakeupTriggerKindEvery:
		step := time.Duration(*spec.IntervalSeconds) * time.Second
		return wakeupScheduled{Next: at(nextTick(anchor, step, now))}, nil
	case wakeupTriggerKindCron:
		next, err := NextOccurrenceAfterUTC(spec.Cron, spec.Timezone, now)
		if err != nil {
			return wakeupScheduled{}, err
		}
		if next.IsZero() {
			return wakeupScheduled{Elapsed: true}, nil
		}
		return wakeupScheduled{Next: at(next)}, nil
	}
	return wakeupScheduled{}, errors.New("not a schedule kind")
}

// nextTick is the first anchor+n*step (n >= 1) after now.
func nextTick(anchor time.Time, step time.Duration, now time.Time) time.Time {
	if !now.After(anchor) {
		return anchor.Add(step)
	}
	return anchor.Add((now.Sub(anchor)/step + 1) * step)
}

// scheduleUnchanged reports whether an instance already runs the schedule, so
// an edit that did not touch it (the instruction, say) keeps the tick the
// instance is on.
func scheduleUnchanged(w db.IssueWakeup, s *wakeupSchedule) bool {
	if w.Kind != s.instanceKind() {
		return false
	}
	spec := s.Spec
	switch s.Kind {
	case wakeupTriggerKindAt:
		return w.NextFireAt.Valid && w.NextFireAt.Time.Equal(spec.At.Truncate(time.Microsecond))
	case wakeupTriggerKindRelative:
		return w.NextFireAt.Valid && w.NextFireAt.Time.Equal(w.CreatedAt.Time.Add(time.Duration(*spec.AfterSeconds)*time.Second))
	case wakeupTriggerKindEvery:
		return w.IntervalSeconds.Valid && w.IntervalSeconds.Int64 == *spec.IntervalSeconds
	}
	return w.CronExpression.String == spec.Cron && w.Timezone == spec.Timezone
}

// settleScheduledInstance runs before a default-derived schedule instance is
// applied (a new one, one a full pool held, one a retired rule left idle). A
// repeating schedule moves to its next future occurrence, so what was missed
// meanwhile is not replayed. A one-shot keeps its instant even when a full pool
// held it past that instant: the delayed fire is delivered once the pool admits
// it, subject to expiry. A one-shot with no instant left has fired or was
// retired, so it is not owed again. A stored schedule that is not readable or is
// out of range ends only this instance. live says the instance may be applied.
func settleScheduledInstance(ctx context.Context, q *db.Queries, w db.IssueWakeup, now time.Time) (live bool, err error) {
	var spec wakeupScheduleSpec
	switch w.Kind {
	case wakeupTriggerKindAt:
		if w.NextFireAt.Valid {
			return true, nil
		}
	case wakeupTriggerKindEvery:
		if w.IntervalSeconds.Valid {
			spec.IntervalSeconds = &w.IntervalSeconds.Int64
		}
	case wakeupTriggerKindCron:
		spec.Cron, spec.Timezone = w.CronExpression.String, w.Timezone
	default:
		return true, nil
	}
	if w.Kind != wakeupTriggerKindAt {
		if _, kindErr := spec.kind(); kindErr == nil {
			if next, err := scheduleOf(w.Kind, spec, w.CreatedAt.Time, now); err == nil && !next.Elapsed {
				return true, q.SetScheduledWakeupNext(ctx, db.SetScheduledWakeupNextParams{ID: w.ID, NextFireAt: next.Next})
			}
		}
	}
	if err := q.MarkWakeupTimedOut(ctx, w.ID); err != nil {
		return false, err
	}
	return false, q.ClearEndedWakeupCapacityReason(ctx, w.ID)
}

// validateEffectiveSchedule checks the schedule and the trigger it belongs to
// as the complete resolved rule, because an override may change either alone: a
// schedule trigger needs the schedule of its own kind, no other trigger takes
// one, and a one-shot cannot repeat while an interval cannot be once.
func validateEffectiveSchedule(c WakeupConfigPatch) error {
	trigger, _ := triggerSpec(c.Trigger)
	scheduled := c.Schedule.Set && !c.Schedule.Null
	if !isScheduleTriggerKind(trigger.Kind) {
		if scheduled {
			return wakeupDefinitionBad("a schedule applies to at, relative, every and cron triggers only")
		}
		return nil
	}
	if !scheduled {
		return wakeupDefinitionBad("a %s trigger needs a schedule", trigger.Kind)
	}
	var spec wakeupScheduleSpec
	if err := decodeWakeupSpec(c.Schedule.Value, &spec); err != nil {
		return wakeupDefinitionBad("invalid schedule: %v", err)
	}
	kind, err := spec.kind()
	if err != nil {
		return err
	}
	if kind != trigger.Kind {
		return wakeupDefinitionBad("a %s trigger needs a schedule with %s", trigger.Kind, scheduleMember[trigger.Kind])
	}
	switch oneShot := kind == wakeupTriggerKindAt || kind == wakeupTriggerKindRelative; {
	case oneShot && (c.MaxFires.Set || c.Mode.Set && c.Mode.Value != "once"):
		return wakeupDefinitionBad("a one-shot schedule fires once: it takes no max_fires and no continuous mode")
	case !oneShot && c.Mode.Set && c.Mode.Value == "once":
		return wakeupDefinitionBad("a repeating schedule is not once; set max_fires to limit it")
	}
	return nil
}

var scheduleMember = map[string]string{
	wakeupTriggerKindAt: "at", wakeupTriggerKindRelative: "after_seconds",
	wakeupTriggerKindEvery: "interval_seconds", wakeupTriggerKindCron: "cron and timezone",
}
