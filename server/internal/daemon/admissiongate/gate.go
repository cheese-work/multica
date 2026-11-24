// Package admissiongate is the S3b claim gate of the CHE-7168 containment
// plan (step 8). It decides, from fresh host samples, whether the daemon may
// claim new work. It has no I/O and no clock of its own: the caller feeds
// samples and passes "now". Thresholds are configuration; their values come
// from S0m (step 15), not from this package.
package admissiongate

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Reason says why the gate is closed.
type Reason string

const (
	ReasonNone         Reason = ""
	ReasonNoSample     Reason = "no_sample"
	ReasonStale        Reason = "stale_sample"
	ReasonSignal       Reason = "signal_over_threshold"
	ReasonMemAvailable Reason = "mem_available_low"
)

// Signal is one upper signal (for example memory PSI full avg10). The gate
// closes when the value is at or above Close and reopens at Close/2.
type Signal struct {
	Name  string
	Close float64
}

// Config holds the gate thresholds. All values are configuration.
type Config struct {
	// Interval is the sampling interval. A sample older than 2*Interval
	// closes the gate.
	Interval time.Duration
	// ReopenAfter is how long a signal must stay in its reopen band before
	// the gate reopens (120 s in the plan).
	ReopenAfter time.Duration
	Signals     []Signal
	// MemAvailableFloor is F in bytes: the gate closes below it.
	MemAvailableFloor uint64
	// RDefault is R_default in bytes: the reopen margin is
	// max(RDefault, 0.1*F).
	RDefault uint64
}

// Sample is one host reading.
type Sample struct {
	At           time.Time
	Signals      map[string]float64
	MemAvailable uint64
}

// Status is the gate state at one instant.
type Status struct {
	Open   bool
	Reason Reason
	// Detail names the signal behind ReasonSignal.
	Detail string
	// ClosedSince is the time the gate last went from open to closed, as
	// seen by Evaluate; zero while open.
	ClosedSince time.Time
}

// Validate rejects a config that cannot gate safely.
func (c Config) Validate() error {
	if c.Interval <= 0 {
		return errors.New("admissiongate: Interval must be positive")
	}
	if c.ReopenAfter <= 0 {
		return errors.New("admissiongate: ReopenAfter must be positive")
	}
	if c.MemAvailableFloor == 0 {
		return errors.New("admissiongate: MemAvailableFloor must be positive")
	}
	seen := map[string]bool{}
	for _, sg := range c.Signals {
		if sg.Name == "" || seen[sg.Name] {
			return fmt.Errorf("admissiongate: signal name %q is empty or repeated", sg.Name)
		}
		seen[sg.Name] = true
		if !(sg.Close > 0) {
			return fmt.Errorf("admissiongate: signal %q needs a positive Close", sg.Name)
		}
	}
	return nil
}

// cond is one gating condition with hysteresis. closed latches on a breach.
// lowSince is when the value first entered the reopen band without leaving
// it since; zero when it is not in the band.
type cond struct {
	name     string
	reason   Reason
	closed   bool
	lowSince time.Time
}

// update applies one sample value. breach: the value is in the close band.
// calm: the value is in the reopen band. A value can be neither (between
// the two); that holds a closed gate closed and resets the clock.
func (c *cond) update(at time.Time, breach, calm bool, reopenAfter time.Duration, gap bool) {
	if gap {
		c.lowSince = time.Time{}
	}
	if !c.closed {
		if breach {
			c.closed = true
			c.lowSince = time.Time{}
		}
		return
	}
	if !calm {
		c.lowSince = time.Time{}
		return
	}
	if c.lowSince.IsZero() {
		c.lowSince = at
	}
	if at.Sub(c.lowSince) >= reopenAfter {
		c.closed = false
		c.lowSince = time.Time{}
	}
}

// Gate is the S3b state machine. It is not safe for concurrent use.
type Gate struct {
	cfg         Config
	have        bool
	last        time.Time
	signals     []cond
	mem         cond
	closedSince time.Time
}

// New returns a closed gate: with no sample the gate refuses. Call
// cfg.Validate first; an invalid config is the caller's bug.
func New(cfg Config) *Gate {
	g := &Gate{cfg: cfg, mem: cond{name: "mem_available", reason: ReasonMemAvailable}}
	for _, sg := range cfg.Signals {
		g.signals = append(g.signals, cond{name: sg.Name, reason: ReasonSignal})
	}
	return g
}

func (g *Gate) staleAfter() time.Duration { return 2 * g.cfg.Interval }

// memReopenLevel is F + max(R_default, 0.1 x F).
func (g *Gate) memReopenLevel() uint64 {
	margin := g.cfg.RDefault
	if tenth := g.cfg.MemAvailableFloor / 10; tenth > margin {
		margin = tenth
	}
	return g.cfg.MemAvailableFloor + margin
}

// Observe records a sample. A sample not newer than the latest one is
// dropped. A gap longer than two intervals since the previous sample
// restarts every reopen clock: calm that nobody measured is not calm.
func (g *Gate) Observe(s Sample) {
	if g.have && !s.At.After(g.last) {
		return
	}
	gap := g.have && s.At.Sub(g.last) > g.staleAfter()
	for i, sg := range g.cfg.Signals {
		v, ok := s.Signals[sg.Name]
		// A missing or NaN reading is neither calm nor safe: fail closed.
		breach := !ok || math.IsNaN(v) || v >= sg.Close
		calm := ok && !math.IsNaN(v) && v <= sg.Close/2
		g.signals[i].update(s.At, breach, calm, g.cfg.ReopenAfter, gap)
	}
	g.mem.update(s.At,
		s.MemAvailable < g.cfg.MemAvailableFloor,
		s.MemAvailable >= g.memReopenLevel(),
		g.cfg.ReopenAfter, gap)
	g.have = true
	g.last = s.At
}

// Evaluate returns the gate state at now. Conditions are checked in order:
// no sample, stale sample, each signal in config order, MemAvailable.
func (g *Gate) Evaluate(now time.Time) Status {
	st := g.status(now)
	if st.Open {
		g.closedSince = time.Time{}
	} else {
		if g.closedSince.IsZero() {
			g.closedSince = now
		}
		st.ClosedSince = g.closedSince
	}
	return st
}

func (g *Gate) status(now time.Time) Status {
	if !g.have {
		return Status{Reason: ReasonNoSample}
	}
	if now.Sub(g.last) > g.staleAfter() {
		return Status{Reason: ReasonStale}
	}
	for _, c := range g.signals {
		if c.closed {
			return Status{Reason: ReasonSignal, Detail: c.name}
		}
	}
	if g.mem.closed {
		return Status{Reason: ReasonMemAvailable, Detail: g.mem.name}
	}
	return Status{Open: true}
}
