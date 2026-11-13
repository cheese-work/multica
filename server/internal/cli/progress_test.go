package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeTTY is a Progress on a buffer with a controllable clock.
func fakeTTY(total int64) (*Progress, *bytes.Buffer, *time.Time) {
	var buf bytes.Buffer
	clock := time.Unix(1000, 0)
	p := NewProgress(&buf)
	p.tty = true
	p.now = func() time.Time { return clock }
	p.start = clock
	_ = total
	return p, &buf, &clock
}

func TestProgressKnownSizeRendersBarPercentAndMB(t *testing.T) {
	p, buf, clock := fakeTTY(0)
	r := p.Wrap(strings.NewReader(strings.Repeat("x", 10<<20)), 20<<20)
	*clock = clock.Add(time.Second)
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	p.Finish()
	out := buf.String()
	for _, want := range []string{"[", "]", "50%", "10.0 / 20.0 MB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q: %q", want, out)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("line not ended: %q", out)
	}
}

func TestProgressUnknownSizeRendersMBAndRateNoPercent(t *testing.T) {
	p, buf, clock := fakeTTY(0)
	r := p.Wrap(strings.NewReader(strings.Repeat("x", 8<<20)), 0)
	*clock = clock.Add(2 * time.Second)
	_, _ = io.Copy(io.Discard, r)
	p.Finish()
	out := buf.String()
	for _, want := range []string{"8.0 MB", "4.0 MB/s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, "%") || strings.Contains(out, "=") {
		t.Fatalf("unknown size must not show percent or bar: %q", out)
	}
}

func TestProgressSilentWhenNotTTYOrNil(t *testing.T) {
	var buf bytes.Buffer
	p := NewProgress(&buf) // a bytes.Buffer is not a terminal
	_, _ = io.Copy(io.Discard, p.Wrap(strings.NewReader("abc"), 3))
	p.Phase("Installing")
	p.Finish()
	if buf.Len() != 0 {
		t.Fatalf("non-TTY wrote %q", buf.String())
	}
	var nilP *Progress
	_, _ = io.Copy(io.Discard, nilP.Wrap(strings.NewReader("abc"), 3))
	nilP.Phase("Installing")
	nilP.Set(5, 10)
	nilP.Finish()
}

func TestProgressPhaseEndsBarLineAndPrintsPhase(t *testing.T) {
	p, buf, _ := fakeTTY(0)
	_, _ = io.Copy(io.Discard, p.Wrap(strings.NewReader("abc"), 3))
	p.Phase("Verifying sha256")
	p.Phase("Installed")
	out := buf.String()
	if !strings.Contains(out, "\nVerifying sha256\nInstalled\n") {
		t.Fatalf("phase lines wrong: %q", out)
	}
}

func TestProgressRedrawsAtMostTenPerSecond(t *testing.T) {
	p, buf, clock := fakeTTY(0)
	for i := 0; i < 100; i++ { // 100 updates within 0.5 s
		*clock = clock.Add(5 * time.Millisecond)
		p.Set(int64(i+1)<<10, 1<<20)
	}
	if n := strings.Count(buf.String(), "\r"); n > 6 {
		t.Fatalf("%d redraws in 0.5s, want <= 6", n)
	}
}

func TestProgressCallbackThrottledAndFinalDelivered(t *testing.T) {
	p, _, clock := fakeTTY(0)
	var got []int64
	p.OnUpdate = func(done, total int64) { got = append(got, done) }
	for i := 0; i < 100; i++ {
		*clock = clock.Add(5 * time.Millisecond)
		p.Set(int64(i+1), 100)
	}
	if len(got) > 6 || len(got) == 0 {
		t.Fatalf("callback fired %d times in 0.5s", len(got))
	}
	p.Finish()
	if got[len(got)-1] != 100 {
		t.Fatalf("last callback = %d, want final 100", got[len(got)-1])
	}
}

func TestProgressSetIsThePollingInput(t *testing.T) {
	p, buf, _ := fakeTTY(0)
	p.Set(5<<20, 10<<20)
	out := buf.String()
	if !strings.Contains(out, "50%") || !strings.Contains(out, "5.0 / 10.0 MB") {
		t.Fatalf("Set did not render: %q", out)
	}
}

func TestProgressDetectsTTYFromFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "p")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if NewProgress(f).tty { // a regular file is not a terminal
		t.Fatal("regular file treated as TTY")
	}
}

func TestProgressCharDeviceThatIsNotATerminalIsSilent(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if NewProgress(f).tty {
		t.Fatal("/dev/null treated as TTY")
	}
}

func TestApplyForkUpdateReportsProgressAndPhases(t *testing.T) {
	f := newFake(testHead)
	srv := f.serve(t)
	exe, _ := newInstallTarget(t)
	u := updateFor(f, srv.URL)
	p, buf, _ := fakeTTY(0)
	u.Progress = p

	if _, err := ApplyForkUpdateTo(context.Background(), u, time.Minute, exe); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	i := strings.Index(out, "Verifying sha256")
	j := strings.Index(out, "Probing --version")
	k := strings.Index(out, "Installing")
	l := strings.Index(out, "Installed")
	if i < 0 || !(i < j && j < k && k < l) {
		t.Fatalf("phases missing or out of order: %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("line not ended: %q", out)
	}
}

func TestApplyForkUpdateErrorEndsProgressLineAndKeepsMessage(t *testing.T) {
	f := newFake(testHead)
	srv := f.serve(t)
	exe, _ := newInstallTarget(t)
	u := updateFor(f, srv.URL)
	u.Digest = strings.Repeat("0", 64)
	p, buf, _ := fakeTTY(0)
	u.Progress = p

	_, err := ApplyForkUpdateTo(context.Background(), u, time.Minute, exe)
	if err == nil || !strings.Contains(err.Error(), "verify download: ") || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("line not ended after error: %q", buf.String())
	}

	f.status = http.StatusInternalServerError
	u.Progress = nil
	if _, err := ApplyForkUpdateTo(context.Background(), u, time.Minute, exe); err == nil || err.Error() != "download binary: HTTP 500 from the server" {
		t.Fatalf("err = %v", err)
	}
}

func TestProgressOnPhaseReceivesEachPhaseName(t *testing.T) {
	p := NewProgress(io.Discard)
	var got []string
	p.OnPhase = func(name string) { got = append(got, name) }
	p.Phase("Verifying sha256")
	p.Phase("Installing")
	if strings.Join(got, ",") != "Verifying sha256,Installing" {
		t.Fatalf("phases = %v", got)
	}
}
