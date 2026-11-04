package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFileSized(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBrewDownloadedBytesRenameKeepsCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bottle.tar.gz")
	writeFileSized(t, path+".incomplete", 1000)
	before := brewDownloadedBytes([]string{path}, nil)
	if err := os.Rename(path+".incomplete", path); err != nil {
		t.Fatal(err)
	}
	after := brewDownloadedBytes([]string{path}, nil)
	if before != 1000 || after != 1000 {
		t.Fatalf("sum across rename: before=%d after=%d, want 1000/1000", before, after)
	}
}

func TestBrewDownloadedBytesIgnoresUnrelatedIncomplete(t *testing.T) {
	dir := t.TempDir()
	owned := filepath.Join(dir, "owned.tar.gz")
	writeFileSized(t, owned+".incomplete", 300)
	writeFileSized(t, filepath.Join(dir, "other.tar.gz.incomplete"), 5000)
	if got := brewDownloadedBytes([]string{owned}, nil); got != 300 {
		t.Fatalf("got %d, want 300 (unrelated .incomplete must not count)", got)
	}
}

func TestBrewDownloadedBytesCacheHitNotCounted(t *testing.T) {
	dir := t.TempDir()
	hit := filepath.Join(dir, "hit.tar.gz")
	writeFileSized(t, hit, 9000)
	hits := map[string]bool{hit: true}
	if got := brewDownloadedBytes([]string{hit}, hits); got != 0 {
		t.Fatalf("cache hit counted: %d", got)
	}
	// A hit whose partial file appears later counts from then on.
	writeFileSized(t, hit+".incomplete", 400)
	if got := brewDownloadedBytes([]string{hit}, hits); got != 400 {
		t.Fatalf("late .incomplete on a hit: got %d, want 400", got)
	}
}

func TestBrewDownloadedBytesCountsDependencyPath(t *testing.T) {
	dir := t.TempDir()
	formula := filepath.Join(dir, "multica.tar.gz")
	dep := filepath.Join(dir, "dep.tar.gz")
	writeFileSized(t, formula+".incomplete", 100)
	writeFileSized(t, dep+".incomplete", 250)
	if got := brewDownloadedBytes([]string{formula, dep}, nil); got != 350 {
		t.Fatalf("got %d, want 350", got)
	}
}

func TestProgressSetDropResetsRateNeverNegative(t *testing.T) {
	p, buf, clock := fakeTTY(0)
	const mb = 1 << 20
	*clock = clock.Add(time.Second)
	p.Set(10*mb, 0)
	// A retry deleted the partial file: the sum falls.
	*clock = clock.Add(time.Second)
	p.Set(2*mb, 0)
	*clock = clock.Add(2 * time.Second)
	p.Set(6*mb, 0)
	p.Finish()
	out := buf.String()
	if strings.Contains(out, "-") {
		t.Fatalf("negative figure drawn: %q", out)
	}
	// After the reset the baseline is 2 MB at t+2s; 4 MB over 2 s = 2.0 MB/s.
	if !strings.Contains(out, "6.0 MB  2.0 MB/s") {
		t.Fatalf("rate not measured from the reset baseline: %q", out)
	}
}

// fakeBrew installs a `brew` shell script on PATH. It logs every call to
// $FAKE_LOG, serves update/deps/--cache itself and runs $FAKE_UPGRADE for upgrade.
func fakeBrew(t *testing.T, cache, upgradeScript string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(t.TempDir(), "calls.log")
	script := `#!/bin/sh
echo "$* NOAUTO=$HOMEBREW_NO_AUTO_UPDATE" >> "$FAKE_LOG"
case "$1" in
update) exit 0 ;;
deps) echo dep ;;
--cache) shift; for f in "$@"; do echo "$FAKE_CACHE/$(basename "$f").tar.gz"; done ;;
upgrade) eval "$FAKE_UPGRADE" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "brew"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	t.Setenv("FAKE_CACHE", cache)
	t.Setenv("FAKE_UPGRADE", upgradeScript)
	oldInterval := brewPollInterval
	brewPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { brewPollInterval = oldInterval })
	return log
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdateViaBrewWithProgressTTYShowsPhasesAndCounts(t *testing.T) {
	cache := t.TempDir()
	upgrade := `
dd if=/dev/zero of="$FAKE_CACHE/multica.tar.gz.incomplete" bs=1048576 count=3 2>/dev/null
dd if=/dev/zero of="$FAKE_CACHE/dep.tar.gz.incomplete" bs=1048576 count=1 2>/dev/null
sleep 0.2
mv "$FAKE_CACHE/multica.tar.gz.incomplete" "$FAKE_CACHE/multica.tar.gz"
mv "$FAKE_CACHE/dep.tar.gz.incomplete" "$FAKE_CACHE/dep.tar.gz"
sleep 0.1
echo upgraded`
	log := fakeBrew(t, cache, upgrade)
	p, buf, _ := fakeTTY(0)
	out, err := UpdateViaBrewWithProgress(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "upgraded") {
		t.Fatalf("output not returned: %q", out)
	}
	got := buf.String()
	for _, want := range []string{"Updating Homebrew", "Downloading and installing via Homebrew", "4.0 MB", "Installed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("terminal output lacks %q: %q", want, got)
		}
	}
	calls := readLog(t, log)
	if !strings.Contains(calls, "update NOAUTO=\n") && !strings.Contains(calls, "update NOAUTO=") {
		t.Fatalf("brew update not run: %q", calls)
	}
	if !strings.Contains(calls, "upgrade multica-ai/tap/multica NOAUTO=1") {
		t.Fatalf("upgrade must run with HOMEBREW_NO_AUTO_UPDATE=1: %q", calls)
	}
}

func TestUpdateViaBrewWithProgressAllCacheHitsDrawsNoLine(t *testing.T) {
	cache := t.TempDir()
	writeFileSized(t, filepath.Join(cache, "multica.tar.gz"), 4096)
	writeFileSized(t, filepath.Join(cache, "dep.tar.gz"), 4096)
	fakeBrew(t, cache, "sleep 0.1; echo upgraded")
	p, buf, _ := fakeTTY(0)
	if _, err := UpdateViaBrewWithProgress(p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "MB") {
		t.Fatalf("all hits must draw no size line: %q", buf.String())
	}
}

func TestUpdateViaBrewWithProgressFailureKeepsTodaysError(t *testing.T) {
	cache := t.TempDir()
	fakeBrew(t, cache, "echo boom; exit 3")
	p, _, _ := fakeTTY(0)
	out, err := UpdateViaBrewWithProgress(p)
	if err == nil || !strings.Contains(err.Error(), "brew upgrade failed") {
		t.Fatalf("err = %v, want 'brew upgrade failed'", err)
	}
	if !strings.Contains(out, "boom") {
		t.Fatalf("brew output lost: %q", out)
	}
}

func TestUpdateViaBrewWithProgressNonTTYIsQuietForm(t *testing.T) {
	cache := t.TempDir()
	log := fakeBrew(t, cache, "echo upgraded")
	var buf bytes.Buffer
	if _, err := UpdateViaBrewWithProgress(NewProgress(&buf)); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("non-TTY wrote %q", buf.String())
	}
	if calls := strings.TrimSpace(readLog(t, log)); strings.Count(calls, "\n") != 0 || !strings.HasPrefix(calls, "upgrade multica-ai/tap/multica") {
		t.Fatalf("non-TTY must run only brew upgrade, got %q", calls)
	}
}

func TestUpdateViaBrewDaemonRunsOnlyUpgrade(t *testing.T) {
	cache := t.TempDir()
	log := fakeBrew(t, cache, "echo upgraded")
	out, err := UpdateViaBrew()
	if err != nil || !strings.Contains(out, "upgraded") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	calls := strings.TrimSpace(readLog(t, log))
	if calls != "upgrade multica-ai/tap/multica NOAUTO=" {
		t.Fatalf("daemon path ran %q, want only today's single upgrade", calls)
	}
}

func TestBrewSamplerReportsDropToZeroAfterData(t *testing.T) {
	var s brewSampler
	for _, tc := range []struct {
		n    int64
		want bool
	}{
		{0, false},      // nothing downloaded yet: no line
		{2 << 20, true}, // partial file grows
		{0, true},       // retry deleted the only partial file: the drop must reach Progress
		{1 << 20, true}, // restart
	} {
		if got := s.report(tc.n); got != tc.want {
			t.Fatalf("report(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}
