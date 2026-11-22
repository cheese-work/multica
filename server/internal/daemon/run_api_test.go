package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runAPIDaemon(t *testing.T) *Daemon {
	t.Helper()
	cancelled := false
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := New(Config{WorkspacesRoot: t.TempDir(), DaemonID: "d-test", ServerBaseURL: "https://api.example.test"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.cancelFunc = func() { cancelled = true; cancel() }
	t.Cleanup(func() {
		if cancelled {
			t.Error("a run API route cancelled the daemon")
		}
	})
	return d
}

// AC 2: the daemon's control routes are not served on the run API.
func TestRunAPIMuxDoesNotServeControlRoutes(t *testing.T) {
	mux := runAPIDaemon(t).runAPIMux(time.Now())
	for _, path := range []string{"/shutdown", "/maintenance/acquire", "/maintenance/release", "/", "/repo/checkout/x"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, path, rec.Code)
			}
		}
	}
}

func TestRunAPIMuxServesHealthAndRepoCheckout(t *testing.T) {
	mux := runAPIDaemon(t).runAPIMux(time.Now())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/health = %d, want 200", rec.Code)
	}
	// no credential: the real checkout handler answers 401, not the mux's 404
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repo/checkout", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /repo/checkout = %d, want 401 from the checkout handler", rec.Code)
	}
}

func TestStartRunAPIServesOnPrivateUnixSocketAndStopRemovesIt(t *testing.T) {
	dir, err := os.MkdirTemp("", "rapi") // short: a Unix socket path holds about 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock, stop, err := runAPIDaemon(t).startRunAPI(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(sock) != dir {
		t.Errorf("socket %s is outside %s", sock, dir)
	}
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket stat = %v, %v; want mode 0600", fi, err)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	resp, err := client.Post("http://run/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /shutdown over the socket = %d, want 404", resp.StatusCode)
	}
	stop()
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still present after stop: %v", err)
	}
}
