package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
)

const updateTestCommit = "0123456789abcdef0123456789abcdef01234567"

// writeDaemonBinaries lays out a binaries dir the way the Dockerfile does and
// returns the bytes it wrote for linux-amd64.
func writeDaemonBinaries(t *testing.T, commit string) []byte {
	t.Helper()
	dir := t.TempDir()
	linux := []byte("linux-amd64 daemon bytes")
	files := map[string][]byte{
		"linux-amd64":  linux,
		"darwin-arm64": []byte("darwin-arm64 daemon bytes"),
	}
	bins := map[string]map[string]any{}
	for key, data := range files {
		if err := os.WriteFile(filepath.Join(dir, "multica-"+key), data, 0o755); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		bins[key] = map[string]any{"sha256": hex.EncodeToString(sum[:]), "size": len(data)}
	}
	manifest, err := json.Marshal(map[string]any{"commit": commit, "version": "0.6.5-test", "binaries": bins})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MULTICA_DAEMON_BINARIES_DIR", dir)
	return linux
}

func updateTestServer(t *testing.T, serverCommit string) *httptest.Server {
	t.Helper()
	prev := commit
	commit = serverCommit
	t.Cleanup(func() { commit = prev })
	hub := realtime.NewHub()
	srv := httptest.NewServer(NewRouter(testPool, hub, events.New(), analytics.NoopClient{}, nil))
	t.Cleanup(srv.Close)
	return srv
}

func updateGet(t *testing.T, url string, authed bool) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authed {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestDaemonUpdateEndpoints(t *testing.T) {
	linux := writeDaemonBinaries(t, updateTestCommit)
	srv := updateTestServer(t, updateTestCommit)

	t.Run("metadata names the server's own build", func(t *testing.T) {
		code, body := updateGet(t, srv.URL+"/api/daemon/update", true)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", code, body)
		}
		var got struct {
			Commit   string `json:"commit"`
			Version  string `json:"version"`
			Binaries map[string]struct {
				SHA256 string `json:"sha256"`
				Size   int64  `json:"size"`
			} `json:"binaries"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(linux)
		if got.Commit != updateTestCommit || got.Version != "0.6.5-test" {
			t.Errorf("commit/version = %q/%q", got.Commit, got.Version)
		}
		if b := got.Binaries["linux-amd64"]; b.SHA256 != hex.EncodeToString(sum[:]) || b.Size != int64(len(linux)) {
			t.Errorf("linux-amd64 entry = %+v", b)
		}
	})

	t.Run("binary streams the listed platform", func(t *testing.T) {
		code, body := updateGet(t, srv.URL+"/api/daemon/update/binary?os=linux&arch=amd64", true)
		if code != http.StatusOK || string(body) != string(linux) {
			t.Fatalf("status = %d, body = %q", code, body)
		}
	})

	for _, q := range []string{"os=linux&arch=riscv64", "os=..&arch=%2Fetc%2Fpasswd", "os=linux", ""} {
		t.Run(fmt.Sprintf("binary rejects %q", q), func(t *testing.T) {
			if code, _ := updateGet(t, srv.URL+"/api/daemon/update/binary?"+q, true); code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", code)
			}
		})
	}

	t.Run("requires daemon auth", func(t *testing.T) {
		for _, p := range []string{"/api/daemon/update", "/api/daemon/update/binary?os=linux&arch=amd64"} {
			if code, _ := updateGet(t, srv.URL+p, false); code != http.StatusUnauthorized {
				t.Errorf("%s status = %d, want 401", p, code)
			}
		}
	})
}

// A binaries dir left over from another build must never be served: the
// manifest has to name the commit this server was built from.
func TestDaemonUpdateRefusesMismatchedBuild(t *testing.T) {
	for name, serverCommit := range map[string]string{
		"different commit": "fedcba9876543210fedcba9876543210fedcba98",
		"unstamped server": "unknown",
	} {
		t.Run(name, func(t *testing.T) {
			writeDaemonBinaries(t, updateTestCommit)
			srv := updateTestServer(t, serverCommit)
			for _, p := range []string{"/api/daemon/update", "/api/daemon/update/binary?os=linux&arch=amd64"} {
				if code, _ := updateGet(t, srv.URL+p, true); code != http.StatusNotFound {
					t.Errorf("%s status = %d, want 404", p, code)
				}
			}
		})
	}
}

// install.sh is public (a fresh host has no token yet, `curl | bash`); the
// manifest and binary it calls stay behind DaemonAuth.
func TestDaemonInstallScriptIsPublicAndNothingElseIs(t *testing.T) {
	writeDaemonBinaries(t, updateTestCommit)
	srv := updateTestServer(t, updateTestCommit)

	code, body := updateGet(t, srv.URL+"/api/daemon/install.sh", false)
	if code != http.StatusOK {
		t.Fatalf("install.sh anonymous status = %d, want 200", code)
	}
	if !strings.HasPrefix(string(body), "#!/usr/bin/env bash") || !strings.Contains(string(body), srv.URL) {
		t.Fatalf("install.sh body is not the script for %s:\n%.200s", srv.URL, body)
	}
	for _, p := range []string{"/api/daemon/update", "/api/daemon/update/binary?os=linux&arch=amd64", "/api/daemon/workspaces"} {
		if code, _ := updateGet(t, srv.URL+p, false); code != http.StatusUnauthorized {
			t.Errorf("%s anonymous status = %d, want 401", p, code)
		}
	}
}
