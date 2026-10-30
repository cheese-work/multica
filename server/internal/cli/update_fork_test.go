package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testHead    = "a6f62b95f6fb61a01b1cb5a9a92e91d2ad2bede7"
	testCurrent = "901cfa043c"
	testToken   = "mdt_test"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fakeBinary(commit string) []byte {
	return []byte("#!/bin/sh\necho 'multica 0.0.0-main (commit: " + commit + ", built: now)'\n")
}

// fakeServer answers the two daemon update routes for linux-amd64.
type fakeServer struct {
	commit   string
	sha      string // metadata sha256; defaults to the sha of bin
	size     int64  // metadata size; defaults to len(bin)
	bin      []byte
	status   int // status for both routes when non-zero
	requests []string
	auths    []string
}

func (f *fakeServer) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.RequestURI())
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		if f.status != 0 {
			http.Error(w, "nope", f.status)
			return
		}
		switch r.URL.Path {
		case "/api/daemon/update":
			sha, size := f.sha, f.size
			if sha == "" {
				sha = sha256Hex(f.bin)
			}
			if size == 0 {
				size = int64(len(f.bin))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"commit": f.commit, "version": "0.6.5-test",
				"binaries": map[string]any{"linux-amd64": map[string]any{"sha256": sha, "size": size}},
			})
		case "/api/daemon/update/binary":
			if r.URL.Query().Get("os") != "linux" || r.URL.Query().Get("arch") != "amd64" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(f.bin)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newFake(commit string) *fakeServer {
	return &fakeServer{commit: commit, bin: fakeBinary(commit)}
}

func TestResolveForkUpdateTakesTheServersBuild(t *testing.T) {
	f := newFake(testHead)
	srv := f.serve(t)

	u, err := ResolveForkUpdate(context.Background(), srv.URL, testToken, "linux", "amd64", testCurrent)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if u.HeadSHA != testHead || u.Name != "linux-amd64" || u.Digest != sha256Hex(f.bin) || u.Version != "0.6.5-test" {
		t.Fatalf("update = %+v", u)
	}
	if len(f.auths) != 1 || f.auths[0] != "Bearer "+testToken {
		t.Fatalf("auths = %v", f.auths)
	}
	if strings.Contains(u.String(), testToken) {
		t.Fatal("String() leaks the token")
	}
}

func TestResolveForkUpdateUpToDateWhenCommitMatches(t *testing.T) {
	srv := newFake(testHead).serve(t)
	for _, cur := range []string{testHead, testHead[:7], testHead[:12]} {
		if _, err := ResolveForkUpdate(context.Background(), srv.URL, testToken, "linux", "amd64", cur); !errors.Is(err, ErrForkUpToDate) {
			t.Fatalf("current %q: err = %v", cur, err)
		}
	}
}

// The daemon follows its server, so any different commit is an update, newer or not.
func TestResolveForkUpdateFollowsServerRollback(t *testing.T) {
	srv := newFake(testHead).serve(t)
	if _, err := ResolveForkUpdate(context.Background(), srv.URL, testToken, "linux", "amd64", "ffffffffff"); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveForkUpdateRejectsUnusableOffers(t *testing.T) {
	cases := map[string]struct {
		mutate func(*fakeServer)
		goos   string
		cur    string
		want   string
	}{
		"server has no update routes": {mutate: func(f *fakeServer) { f.status = http.StatusNotFound }, want: "HTTP 404"},
		"server refuses the token":    {mutate: func(f *fakeServer) { f.status = http.StatusUnauthorized }, want: "HTTP 401"},
		"platform not built":          {goos: "darwin", want: "no darwin-amd64 build"},
		"short server commit":         {mutate: func(f *fakeServer) { f.commit = "abc1234" }, want: "server build commit"},
		"bad sha256":                  {mutate: func(f *fakeServer) { f.sha = "xyz" }, want: "no valid sha256"},
		"oversize":                    {mutate: func(f *fakeServer) { f.size = forkMaxBytes + 1 }, want: "out of range"},
		"unusable running commit":     {cur: "unknown", want: "no usable build commit"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(testHead)
			if c.mutate != nil {
				c.mutate(f)
			}
			goos, cur := c.goos, c.cur
			if goos == "" {
				goos = "linux"
			}
			if cur == "" {
				cur = testCurrent
			}
			arch := "amd64"
			_, err := ResolveForkUpdate(context.Background(), f.serve(t).URL, testToken, goos, arch, cur)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
	if _, err := ResolveForkUpdate(context.Background(), "ws://localhost:8080/ws", testToken, "linux", "amd64", testCurrent); err == nil {
		t.Fatal("non-http server URL accepted")
	}
}

func newInstallTarget(t *testing.T) (string, []byte) {
	t.Helper()
	old := []byte("old-binary")
	p := filepath.Join(t.TempDir(), "multica")
	if err := os.WriteFile(p, old, 0o755); err != nil {
		t.Fatal(err)
	}
	return p, old
}

func updateFor(f *fakeServer, srvURL string) *ForkUpdate {
	return &ForkUpdate{ServerURL: srvURL, Token: testToken, HeadSHA: f.commit, Name: "linux-amd64", Digest: sha256Hex(f.bin)}
}

func TestApplyForkUpdateInstallsVerifiedBinary(t *testing.T) {
	f := newFake(testHead)
	srv := f.serve(t)
	exe, _ := newInstallTarget(t)

	out, err := ApplyForkUpdateTo(context.Background(), updateFor(f, srv.URL), time.Minute, exe)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got, _ := os.ReadFile(exe); !bytes.Equal(got, f.bin) {
		t.Fatalf("installed bytes differ")
	}
	if info, _ := os.Stat(exe); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", info.Mode())
	}
	if !strings.Contains(out, testHead) {
		t.Fatalf("output lacks the commit: %s", out)
	}
	if len(f.requests) != 1 || !strings.Contains(f.requests[0], "os=linux") || f.auths[0] != "Bearer "+testToken {
		t.Fatalf("requests = %v auths = %v", f.requests, f.auths)
	}
}

func TestApplyForkUpdateFailuresKeepInstalledBinary(t *testing.T) {
	cases := map[string]struct {
		mutate func(*fakeServer, *ForkUpdate)
		want   string
	}{
		"digest mismatch":     {mutate: func(_ *fakeServer, u *ForkUpdate) { u.Digest = strings.Repeat("0", 64) }, want: "checksum mismatch"},
		"wrong commit":        {mutate: func(f *fakeServer, u *ForkUpdate) { f.bin = fakeBinary("deadbeef"); u.Digest = sha256Hex(f.bin) }, want: "does not report commit"},
		"short reported hash": {mutate: func(f *fakeServer, u *ForkUpdate) { f.bin = fakeBinary("a6f62"); u.Digest = sha256Hex(f.bin) }, want: "does not report commit"},
		"not executable": {mutate: func(f *fakeServer, u *ForkUpdate) {
			f.bin = []byte("\x7fELF-garbage")
			u.Digest = sha256Hex(f.bin)
		}, want: "does not run"},
		"server error": {mutate: func(f *fakeServer, _ *ForkUpdate) { f.status = http.StatusInternalServerError }, want: "HTTP 500"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(testHead)
			srv := f.serve(t)
			u := updateFor(f, srv.URL)
			c.mutate(f, u)
			exe, old := newInstallTarget(t)

			_, err := ApplyForkUpdateTo(context.Background(), u, time.Minute, exe)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
			if got, _ := os.ReadFile(exe); !bytes.Equal(got, old) {
				t.Fatalf("installed binary was modified")
			}
		})
	}
}

func TestForkUpdateSupported(t *testing.T) {
	for goos, want := range map[string]bool{"linux": true, "darwin": true, "windows": false} {
		if ForkUpdateSupported(goos) != want {
			t.Fatalf("ForkUpdateSupported(%s) != %v", goos, want)
		}
	}
}
