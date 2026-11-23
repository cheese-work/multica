package oomwatch

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// beat writes one heartbeat at now and returns its nonce.
func (h *harness) beat(now time.Time) string {
	h.t.Helper()
	var nonce string
	err := Beat(context.Background(), h.mon, func(_ context.Context, _, m string) error {
		nonce, _ = ParseMessage(m)
		return nil
	}, now)
	if err != nil {
		h.t.Fatal(err)
	}
	return nonce
}

// --- heartbeat through the reader -------------------------------------------

func TestReaderHealthySilence(t *testing.T) {
	h := newHarness(t, "")
	seq := 1
	for i := 0; i < 4; i++ { // 2 minutes of heartbeats and nothing else
		now := t0.Add(time.Duration(i) * HeartbeatPeriod)
		n := h.beat(now)
		h.r.Process(hbEntry(seq, n), now.Add(time.Second))
		seq++
		for s := 1; s <= 30; s++ {
			h.r.Tick(now.Add(time.Duration(s) * time.Second))
		}
	}
	if len(h.holds) != 0 || len(h.gaps) != 0 || len(h.verdicts) != 0 {
		t.Fatalf("healthy silence must report nothing: holds %v gaps %v verdicts %v", h.holds, h.gaps, h.verdicts)
	}
}

func TestReaderStuckReaderHoldsWithin10s(t *testing.T) {
	h := newHarness(t, "")
	h.beat(t0) // the reader never sees it
	h.r.Tick(t0.Add(HeartbeatWindow - time.Nanosecond))
	if len(h.holds) != 0 {
		t.Fatalf("held early: %v", h.holds)
	}
	h.r.Tick(t0.Add(HeartbeatWindow))
	if got := holdCauses(h.holds); len(got) != 1 || got[0] != HoldHeartbeatMissed {
		t.Fatalf("want one heartbeat_missed hold at 10 s, got %v", got)
	}
	if len(h.gaps) != 1 || h.gaps[0].Reason != GapHeartbeatMissed {
		t.Fatalf("want a heartbeat_missed telemetry gap, got %v", h.gaps)
	}
	h.r.Tick(t0.Add(time.Minute))
	if len(h.holds) != 1 {
		t.Fatalf("the same nonce must not hold twice: %v", h.holds)
	}
}

func TestReaderLateNonceStillHoldsAtTheDeadline(t *testing.T) {
	h := newHarness(t, "")
	n := h.beat(t0)
	h.r.Tick(t0.Add(HeartbeatWindow)) // deadline passes first
	h.r.Process(hbEntry(1, n), t0.Add(11*time.Second))
	if len(h.holds) != 1 {
		t.Fatalf("a nonce seen after the window must not undo the hold: %v", h.holds)
	}
}

func TestReaderHeartbeatSpoofsDoNotCount(t *testing.T) {
	cases := map[string]func(Entry) Entry{
		"wrong uid":        func(e Entry) Entry { e.UID = "1001"; return e },
		"wrong identifier": func(e Entry) Entry { e.Identifier = "other"; return e },
		"wrong boot":       func(e Entry) Entry { e.BootID = "ffffffffffffffffffffffffffffffff"; return e },
		"kernel transport": func(e Entry) Entry { e.Transport = "kernel"; return e },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "")
			n := h.beat(t0)
			h.r.Process(mutate(hbEntry(1, n)), t0.Add(time.Second))
			h.r.Tick(t0.Add(HeartbeatWindow))
			if got := holdCauses(h.holds); len(got) != 1 || got[0] != HoldHeartbeatMissed {
				t.Fatalf("a spoofed heartbeat must not satisfy the nonce: %v", got)
			}
		})
	}
}

func TestReaderUnknownAndRepeatedNonceAreLoggedNotTriggers(t *testing.T) {
	h := newHarness(t, "")
	n := h.beat(t0)
	h.r.Process(hbEntry(1, n), t0.Add(time.Second))
	h.r.Process(hbEntry(2, n), t0.Add(2*time.Second))                                  // repeat
	h.r.Process(hbEntry(3, "00000000000000000000000000000000"), t0.Add(3*time.Second)) // never sent
	if len(h.holds) != 0 || len(h.gaps) != 0 {
		t.Fatalf("holds %v gaps %v", h.holds, h.gaps)
	}
	if len(h.ignored) != 2 {
		t.Fatalf("want two ignored entries, got %v", h.ignored)
	}
}

func TestReaderBeatWriteFailureHolds(t *testing.T) {
	h := newHarness(t, "")
	_ = Beat(context.Background(), h.mon, func(context.Context, string, string) error { return io.ErrClosedPipe }, t0)
	h.r.Tick(t0.Add(HeartbeatWindow))
	if got := holdCauses(h.holds); len(got) != 1 || got[0] != HoldHeartbeatMissed {
		t.Fatalf("got %v", got)
	}
}

// --- history, other boots, other sources -------------------------------------

func TestReaderReplayedHistoryAndOtherBootDoNotTrigger(t *testing.T) {
	h := newHarness(t, cursorFor(10))
	// a host-wide kill at or before the cursor: history
	history := []string{hdr("python3"), con("CONSTRAINT_NONE", "", docker, "python3", 50), killGlobal(50)}
	for i, l := range history {
		h.r.Process(kernelEntry(8+i, l), t0) // seq 8, 9, 10
	}
	// the same kill from another boot
	for i, l := range history {
		e := kernelEntry(20+i, l)
		e.BootID = "ffffffffffffffffffffffffffffffff"
		h.r.Process(e, t0)
	}
	if len(h.holds) != 0 || len(h.verdicts) != 0 {
		t.Fatalf("history and other boots must not trigger: holds %v verdicts %v", h.holds, h.verdicts)
	}
	if len(h.ignored) != 6 {
		t.Fatalf("each ignored entry is logged, got %v", h.ignored)
	}
	if h.r.Cursor() != cursorFor(10) {
		t.Fatalf("history must not move the cursor, got %s", h.r.Cursor())
	}
	// a record after the cursor does trigger
	h.feed(11, hdr("python3"), con("CONSTRAINT_NONE", "", docker, "python3", 51), killGlobal(51))
	if got := holdCauses(h.holds); len(got) != 1 || got[0] != HoldConstraintNotMemcg {
		t.Fatalf("got %v", got)
	}
	if h.r.Cursor() != cursorFor(13) {
		t.Fatalf("cursor must follow the last entry, got %s", h.r.Cursor())
	}
}

func TestReaderIgnoresKilledProcessTextFromUserspace(t *testing.T) {
	h := newHarness(t, "")
	for i, l := range []string{hdr("a"), memcgCon(svc+"/runs/x", 60), kill(60)} {
		e := kernelEntry(1+i, l)
		e.Transport, e.Identifier, e.UID = "syslog", "someone", "1000"
		h.r.Process(e, t0)
	}
	if len(h.holds) != 0 || len(h.verdicts) != 0 {
		t.Fatalf("only the kernel transport feeds the classifier: %v %v", h.holds, h.verdicts)
	}
}

func TestReaderHeartbeatBetweenConstraintAndRecordKeepsTheJoin(t *testing.T) {
	h := newHarness(t, "")
	n := h.beat(t0)
	h.r.Process(kernelEntry(1, hdr("python3")), t0)
	h.r.Process(kernelEntry(2, memcgCon(svc+"/runs/x", 61)), t0)
	h.r.Process(hbEntry(3, n), t0)
	h.r.Process(kernelEntry(4, kill(61)), t0)
	if got := classes(h.verdicts); !sameStrings(got, []string{"contained"}) || len(h.holds) != 0 {
		t.Fatalf("verdicts %v holds %v", got, h.holds)
	}
}

func TestReaderVerdictsAndHoldsAreSeparateSinks(t *testing.T) {
	h := newHarness(t, "")
	h.feed(1, hdr("a"), memcgCon(docker, 62), kill(62), hdr("b"), con("CONSTRAINT_NONE", "", docker, "b", 63), killGlobal(63))
	if got := classes(h.verdicts); !sameStrings(got, []string{"foreign_memcg", "hold:constraint_not_memcg"}) {
		t.Fatalf("verdicts %v", got)
	}
	if got := holdCauses(h.holds); len(got) != 1 || got[0] != HoldConstraintNotMemcg {
		t.Fatalf("a foreign_memcg kill must not request a hold; holds %v", got)
	}
}

// --- fixtures ----------------------------------------------------------------

// The redacted replay of the X99 boot 77e6ee2a records read at 19:24 ICT on
// 18 Nov 2026: 129 kills, 5 CONSTRAINT_NONE and 124 CONSTRAINT_MEMCG outside
// multica.service. Expected: exactly 5 holds and 124 foreign_memcg.
func TestRedacted129RecordReplay(t *testing.T) {
	entries := loadFixture(t, "boot-oom-129.jsonl")
	h := newHarness(t, "")
	for _, e := range entries {
		h.r.Process(e, t0)
	}
	var contained, foreign, holds int
	for _, v := range h.verdicts {
		switch {
		case v.Hold:
			holds++
			if v.Cause != HoldConstraintNotMemcg || v.Constraint != "CONSTRAINT_NONE" {
				t.Errorf("unexpected hold %+v", v)
			}
		case v.Class == ClassContained:
			contained++
		case v.Class == ClassForeignMemcg:
			foreign++
		}
	}
	if len(h.verdicts) != 129 || holds != 5 || foreign != 124 || contained != 0 {
		t.Fatalf("verdicts %d holds %d foreign %d contained %d", len(h.verdicts), holds, foreign, contained)
	}
	if len(h.holds) != 5 {
		t.Fatalf("hold requests %d, want 5", len(h.holds))
	}
	if len(h.ignored) != 0 || len(h.gaps) != 0 {
		t.Fatalf("ignored %v gaps %v", h.ignored, h.gaps)
	}
}

// Step 2 capture (CHE-7231): a single-victim contained OOM in a MiB-scale leaf.
const captureService = "/user.slice/user-1000.slice/user@1000.service/app.slice/s1t-che7231-test.service"

func captureHarness(t *testing.T) (*harness, int) {
	t.Helper()
	h := newHarnessSvc(t, "", captureService)
	entries := loadFixture(t, "oom-single-victim-contained.jsonl")
	for _, e := range entries {
		h.r.Process(e, t0)
	}
	return h, len(entries) + 1
}

func TestCaptureSingleVictimContained(t *testing.T) {
	h, _ := captureHarness(t)
	if got := classes(h.verdicts); !sameStrings(got, []string{"contained"}) || len(h.holds) != 0 {
		t.Fatalf("verdicts %v holds %v", got, h.holds)
	}
	v := h.verdicts[0]
	if v.PID != 93693 || v.OOMMemcg != captureService+"/runs/bomb" || v.Constraint != "CONSTRAINT_MEMCG" {
		t.Fatalf("got %+v", v)
	}
}

func TestCaptureJoinCases(t *testing.T) {
	t.Run("Sol r8: a headerless record within 2 s of the contained one holds", func(t *testing.T) {
		h, next := captureHarness(t)
		h.feed(next, "Memory cgroup out of memory: Killed process 93700 (python3) total-vm:282604kB, anon-rss:32596kB, file-rss:6396kB, shmem-rss:0kB, UID:1000 pgtables:144kB oom_score_adj:200")
		if got := classes(h.verdicts); !sameStrings(got, []string{"contained", "hold:constraint_rejoined"}) {
			t.Fatalf("verdicts %v", got)
		}
		if len(h.holds) != 1 {
			t.Fatalf("holds %v", h.holds)
		}
	})
	t.Run("a header-only headerless record (rate-limited constraint line) holds", func(t *testing.T) {
		h, next := captureHarness(t)
		h.feed(next, hdr("python3"), kill(93701))
		if got := classes(h.verdicts); !sameStrings(got, []string{"contained", "hold:no_constraint_line"}) {
			t.Fatalf("verdicts %v", got)
		}
	})
	t.Run("a constraint line followed by a record with another pid holds", func(t *testing.T) {
		h, next := captureHarness(t)
		h.feed(next, hdr("python3"), memcgCon(captureService+"/runs/bomb", 93702), kill(93703))
		if got := classes(h.verdicts); !sameStrings(got, []string{"contained", "hold:constraint_pid_mismatch"}) {
			t.Fatalf("verdicts %v", got)
		}
	})
	t.Run("two records after one constraint line: the second holds", func(t *testing.T) {
		h, next := captureHarness(t)
		h.feed(next, hdr("python3"), memcgCon(captureService+"/runs/bomb", 93704), kill(93704), kill(93704))
		if got := classes(h.verdicts); !sameStrings(got, []string{"contained", "contained", "hold:constraint_rejoined"}) {
			t.Fatalf("verdicts %v", got)
		}
	})
	t.Run("a memory.oom.group line in a leaf holds", func(t *testing.T) {
		h, next := captureHarness(t)
		h.feed(next, groupLine(captureService+"/runs/bomb"))
		if got := holdCauses(h.holds); len(got) != 1 || got[0] != HoldOOMGroup {
			t.Fatalf("holds %v", got)
		}
	})
	t.Run("a memory.oom.group line in a docker scope holds", func(t *testing.T) {
		h, next := captureHarness(t)
		h.feed(next, groupLine(docker))
		if got := holdCauses(h.holds); len(got) != 1 || got[0] != HoldOOMGroup {
			t.Fatalf("holds %v", got)
		}
	})
	t.Run("a foreign memcg OOM with its own constraint line right after the contained one is foreign_memcg", func(t *testing.T) {
		h, next := captureHarness(t)
		h.feed(next, hdr("buildkit"), memcgCon(docker, 93705), kill(93705))
		if got := classes(h.verdicts); !sameStrings(got, []string{"contained", "foreign_memcg"}) || len(h.holds) != 0 {
			t.Fatalf("verdicts %v holds %v", got, h.holds)
		}
	})
}

// --- the JSON stream ---------------------------------------------------------

func TestReaderRunProcessesLinesAndFailsClosedOnGarbage(t *testing.T) {
	h := newHarness(t, "")
	mk := func(seq int, msg string) string {
		return `{"__CURSOR":"` + cursorFor(seq) + `","_BOOT_ID":"` + testBoot + `","_TRANSPORT":"kernel","SYSLOG_IDENTIFIER":"kernel","MESSAGE":"` + strings.ReplaceAll(msg, `"`, `\"`) + `"}`
	}
	stream := strings.Join([]string{
		mk(1, hdr("a")),
		mk(2, memcgCon(docker, 70)),
		mk(3, kill(70)),
		`{ this is not json`,
		mk(4, hdr("b")),
		mk(5, con("CONSTRAINT_NONE", "", docker, "b", 71)),
		mk(6, killGlobal(71)),
	}, "\n") + "\n"
	if err := h.r.Run(strings.NewReader(stream), func() time.Time { return t0 }); err != nil {
		t.Fatal(err)
	}
	if got := classes(h.verdicts); !sameStrings(got, []string{"foreign_memcg", "hold:constraint_not_memcg"}) {
		t.Fatalf("verdicts %v", got)
	}
	causes := holdCauses(h.holds)
	if len(causes) != 2 || causes[0] != HoldUnparsable || causes[1] != HoldConstraintNotMemcg {
		t.Fatalf("an unparsable line must latch the hold and the stream must go on: %v", causes)
	}
	if h.r.Cursor() != cursorFor(6) {
		t.Fatalf("cursor %s", h.r.Cursor())
	}
}

func TestReaderRunHandlesAVeryLongLine(t *testing.T) {
	h := newHarness(t, "")
	long := strings.Repeat("x", 3<<20)
	line := `{"__CURSOR":"` + cursorFor(1) + `","_BOOT_ID":"` + testBoot + `","_TRANSPORT":"kernel","MESSAGE":"` + long + `"}` + "\n"
	if err := h.r.Run(strings.NewReader(line), func() time.Time { return t0 }); err != nil {
		t.Fatal(err)
	}
	if len(h.holds) != 0 {
		t.Fatalf("a long ordinary kernel line is not a trigger: %v", h.holds)
	}
}

// The resume gap is the interval from the last persisted heartbeat to the first
// heartbeat the new reader sees (plan r13 section 3.5), not the restart time.
func TestReaderResumeGapIsMeasuredToTheFirstNewHeartbeat(t *testing.T) {
	cases := []struct {
		name    string
		firstAt time.Duration // first new heartbeat, after the last persisted one
		gap     bool
	}{
		{"restart at 59 s, first heartbeat at 61 s", 61 * time.Second, true},
		{"exactly 60 s", 60 * time.Second, false},
		{"just over 60 s", 60*time.Second + time.Nanosecond, true},
		{"first heartbeat at 59 s", 59 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessResume(t, cursorFor(40), svc, t0)
			// the restart itself took 59 s: nothing is decided yet
			h.r.Tick(t0.Add(59 * time.Second))
			if len(h.gaps) != 0 {
				t.Fatalf("a gap before the first heartbeat: %v", h.gaps)
			}
			at := t0.Add(tc.firstAt)
			h.r.Process(hbEntry(41, h.beat(at)), at)
			if tc.gap {
				if len(h.gaps) != 1 || h.gaps[0].Reason != GapResume || !h.gaps[0].From.Equal(t0) || !h.gaps[0].To.Equal(at) {
					t.Fatalf("want one resume_gap %v to %v, got %+v", t0, at, h.gaps)
				}
			} else if len(h.gaps) != 0 {
				t.Fatalf("unexpected gap %+v", h.gaps)
			}
			if len(h.holds) != 0 {
				t.Fatalf("a resume gap is telemetry, not a hold: %v", h.holds)
			}
			// only the first heartbeat is measured
			later := at.Add(time.Hour)
			h.r.Process(hbEntry(42, h.beat(later)), later)
			if want := map[bool]int{true: 1, false: 0}[tc.gap]; len(h.gaps) != want {
				t.Fatalf("gaps after a later heartbeat: %d, want %d", len(h.gaps), want)
			}
		})
	}
}

func TestReaderWithoutResumeBoundaryEmitsNoResumeGap(t *testing.T) {
	h := newHarness(t, "")
	h.r.Process(hbEntry(1, h.beat(t0.Add(time.Hour))), t0.Add(time.Hour))
	if len(h.gaps) != 0 {
		t.Fatalf("unexpected gap %+v", h.gaps)
	}
}
