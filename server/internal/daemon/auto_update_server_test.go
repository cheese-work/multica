package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

const serverUpdateCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// noGitHub fails any request to GitHub: the daemon must update from its own
// server only.
type noGitHub struct {
	t    *testing.T
	next http.RoundTripper
}

func (n noGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Host, "github") {
		n.t.Errorf("daemon called GitHub: %s", r.URL)
		return nil, fmt.Errorf("GitHub call blocked: %s", r.URL)
	}
	return n.next.RoundTrip(r)
}

func blockGitHub(t *testing.T) {
	t.Helper()
	prev := http.DefaultTransport
	http.DefaultTransport = noGitHub{t: t, next: prev}
	t.Cleanup(func() { http.DefaultTransport = prev })
}

// fakeUpdateServer serves the daemon update endpoints for this platform and
// records the Authorization header of every request.
func fakeUpdateServer(t *testing.T, bin []byte, sha string, auths *[]string) *httptest.Server {
	t.Helper()
	key := runtime.GOOS + "-" + runtime.GOARCH
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/update", func(w http.ResponseWriter, r *http.Request) {
		*auths = append(*auths, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"commit": serverUpdateCommit, "version": "0.6.5-test",
			"binaries": map[string]any{key: map[string]any{"sha256": sha, "size": len(bin)}},
		})
	})
	mux.HandleFunc("/api/daemon/update/binary", func(w http.ResponseWriter, r *http.Request) {
		*auths = append(*auths, r.Header.Get("Authorization"))
		if r.URL.Query().Get("os") != runtime.GOOS || r.URL.Query().Get("arch") != runtime.GOARCH {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(bin)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func serverUpdateDaemon(t *testing.T, srv *httptest.Server) (*Daemon, string) {
	t.Helper()
	if !cli.ForkUpdateSupported(runtime.GOOS) {
		t.Skip("fork source is Linux/macOS only")
	}
	d, _ := newAutoUpdateTestDaemon(t, "v0.1.13")
	d.client = NewClient(srv.URL)
	d.client.SetToken("mdt_test")

	exe := filepath.Join(t.TempDir(), "multica")
	if err := os.WriteFile(exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	prevSrc, prevCommit, prevApply := forkUpdateSource, currentCommit, applyForkUpdate
	forkUpdateSource = func() bool { return true }
	currentCommit = func() string { return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }
	// Install into a temp file, never over the test binary.
	applyForkUpdate = func(ctx context.Context, u *cli.ForkUpdate, timeout time.Duration) (string, error) {
		return cli.ApplyForkUpdateTo(ctx, u, timeout, exe)
	}
	t.Cleanup(func() { forkUpdateSource, currentCommit, applyForkUpdate = prevSrc, prevCommit, prevApply })
	return d, exe
}

func fakeServerBinary() ([]byte, string) {
	bin := []byte("#!/bin/sh\necho 'multica 0.6.5-test (commit: " + serverUpdateCommit + ", built: now)'\n")
	sum := sha256.Sum256(bin)
	return bin, hex.EncodeToString(sum[:])
}

// A host with no GitHub token resolves, downloads and verifies the update
// through its own server, and never talks to GitHub.
func TestRunForkUpdate_InstallsFromOwnServerWithoutGitHub(t *testing.T) {
	blockGitHub(t)
	bin, sha := fakeServerBinary()
	var auths []string
	srv := fakeUpdateServer(t, bin, sha, &auths)
	d, exe := serverUpdateDaemon(t, srv)

	out, err := d.runForkUpdate()
	if err != nil {
		t.Fatalf("runForkUpdate: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != string(bin) {
		t.Fatalf("installed bytes differ (out %q)", out)
	}
	if len(auths) != 2 {
		t.Fatalf("server saw %d requests, want metadata + binary", len(auths))
	}
	for _, a := range auths {
		if a != "Bearer mdt_test" {
			t.Errorf("Authorization = %q", a)
		}
	}
}

// The digest comes from the metadata; bytes that do not match it are never installed.
func TestRunForkUpdate_RejectsServerBytesThatMissTheDigest(t *testing.T) {
	blockGitHub(t)
	bin, _ := fakeServerBinary()
	var auths []string
	srv := fakeUpdateServer(t, bin, strings.Repeat("0", 64), &auths)
	d, exe := serverUpdateDaemon(t, srv)

	if _, err := d.runForkUpdate(); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old-binary" {
		t.Fatal("installed binary was modified")
	}
}

// The Runtimes-page update path (handleUpdate -> runUpdate) uses the same source.
func TestRunUpdate_UsesOwnServerSource(t *testing.T) {
	blockGitHub(t)
	bin, sha := fakeServerBinary()
	var auths []string
	srv := fakeUpdateServer(t, bin, sha, &auths)
	d, exe := serverUpdateDaemon(t, srv)

	if _, err := d.runUpdate("v9.9.9"); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != string(bin) {
		t.Fatal("installed bytes differ")
	}
}

// Same commit as the server: nothing to do.
func TestAutoUpdateTarget_UpToDateWithServer(t *testing.T) {
	blockGitHub(t)
	bin, sha := fakeServerBinary()
	var auths []string
	srv := fakeUpdateServer(t, bin, sha, &auths)
	d, _ := serverUpdateDaemon(t, srv)
	currentCommit = func() string { return serverUpdateCommit[:12] }

	if target, ok := d.autoUpdateTarget(context.Background()); ok {
		t.Fatalf("target %q for a daemon already on the server's commit", target)
	}
	if target, ok := func() (string, bool) {
		currentCommit = func() string { return strings.Repeat("a", 40) }
		return d.autoUpdateTarget(context.Background())
	}(); !ok || target != serverUpdateCommit {
		t.Fatalf("target = %q, ok = %v", target, ok)
	}
}
