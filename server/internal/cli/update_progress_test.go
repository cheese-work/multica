package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

const progressTestTag = "v1.2.3"

// fakeReleaseSource serves release metadata, a checksum manifest that does not
// match the archive, and a 1 MiB archive. The checksum mismatch stops the
// update before it can replace the test binary.
func fakeReleaseSource(t *testing.T) {
	t.Helper()
	archive := make([]byte, 1<<20)
	name := releaseAssetCandidates(progressTestTag, runtime.GOOS, runtime.GOARCH)[0]
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/multica-ai/multica/releases/tags/" + progressTestTag:
			_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: progressTestTag, Assets: []GitHubReleaseAsset{
				{Name: name, BrowserDownloadURL: srv.URL + "/archive"},
				{Name: ChecksumManifestName, BrowserDownloadURL: srv.URL + "/checksums"},
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
	t.Setenv(releaseAPIBaseURLEnv, srv.URL)
	t.Setenv(releaseDownloadBaseURLEnv, "")
}

func TestUpdateViaDownloadReportsProgressOnTTY(t *testing.T) {
	fakeReleaseSource(t)
	p, buf, _ := fakeTTY(0)

	_, err := UpdateViaDownloadWithOptions(progressTestTag, DownloadOptions{Timeout: time.Minute, Progress: p})
	if err == nil || !strings.Contains(err.Error(), "verify download: ") || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "100%") || !strings.Contains(out, "1.0 / 1.0 MB") {
		t.Fatalf("no bar for the archive: %q", out)
	}
	if !strings.Contains(out, "Verifying sha256") || !strings.HasSuffix(out, "\n") {
		t.Fatalf("phase line missing or bar line left open: %q", out)
	}
}

func TestUpdateViaDownloadSilentWithoutTTY(t *testing.T) {
	fakeReleaseSource(t)
	var buf strings.Builder
	quiet := NewProgress(&buf) // a strings.Builder is not a terminal

	for name, opts := range map[string]DownloadOptions{
		"not a tty": {Timeout: time.Minute, Progress: quiet},
		"nil":       {Timeout: time.Minute},
	} {
		_, err := UpdateViaDownloadWithOptions(progressTestTag, opts)
		if err == nil || !strings.Contains(err.Error(), "verify download: ") || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if _, err := UpdateViaDownloadWithTimeout(progressTestTag, time.Minute); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("quiet form: err = %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("output without a tty: %q", buf.String())
	}
}
