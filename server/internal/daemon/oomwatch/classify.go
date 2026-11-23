package oomwatch

import (
	"regexp"
	"strconv"
	"strings"
)

// Config is the classifier's view of the host.
type Config struct {
	// ServiceCgroup is the cgroup path of multica.service as the kernel prints
	// it in oom_memcg, for example
	// /user.slice/user-1000.slice/user@1000.service/app.slice/multica.service
	ServiceCgroup string
}

const (
	constraintPrefix = "oom-kill:constraint="
	groupMarker      = "memory.oom.group"
	headerMarker     = " invoked oom-killer: "
	runsSegment      = "runs"
)

var (
	// A comm may hold commas, spaces and parentheses, so the pid and uid are
	// read from the end of the constraint line.
	constraintTail = regexp.MustCompile(`,task=.*,pid=(\d+),uid=\d+$`)
	// Anchored: a task-table row that names "Killed process" is not a record.
	recordLine = regexp.MustCompile(`^(?:Memory cgroup out of memory|Out of memory[^:]*): Killed process (\d+) \(`)
	recordHead = regexp.MustCompile(`^(?:Memory cgroup out of memory|Out of memory[^:]*): Killed process `)
)

// lastLine is the last OOM-killer line the classifier saw. A record joins only
// the constraint line directly before it, and only once.
type lastLine int

const (
	lastNone       lastLine = iota // nothing, or a line that broke the join
	lastHeader                     // "<comm> invoked oom-killer"
	lastConstraint                 // a constraint line, not yet joined
	lastConsumed                   // the constraint line already joined a record
)

type constraint struct {
	valid    bool
	kind     string // CONSTRAINT_MEMCG, CONSTRAINT_NONE, ...
	oomMemcg string
	pid      int
}

// Classifier reads kernel message lines in journal order. It is not safe for
// concurrent use; the Reader serializes calls.
type Classifier struct {
	cfg  Config
	last lastLine
	con  constraint
}

// NewClassifier returns a classifier with no pending OOM-killer line.
func NewClassifier(cfg Config) *Classifier { return &Classifier{cfg: cfg} }

// Feed takes one kernel message (the MESSAGE field of a _TRANSPORT=kernel
// entry). It returns a verdict for a `Killed process` record or a
// memory.oom.group line, and false for every other line.
func (c *Classifier) Feed(message string) (Verdict, bool) {
	switch {
	case strings.Contains(message, groupMarker):
		// Any group line fails closed. It also breaks the join: the kernel
		// prints it between the constraint line and the record.
		c.last = lastNone
		return Verdict{Line: message, Hold: true, Cause: HoldOOMGroup}, true
	case strings.Contains(message, headerMarker):
		c.last = lastHeader
		return Verdict{}, false
	case strings.HasPrefix(message, constraintPrefix):
		c.con = parseConstraint(message)
		c.last = lastConstraint
		return Verdict{}, false
	case recordHead.MatchString(message):
		return c.record(message), true
	}
	return Verdict{}, false
}

func (c *Classifier) record(line string) Verdict {
	v := Verdict{Line: line}
	state, con := c.last, c.con
	c.last = lastConsumed

	if m := recordLine.FindStringSubmatch(line); m != nil {
		v.PID, _ = strconv.Atoi(m[1])
	}
	hold := func(cause HoldCause) Verdict {
		v.Hold, v.Cause = true, cause
		return v
	}

	switch state {
	case lastConsumed:
		return hold(HoldConstraintRejoined)
	case lastNone, lastHeader:
		return hold(HoldNoConstraintLine)
	}
	v.Constraint, v.OOMMemcg = con.kind, con.oomMemcg
	if !con.valid || v.PID == 0 {
		return hold(HoldUnparsable)
	}
	if con.pid != v.PID {
		return hold(HoldConstraintPIDMiss)
	}
	if con.kind != "CONSTRAINT_MEMCG" {
		return hold(HoldConstraintNotMemcg)
	}
	class, cause := c.classifyMemcg(con.oomMemcg)
	if cause != "" {
		return hold(cause)
	}
	v.Class = class
	return v
}

// classifyMemcg decides by whole path segments, never by string prefix.
func (c *Classifier) classifyMemcg(oomMemcg string) (Class, HoldCause) {
	svc, ok := cgroupSegments(c.cfg.ServiceCgroup)
	if !ok || len(svc) == 0 {
		return "", HoldUnparsable
	}
	o, ok := cgroupSegments(oomMemcg)
	if !ok {
		return "", HoldUnparsable
	}
	switch {
	case len(o) < len(svc) && hasPrefix(svc, o):
		return "", HoldAncestorMemcg
	case hasPrefix(o, svc):
		if len(o) > len(svc) && o[len(svc)] == runsSegment {
			return ClassContained, ""
		}
		return "", HoldServiceOutsideRuns
	}
	return ClassForeignMemcg, ""
}

func hasPrefix(s, prefix []string) bool {
	if len(prefix) > len(s) {
		return false
	}
	for i := range prefix {
		if s[i] != prefix[i] {
			return false
		}
	}
	return true
}

// cgroupSegments splits an absolute cgroup path. Relative paths, empty, "."
// and ".." segments are refused.
func cgroupSegments(p string) ([]string, bool) {
	if !strings.HasPrefix(p, "/") {
		return nil, false
	}
	if p == "/" {
		return nil, true
	}
	segs := strings.Split(p[1:], "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return nil, false
		}
	}
	return segs, true
}

func parseConstraint(line string) constraint {
	rest := strings.TrimPrefix(line, constraintPrefix)
	kind, _, _ := strings.Cut(rest, ",")
	tail := constraintTail.FindStringSubmatchIndex(rest)
	if kind == "" || tail == nil {
		return constraint{kind: kind}
	}
	pid, err := strconv.Atoi(rest[tail[2]:tail[3]])
	if err != nil {
		return constraint{kind: kind}
	}
	// oom_memcg comes before task_memcg and the comm; search only that part.
	head := rest[:tail[0]]
	if i := strings.Index(head, ",task_memcg="); i >= 0 {
		head = head[:i]
	}
	var oom string
	if i := strings.Index(head, ",oom_memcg="); i >= 0 {
		oom = head[i+len(",oom_memcg="):]
		if j := strings.IndexByte(oom, ','); j >= 0 {
			oom = oom[:j]
		}
	}
	return constraint{valid: true, kind: kind, oomMemcg: oom, pid: pid}
}
