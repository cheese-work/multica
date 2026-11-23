package oomwatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func cp(boot string, at time.Time) *Checkpoint {
	return &Checkpoint{BootID: boot, Cursor: cursorFor(40), HeartbeatNonce: "0123456789abcdef0123456789abcdef", HeartbeatAt: at}
}

func TestPlanStart(t *testing.T) {
	const otherBoot = "ffffffffffffffffffffffffffffffff"
	now := t0.Add(time.Hour)
	tests := []struct {
		name        string
		in          StartInput
		mode        StartMode
		cursor      string
		gap         GapReason // "" means no gap
		hold        HoldCause // "" means no hold
		restoreCtrs bool
		resumeFrom  bool // the plan carries the last heartbeat time to the reader
	}{
		{
			name: "first start: tail, nothing to restore, no gap",
			in:   StartInput{Boot: testBoot, Now: now},
			mode: StartTail,
		},
		{
			name: "torn checkpoint: tail, gap and hold",
			in:   StartInput{Boot: testBoot, CheckpointTorn: true, Now: now},
			mode: StartTail, gap: GapCheckpointTorn, hold: HoldCheckpointTorn,
		},
		{
			name: "boot change: tail from the new boot, gap, no counter restore",
			in:   StartInput{Boot: testBoot, Checkpoint: cp(otherBoot, now.Add(-time.Minute)), CursorValid: true, Now: now},
			mode: StartTail, gap: GapBootChange,
		},
		{
			name: "same boot, valid cursor, short downtime: resume, no gap yet",
			in:   StartInput{Boot: testBoot, Checkpoint: cp(testBoot, now.Add(-60*time.Second)), CursorValid: true, Now: now},
			mode: StartResume, cursor: cursorFor(40), restoreCtrs: true, resumeFrom: true,
		},
		{
			// The gap is measured to the first new heartbeat, so the start
			// decides nothing even when the restart alone took over 60 s.
			name: "same boot, valid cursor, long downtime: resume, the reader decides the gap",
			in:   StartInput{Boot: testBoot, Checkpoint: cp(testBoot, now.Add(-61*time.Second)), CursorValid: true, Now: now},
			mode: StartResume, cursor: cursorFor(40), restoreCtrs: true, resumeFrom: true,
		},
		{
			name: "same boot, invalid cursor: tail, gap and live_gap hold, no counter restore",
			in:   StartInput{Boot: testBoot, Checkpoint: cp(testBoot, now.Add(-time.Minute)), CursorValid: false, Now: now},
			mode: StartTail, gap: GapCursorInvalid, hold: HoldLiveGap,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := PlanStart(tc.in)
			if p.Mode != tc.mode || p.Cursor != tc.cursor || p.RestoreCounters != tc.restoreCtrs {
				t.Fatalf("mode %q cursor %q restore %v; want %q %q %v", p.Mode, p.Cursor, p.RestoreCounters, tc.mode, tc.cursor, tc.restoreCtrs)
			}
			if got := !p.ResumeFrom.IsZero(); got != tc.resumeFrom {
				t.Fatalf("ResumeFrom %v, want carried=%v", p.ResumeFrom, tc.resumeFrom)
			}
			if tc.resumeFrom && !p.ResumeFrom.Equal(tc.in.Checkpoint.HeartbeatAt) {
				t.Fatalf("ResumeFrom %v, want the checkpoint heartbeat %v", p.ResumeFrom, tc.in.Checkpoint.HeartbeatAt)
			}
			switch {
			case tc.gap == "" && p.Gap != nil:
				t.Fatalf("unexpected gap %+v", p.Gap)
			case tc.gap != "" && (p.Gap == nil || p.Gap.Reason != tc.gap):
				t.Fatalf("gap %+v, want %s", p.Gap, tc.gap)
			}
			switch {
			case tc.hold == "" && p.Hold != nil:
				t.Fatalf("unexpected hold %+v", p.Hold)
			case tc.hold != "" && (p.Hold == nil || p.Hold.Cause != tc.hold):
				t.Fatalf("hold %+v, want %s", p.Hold, tc.hold)
			}
		})
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oomwatch.json")
	want := Checkpoint{BootID: testBoot, Cursor: cursorFor(7), HeartbeatNonce: "00112233445566778899aabbccddeeff", HeartbeatAt: t0}
	if err := WriteCheckpoint(path, want); err != nil {
		t.Fatal(err)
	}
	got, torn, err := ReadCheckpoint(path)
	if err != nil || torn || got == nil {
		t.Fatalf("got %v torn %v err %v", got, torn, err)
	}
	if got.BootID != want.BootID || got.Cursor != want.Cursor || got.HeartbeatNonce != want.HeartbeatNonce || !got.HeartbeatAt.Equal(want.HeartbeatAt) {
		t.Fatalf("got %+v want %+v", *got, want)
	}
	// overwrite replaces whole
	want.Cursor = cursorFor(8)
	if err := WriteCheckpoint(path, want); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := ReadCheckpoint(path); got == nil || got.Cursor != cursorFor(8) {
		t.Fatalf("overwrite lost: %+v", got)
	}
}

func TestCheckpointMissingIsNotTorn(t *testing.T) {
	got, torn, err := ReadCheckpoint(filepath.Join(t.TempDir(), "absent.json"))
	if got != nil || torn || err != nil {
		t.Fatalf("got %v torn %v err %v", got, torn, err)
	}
}

func TestCheckpointTornFiles(t *testing.T) {
	dir := t.TempDir()
	good := Checkpoint{BootID: testBoot, Cursor: cursorFor(7), HeartbeatNonce: "00112233445566778899aabbccddeeff", HeartbeatAt: t0}
	goodPath := filepath.Join(dir, "good.json")
	if err := WriteCheckpoint(goodPath, good); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(goodPath)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"truncated":      raw[:len(raw)/2],
		"empty":          {},
		"garbage":        []byte("\x00\x01\x02 not json"),
		"missing boot":   []byte(`{"cursor":"s=a;i=1;b=77e6ee2acacf45389e0ad628f9a2d460;m=1;t=0;x=0"}`),
		"missing cursor": []byte(`{"boot_id":"77e6ee2acacf45389e0ad628f9a2d460"}`),
		"trailing junk":  append(append([]byte{}, raw...), []byte("{}")...),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			got, torn, err := ReadCheckpoint(path)
			if got != nil || !torn || err != nil {
				t.Fatalf("got %v torn %v err %v", got, torn, err)
			}
		})
	}
}

func TestCheckpointWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oomwatch.json")
	for i := 0; i < 3; i++ {
		if err := WriteCheckpoint(path, Checkpoint{BootID: testBoot, Cursor: cursorFor(i + 1), HeartbeatAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "oomwatch.json" {
		t.Fatalf("directory holds %v, want only oomwatch.json", ents)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
}

func TestCheckpointWriteFailsWhenDirectoryIsMissing(t *testing.T) {
	err := WriteCheckpoint(filepath.Join(t.TempDir(), "nope", "oomwatch.json"), Checkpoint{BootID: testBoot, Cursor: cursorFor(1)})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
}
