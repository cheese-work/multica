package admissiongate

import (
	"testing"
	"time"
)

const gib = uint64(1) << 30

var t0 = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

func testCfg() Config {
	return Config{
		Interval:    10 * time.Second,
		ReopenAfter: 120 * time.Second,
		Signals: []Signal{
			{Name: "mem_psi", Close: 5},
			{Name: "io_psi", Close: 20},
		},
		MemAvailableFloor: 20 * gib,
		RDefault:          4 * gib,
	}
}

// at returns t0 + d.
func at(d time.Duration) time.Time { return t0.Add(d) }

func sample(d time.Duration, mem, io float64, avail uint64) Sample {
	return Sample{
		At:           at(d),
		Signals:      map[string]float64{"mem_psi": mem, "io_psi": io},
		MemAvailable: avail,
	}
}

// feed observes one sample and evaluates at the sample time.
func feed(g *Gate, s Sample) Status {
	g.Observe(s)
	return g.Evaluate(s.At)
}

// hold feeds one sample every 10 s from..to inclusive and returns the last
// status. Reopen clocks need an unbroken run of samples.
func hold(g *Gate, from, to time.Duration, mem, io float64, avail uint64) Status {
	var st Status
	for d := from; d <= to; d += 10 * time.Second {
		st = feed(g, sample(d, mem, io, avail))
	}
	return st
}

func wantClosed(t *testing.T, st Status, r Reason) {
	t.Helper()
	if st.Open || st.Reason != r {
		t.Fatalf("want closed(%q), got open=%v reason=%q detail=%q", r, st.Open, st.Reason, st.Detail)
	}
}

func wantOpen(t *testing.T, st Status) {
	t.Helper()
	if !st.Open || st.Reason != ReasonNone {
		t.Fatalf("want open, got open=%v reason=%q detail=%q", st.Open, st.Reason, st.Detail)
	}
}

func TestNoSampleClosesGate(t *testing.T) {
	g := New(testCfg())
	wantClosed(t, g.Evaluate(t0), ReasonNoSample)
}

func TestFreshCleanSampleOpensGate(t *testing.T) {
	g := New(testCfg())
	wantOpen(t, feed(g, sample(0, 0.1, 1, 30*gib)))
}

func TestSignalClosesAtThreshold(t *testing.T) {
	g := New(testCfg())
	wantOpen(t, feed(g, sample(0, 4.9, 1, 30*gib)))
	st := feed(g, sample(10*time.Second, 5, 1, 30*gib)) // exactly T closes
	wantClosed(t, st, ReasonSignal)
	if st.Detail != "mem_psi" {
		t.Fatalf("detail = %q, want mem_psi", st.Detail)
	}
	if !st.ClosedSince.Equal(at(10 * time.Second)) {
		t.Fatalf("ClosedSince = %v", st.ClosedSince)
	}
}

func TestSignalHysteresisReopen(t *testing.T) {
	g := New(testCfg())
	feed(g, sample(0, 6, 1, 30*gib)) // closed

	// Between T/2 and T: stays closed however long it lasts.
	wantClosed(t, hold(g, 10*time.Second, 300*time.Second, 3, 1, 30*gib), ReasonSignal)

	// At T/2 or below starts the 120 s clock; still closed before 120 s.
	base := 310 * time.Second
	wantClosed(t, hold(g, base, base+110*time.Second, 2.5, 1, 30*gib), ReasonSignal)
	// 120 s after the first low sample: open.
	wantOpen(t, feed(g, sample(base+120*time.Second, 2.5, 1, 30*gib)))
	// T/2 itself counts as low.
	g2 := New(testCfg())
	feed(g2, sample(0, 6, 1, 30*gib))
	wantOpen(t, hold(g2, 10*time.Second, 130*time.Second, 2.5, 1, 30*gib))
}

func TestSignalReopenClockResetsOnRelapse(t *testing.T) {
	g := New(testCfg())
	feed(g, sample(0, 6, 1, 30*gib))
	hold(g, 10*time.Second, 100*time.Second, 1, 1, 30*gib) // low clock starts at 10 s
	// A sample back above T/2 at 110 s resets the clock.
	wantClosed(t, feed(g, sample(110*time.Second, 3, 1, 30*gib)), ReasonSignal)
	hold(g, 120*time.Second, 220*time.Second, 1, 1, 30*gib) // new clock starts at 120 s
	wantClosed(t, feed(g, sample(230*time.Second, 1, 1, 30*gib)), ReasonSignal)
	wantOpen(t, feed(g, sample(240*time.Second, 1, 1, 30*gib)))
}

func TestEachSignalGatesIndependently(t *testing.T) {
	g := New(testCfg())
	st := feed(g, sample(0, 0, 25, 30*gib))
	wantClosed(t, st, ReasonSignal)
	if st.Detail != "io_psi" {
		t.Fatalf("detail = %q, want io_psi", st.Detail)
	}
	// io_psi recovers (<= 10) but mem_psi is now high: stays closed on mem_psi.
	st = hold(g, 10*time.Second, 140*time.Second, 6, 5, 30*gib)
	wantClosed(t, st, ReasonSignal)
	if st.Detail != "mem_psi" {
		t.Fatalf("detail = %q, want mem_psi", st.Detail)
	}
}

func TestStaleSampleClosesGate(t *testing.T) {
	g := New(testCfg())
	wantOpen(t, feed(g, sample(0, 0, 0, 30*gib)))
	// Exactly two intervals old: still fresh.
	wantOpen(t, g.Evaluate(at(20*time.Second)))
	// Older than two intervals: closed.
	wantClosed(t, g.Evaluate(at(20*time.Second+time.Millisecond)), ReasonStale)
	// A new fresh clean sample reopens (staleness is not latched).
	wantOpen(t, feed(g, sample(60*time.Second, 0, 0, 30*gib)))
}

func TestGapInSamplesResetsReopenClock(t *testing.T) {
	g := New(testCfg())
	feed(g, sample(0, 6, 1, 30*gib))
	hold(g, 10*time.Second, 60*time.Second, 1, 1, 30*gib) // low clock starts
	// Samples vanish for 200 s (> 2 intervals), then a low sample arrives.
	// The 120 s of "calm" cannot be vouched for across the gap.
	wantClosed(t, feed(g, sample(260*time.Second, 1, 1, 30*gib)), ReasonSignal)
	wantClosed(t, hold(g, 270*time.Second, 370*time.Second, 1, 1, 30*gib), ReasonSignal)
	wantOpen(t, feed(g, sample(380*time.Second, 1, 1, 30*gib)))
}

func TestMemAvailableClosesBelowFloor(t *testing.T) {
	g := New(testCfg())
	wantOpen(t, feed(g, sample(0, 0, 0, 20*gib))) // at F is not below F
	wantClosed(t, feed(g, sample(10*time.Second, 0, 0, 20*gib-1)), ReasonMemAvailable)
}

func TestMemAvailableReopenMargin(t *testing.T) {
	g := New(testCfg())
	feed(g, sample(0, 0, 0, 10*gib)) // closed
	// Reopen level = F + max(R_default=4, 0.1*F=2) = 24 GiB.
	reopen := 24 * gib
	// Back above F but below the reopen level: stays closed forever.
	wantClosed(t, hold(g, 10*time.Second, 300*time.Second, 0, 0, reopen-1), ReasonMemAvailable)
	// At the reopen level: clock starts, open only after 120 s.
	base := 310 * time.Second
	wantClosed(t, hold(g, base, base+110*time.Second, 0, 0, reopen), ReasonMemAvailable)
	wantOpen(t, feed(g, sample(base+120*time.Second, 0, 0, reopen)))
}

func TestMemAvailableMarginUsesTenPercentWhenLarger(t *testing.T) {
	cfg := testCfg()
	cfg.MemAvailableFloor = 100 * gib
	cfg.RDefault = 4 * gib // 0.1*F = 10 GiB wins; reopen level 110 GiB
	g := New(cfg)
	feed(g, sample(0, 0, 0, 50*gib))
	wantClosed(t, hold(g, 10*time.Second, 200*time.Second, 0, 0, 110*gib-1), ReasonMemAvailable)
	wantClosed(t, hold(g, 210*time.Second, 320*time.Second, 0, 0, 110*gib), ReasonMemAvailable)
	wantOpen(t, feed(g, sample(330*time.Second, 0, 0, 110*gib)))
}

func TestMemAvailableReopenClockResetsOnRelapse(t *testing.T) {
	g := New(testCfg())
	feed(g, sample(0, 0, 0, 10*gib))
	hold(g, 10*time.Second, 100*time.Second, 0, 0, 30*gib)  // clock starts at 10 s
	feed(g, sample(110*time.Second, 0, 0, 23*gib))          // dips under reopen level: reset
	hold(g, 120*time.Second, 230*time.Second, 0, 0, 30*gib) // new clock at 120 s
	wantClosed(t, feed(g, sample(230*time.Second, 0, 0, 30*gib)), ReasonMemAvailable)
	wantOpen(t, feed(g, sample(240*time.Second, 0, 0, 30*gib)))
}

func TestMissingConfiguredSignalFailsClosed(t *testing.T) {
	g := New(testCfg())
	s := Sample{At: at(0), Signals: map[string]float64{"mem_psi": 0}, MemAvailable: 30 * gib}
	st := feed(g, s)
	wantClosed(t, st, ReasonSignal)
	if st.Detail != "io_psi" {
		t.Fatalf("detail = %q, want io_psi", st.Detail)
	}
}

func TestOutOfOrderSampleIgnored(t *testing.T) {
	g := New(testCfg())
	feed(g, sample(10*time.Second, 0, 0, 30*gib))
	g.Observe(sample(0, 9, 0, 30*gib)) // older than the latest: dropped
	wantOpen(t, g.Evaluate(at(10*time.Second)))
}

func TestClosedSinceKeepsFirstCloseAcrossReasons(t *testing.T) {
	g := New(testCfg())
	feed(g, sample(0, 0, 0, 30*gib))
	feed(g, sample(10*time.Second, 6, 0, 30*gib))
	st := feed(g, sample(20*time.Second, 6, 0, 10*gib))
	if !st.ClosedSince.Equal(at(10 * time.Second)) {
		t.Fatalf("ClosedSince = %v, want first close at 10s", st.ClosedSince)
	}
}

func TestValidate(t *testing.T) {
	if err := testCfg().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := map[string]func(*Config){
		"interval":  func(c *Config) { c.Interval = 0 },
		"reopen":    func(c *Config) { c.ReopenAfter = 0 },
		"floor":     func(c *Config) { c.MemAvailableFloor = 0 },
		"zero T":    func(c *Config) { c.Signals[0].Close = 0 },
		"empty":     func(c *Config) { c.Signals[0].Name = "" },
		"duplicate": func(c *Config) { c.Signals[1].Name = c.Signals[0].Name },
	}
	for name, mut := range bad {
		c := testCfg()
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestNaNSignalFailsClosed(t *testing.T) {
	g := New(testCfg())
	nan := 0.0
	nan = nan / nan
	wantClosed(t, feed(g, sample(0, nan, 0, 30*gib)), ReasonSignal)
}
