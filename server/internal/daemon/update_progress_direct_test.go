package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

const directTestTag = "v1.2.3"

// directReleaseSource serves a release whose checksum manifest does not match
// the 1 MiB archive, so the update fails at verification and never replaces the
// test binary. Windows takes this path in production; the tests force it by
// switching the fork source off.
func directReleaseSource(t *testing.T) {
	t.Helper()
	archive := make([]byte, 1<<20)
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	name := fmt.Sprintf("multica-cli-1.2.3-%s-%s.%s", runtime.GOOS, runtime.GOARCH, ext)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/multica-ai/multica/releases/tags/" + directTestTag:
			_ = json.NewEncoder(w).Encode(cli.GitHubRelease{TagName: directTestTag, Assets: []cli.GitHubReleaseAsset{
				{Name: name, BrowserDownloadURL: srv.URL + "/archive"},
				{Name: cli.ChecksumManifestName, BrowserDownloadURL: srv.URL + "/checksums"},
			}})
		case "/checksums":
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), name)
		case "/archive":
			w.Header().Set("Content-Length", fmt.Sprint(len(archive)))
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_RELEASE_API_BASE_URL", srv.URL)
	t.Setenv("MULTICA_RELEASE_DOWNLOAD_BASE_URL", "")
	prev := forkUpdateSource
	forkUpdateSource = func() bool { return false }
	t.Cleanup(func() { forkUpdateSource = prev })
}

func exeDigest(t *testing.T) [32]byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return [32]byte(h.Sum(nil))
}

// Server-triggered direct-download update (Windows): the download reports
// throttled progress, a failed download keeps its error text and the binary.
func TestHandleUpdate_DirectDownloadReportsProgressAndKeepsBinaryOnFailure(t *testing.T) {
	directReleaseSource(t)
	before := exeDigest(t)
	srv := &progressServer{}
	ts := httptest.NewServer(srv.handler(false, 0))
	t.Cleanup(ts.Close)
	d := &Daemon{client: NewClient(ts.URL), logger: slog.Default()}
	d.runUpdateFn = d.runUpdate
	d.cancelFunc = func() {}

	d.handleUpdate(context.Background(), "runtime-1", &PendingUpdate{ID: "update-1", TargetVersion: directTestTag})

	reports := srv.progressReports()
	if len(reports) != 2 { // the fake download takes well under 1s: first report + flushed final one
		t.Fatalf("progress reports = %d, want 2: %v", len(reports), reports)
	}
	if fin := reports[1]["progress"].(map[string]any); fin["bytes_done"].(float64) != 1<<20 {
		t.Fatalf("final progress = %v, want bytes_done == bytes_total", fin)
	}
	prog := reports[0]["progress"].(map[string]any)
	if prog["phase"] != "downloading" || prog["bytes_total"].(float64) != 1<<20 {
		t.Fatalf("progress = %v", prog)
	}
	last := srv.payloads[len(srv.payloads)-1]
	if last["status"] != "failed" || !strings.Contains(fmt.Sprint(last["error"]), "download update failed: verify download: ") || !strings.Contains(fmt.Sprint(last["error"]), "checksum mismatch") {
		t.Fatalf("terminal report = %v", last)
	}
	if exeDigest(t) != before {
		t.Fatal("installed binary changed")
	}
}

// With a live reporter every report must reach the server: a fake clock lifts
// the throttle so the last phase report carries the finished byte counts.
func TestRunUpdate_DirectDownloadFinalReportCarriesFullBytes(t *testing.T) {
	directReleaseSource(t)
	var got []map[string]any
	rep := newUpdateProgressReporter(func(_ context.Context, p map[string]any) error {
		got = append(got, p["progress"].(map[string]any))
		return nil
	})
	tick := rep.now()
	rep.now = func() time.Time { tick = tick.Add(2 * time.Second); return tick }
	d := &Daemon{logger: slog.Default()}
	d.updateProgress.Store(rep)

	_, err := d.runUpdate(directTestTag)
	rep.wait()

	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no progress reports sent")
	}
	last := got[len(got)-1]
	if last["bytes_done"].(int64) != 1<<20 || last["bytes_total"].(int64) != 1<<20 {
		t.Fatalf("final report = %v (all: %v)", last, got)
	}
}

// The auto-update poller runs the same branch without a reporter: no progress
// report reaches the server and the log stays free of progress lines.
func TestRunUpdate_DirectDownloadWithoutReporterIsQuiet(t *testing.T) {
	directReleaseSource(t)
	srv := &progressServer{}
	ts := httptest.NewServer(srv.handler(false, 0))
	t.Cleanup(ts.Close)
	var logs strings.Builder
	d := &Daemon{client: NewClient(ts.URL), logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}

	_, err := d.runUpdate(directTestTag)

	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if n := len(srv.payloads); n != 0 {
		t.Fatalf("server got %d reports, want 0", n)
	}
	if strings.Contains(strings.ToLower(logs.String()), "progress") {
		t.Fatalf("log carries progress lines:\n%s", logs.String())
	}
}
