package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

// progressServer records every update report the daemon posts. When oldServer
// is set it decodes only the pre-progress fields, like a server that predates
// the progress field.
type progressServer struct {
	mu       sync.Mutex
	payloads []map[string]any
}

func (s *progressServer) handler(oldServer bool, progressStatus int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		s.mu.Lock()
		s.payloads = append(s.payloads, p)
		s.mu.Unlock()
		if _, has := p["progress"]; has && !oldServer && progressStatus != 0 {
			http.Error(w, "{}", progressStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (s *progressServer) progressReports() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, p := range s.payloads {
		if _, ok := p["progress"]; ok {
			out = append(out, p)
		}
	}
	return out
}

// downloadingUpdate stubs the fork source so applyForkUpdate streams a fake
// download through the Progress the daemon supplies.
func downloadingUpdate(t *testing.T) {
	t.Helper()
	stubFork(t, &cli.ForkUpdate{}, nil)
	applyForkUpdate = func(_ context.Context, u *cli.ForkUpdate, _ time.Duration) (string, error) {
		const total = 1 << 20
		r := u.Progress.Wrap(io.LimitReader(zeroReader{}, total), total)
		buf := make([]byte, 4096)
		for {
			if _, err := r.Read(buf); err != nil {
				break
			}
		}
		u.Progress.Finish()
		u.Progress.Phase("Verifying sha256")
		return "installed", nil
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func runProgressUpdate(t *testing.T, srv *progressServer, oldServer bool, progressStatus int) (*Daemon, *bytes.Buffer) {
	t.Helper()
	ts := httptest.NewServer(srv.handler(oldServer, progressStatus))
	t.Cleanup(ts.Close)
	var logs bytes.Buffer
	d := &Daemon{
		client: NewClient(ts.URL),
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	d.runUpdateFn = d.runUpdate
	d.cancelFunc = func() {}
	downloadingUpdate(t)
	d.handleUpdate(context.Background(), "runtime-1", &PendingUpdate{ID: "update-1", TargetVersion: "v9.9.9"})
	return d, &logs
}

func TestHandleUpdate_ReportsThrottledDownloadProgress(t *testing.T) {
	srv := &progressServer{}
	runProgressUpdate(t, srv, false, 0)

	reports := srv.progressReports()
	if len(reports) != 1 {
		t.Fatalf("progress reports = %d, want exactly 1 (the whole fake download takes well under 1s): %v", len(reports), reports)
	}
	if reports[0]["status"] != "running" {
		t.Fatalf("status = %v, want running", reports[0]["status"])
	}
	prog := reports[0]["progress"].(map[string]any)
	if prog["phase"] != "downloading" || prog["bytes_total"].(float64) != 1<<20 {
		t.Fatalf("progress = %v", prog)
	}
	last := srv.payloads[len(srv.payloads)-1]
	if last["status"] != "completed" {
		t.Fatalf("final report = %v, want completed", last)
	}
}

func TestHandleUpdate_ProgressLeavesNoLogLines(t *testing.T) {
	_, logs := runProgressUpdate(t, &progressServer{}, false, 0)
	if strings.Contains(strings.ToLower(logs.String()), "progress") {
		t.Fatalf("daemon log carries progress lines:\n%s", logs.String())
	}
}

// A server that rejects or does not know the progress field must not stop the update.
func TestHandleUpdate_CompletesWhenServerIgnoresOrRejectsProgress(t *testing.T) {
	for name, tc := range map[string]struct {
		old    bool
		status int
	}{"old server": {old: true}, "server error": {status: http.StatusInternalServerError}} {
		t.Run(name, func(t *testing.T) {
			srv := &progressServer{}
			_, logs := runProgressUpdate(t, srv, tc.old, tc.status)
			last := srv.payloads[len(srv.payloads)-1]
			if last["status"] != "completed" {
				t.Fatalf("final report = %v, want completed", last)
			}
			if strings.Contains(strings.ToLower(logs.String()), "progress") {
				t.Fatalf("progress failure leaked into logs:\n%s", logs.String())
			}
		})
	}
}
