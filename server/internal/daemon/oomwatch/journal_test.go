package oomwatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func jsonEntry(seq int, boot string) string {
	return `{"__CURSOR":"` + cursorFor(seq) + `","_BOOT_ID":"` + boot + `","_TRANSPORT":"kernel","MESSAGE":"x"}` + "\n"
}

func TestTailCursor(t *testing.T) {
	var gotName string
	var gotArgs []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return []byte(jsonEntry(9, testBoot)), nil
	}
	c, err := TailCursor(context.Background(), run, "/bin/journalctl", testBoot)
	if err != nil || c != cursorFor(9) {
		t.Fatalf("got %q, %v", c, err)
	}
	want := []string{"--no-pager", "--output=json", "-n", "1", "_BOOT_ID=" + testBoot}
	if gotName != "/bin/journalctl" || !sameStrings(gotArgs, want) {
		t.Fatalf("ran %s %q", gotName, gotArgs)
	}
}

func TestTailCursorFailsClosed(t *testing.T) {
	ok := func(out string, err error) Runner {
		return func(context.Context, string, ...string) ([]byte, error) { return []byte(out), err }
	}
	for name, run := range map[string]Runner{
		"run error":      ok("", errors.New("boom")),
		"no output":      ok("", nil),
		"not json":       ok("garbage\n", nil),
		"other boot":     ok(jsonEntry(1, "ffffffffffffffffffffffffffffffff"), nil),
		"no cursor":      ok(`{"_BOOT_ID":"`+testBoot+`"}`+"\n", nil),
		"unparsable cur": ok(`{"__CURSOR":"x","_BOOT_ID":"`+testBoot+`"}`+"\n", nil),
	} {
		if c, err := TailCursor(context.Background(), run, "j", testBoot); err == nil || c != "" {
			t.Errorf("%s: got %q, %v", name, c, err)
		}
	}
	if _, err := TailCursor(context.Background(), ok("", nil), "j", "not-a-boot"); err == nil {
		t.Error("bad boot must be refused before running anything")
	}
}

func TestCursorValid(t *testing.T) {
	var gotArgs []string
	reply := func(out string, err error) Runner {
		return func(_ context.Context, _ string, args ...string) ([]byte, error) {
			gotArgs = args
			return []byte(out), err
		}
	}
	// journalctl returns the entry itself: valid
	v, err := CursorValid(context.Background(), reply(jsonEntry(5, testBoot), nil), "j", testBoot, cursorFor(5))
	if err != nil || !v {
		t.Fatalf("valid cursor: %v, %v", v, err)
	}
	want := []string{"--no-pager", "--output=json", "-n", "1", "--cursor=" + cursorFor(5), "_BOOT_ID=" + testBoot}
	if !sameStrings(gotArgs, want) {
		t.Fatalf("ran %q", gotArgs)
	}
	// journalctl seeked to the nearest entry: the cursor is gone
	if v, err := CursorValid(context.Background(), reply(jsonEntry(99, testBoot), nil), "j", testBoot, cursorFor(5)); err != nil || v {
		t.Fatalf("rotated-away cursor: %v, %v", v, err)
	}
	// a cursor past the end of the journal prints nothing: not valid, not an error
	if v, err := CursorValid(context.Background(), reply("", nil), "j", testBoot, cursorFor(5)); err != nil || v {
		t.Fatalf("empty answer: %v, %v", v, err)
	}
	// "Failed to seek to cursor": an error, never valid
	if v, err := CursorValid(context.Background(), reply("", errors.New("exit 1")), "j", testBoot, cursorFor(5)); err == nil || v {
		t.Fatalf("seek failure: %v, %v", v, err)
	}
	// nothing is run for input that is not a cursor or a boot
	if v, err := CursorValid(context.Background(), reply("", nil), "j", testBoot, "--output=cat"); err == nil || v {
		t.Fatalf("bad cursor: %v, %v", v, err)
	}
	if v, err := CursorValid(context.Background(), reply("", nil), "j", "x", cursorFor(5)); err == nil || v {
		t.Fatalf("bad boot: %v, %v", v, err)
	}
}

// fakeJournalctl writes a script that records its arguments, then prints the
// given file or hangs.
func fakeJournalctl(t *testing.T, body string) (script, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script = filepath.Join(dir, "journalctl")
	src := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n" + body
	if err := os.WriteFile(script, []byte(src), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, argsFile
}

func TestFollowFeedsTheReaderAndReportsAnExit(t *testing.T) {
	h := newHarness(t, "")
	data := filepath.Join(t.TempDir(), "stream")
	stream := jsonEntry(1, testBoot)
	stream = strings.ReplaceAll(stream, `"MESSAGE":"x"`, `"MESSAGE":"`+hdr("a")+`"`) +
		strings.ReplaceAll(jsonEntry(2, testBoot), `"MESSAGE":"x"`, `"MESSAGE":"`+memcgCon(docker, 80)+`"`) +
		strings.ReplaceAll(jsonEntry(3, testBoot), `"MESSAGE":"x"`, `"MESSAGE":"`+kill(80)+`"`)
	if err := os.WriteFile(data, []byte(stream), 0o600); err != nil {
		t.Fatal(err)
	}
	script, argsFile := fakeJournalctl(t, "cat '"+data+"'\necho 'Failed to seek' >&2\nexit 1\n")
	args, err := JournalctlArgs(testBoot, cursorFor(0), testUID)
	if err != nil {
		t.Fatal(err)
	}
	err = Follow(context.Background(), script, args, h.r, func() time.Time { return t0 })
	if !errors.Is(err, ErrFollowerExited) || !strings.Contains(err.Error(), "Failed to seek") {
		t.Fatalf("an exited follower must be reported with its stderr, got %v", err)
	}
	if got := classes(h.verdicts); !sameStrings(got, []string{"foreign_memcg"}) {
		t.Fatalf("verdicts %v", got)
	}
	raw, _ := os.ReadFile(argsFile)
	if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !sameStrings(got, args) {
		t.Fatalf("journalctl got %q, want the production filter %q", got, args)
	}
}

func TestFollowStopsWhenTheContextEnds(t *testing.T) {
	h := newHarness(t, "")
	script, _ := fakeJournalctl(t, "exec sleep 5\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Follow(ctx, script, []string{"x"}, h.r, func() time.Time { return t0 })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the child was not stopped: %v", time.Since(start))
	}
}

func TestFollowReportsAMissingProgram(t *testing.T) {
	h := newHarness(t, "")
	if err := Follow(context.Background(), filepath.Join(t.TempDir(), "nope"), nil, h.r, time.Now); err == nil {
		t.Fatal("want an error")
	}
}

func TestExecLoggerRunsTheTagAndMessage(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	prog := filepath.Join(dir, "logger")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+out+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ExecLogger(prog)(context.Background(), HeartbeatTag, Message("00112233445566778899aabbccddeeff")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	want := []string{"-t", "multica-containment-hb", "--", "hb nonce=00112233445566778899aabbccddeeff"}
	if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !sameStrings(got, want) {
		t.Fatalf("got %q", got)
	}
	failing := filepath.Join(dir, "failing")
	_ = os.WriteFile(failing, []byte("#!/bin/sh\necho no journal >&2\nexit 3\n"), 0o700)
	if err := ExecLogger(failing)(context.Background(), "t", "m"); err == nil || !strings.Contains(err.Error(), "no journal") {
		t.Fatalf("got %v", err)
	}
}

// Read-only check against the real journal of this host. Run with
// OOMWATCH_LIVE_JOURNAL=1 go test -run TestLiveJournalReadOnly -v.
func TestLiveJournalReadOnly(t *testing.T) {
	if os.Getenv("OOMWATCH_LIVE_JOURNAL") != "1" {
		t.Skip("set OOMWATCH_LIVE_JOURNAL=1 to query the live journal (read-only)")
	}
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	boot, err := NormalizeBootID(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := TailCursor(ctx, ExecRunner, "journalctl", boot)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := CursorValid(ctx, ExecRunner, "journalctl", boot, c); err != nil || !ok {
		t.Fatalf("the tail cursor must be valid: %v, %v", ok, err)
	}
	pc, _ := ParseCursor(c)
	pc.Seqnum = 0xffffffffff
	bad := "s=" + pc.SeqnumID + ";i=ffffffffff;b=" + pc.Boot + ";m=1;t=1;x=1"
	if ok, err := CursorValid(ctx, ExecRunner, "journalctl", boot, bad); err != nil || ok {
		t.Fatalf("a cursor that is not in the journal must be invalid: %v, %v", ok, err)
	}
	t.Logf("boot %s tail %s", boot, c)
}
