package cli

import (
	"archive/zip"
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
	testRunID   = 36978960011
	testArtID   = 11215971428
)

func goodRun() map[string]any {
	return map[string]any{
		"id": testRunID, "html_url": "https://example/run", "path": forkWorkflowPath,
		"status": "completed", "conclusion": "success", "event": "push", "head_branch": "main", "head_sha": testHead,
		"repository":      map[string]any{"full_name": ForkRepo},
		"head_repository": map[string]any{"full_name": ForkRepo},
	}
}

func goodArtifact(name, digest string) map[string]any {
	return map[string]any{
		"id": testArtID, "name": name, "expired": false, "digest": digest,
		"workflow_run": map[string]any{"id": testRunID, "head_branch": "main", "head_sha": testHead, "repository_id": 1, "head_repository_id": 1},
	}
}

func sha256Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// fakeGitHub serves the three endpoints the resolver uses plus the artifact zip.
type fakeGitHub struct {
	runs      []map[string]any
	artifacts []map[string]any
	compare   string // compare status; "" => 404
	zip       []byte
	gotAuth   []string
	paths     []string
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.paths = append(f.paths, r.URL.RequestURI())
		f.gotAuth = append(f.gotAuth, r.Header.Get("Authorization"))
		base := "/repos/" + ForkRepo
		switch {
		case r.URL.Path == base+"/actions/workflows/ci.yml/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": f.runs})
		case strings.HasSuffix(r.URL.Path, "/artifacts") && strings.Contains(r.URL.Path, "/actions/runs/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": f.artifacts})
		case strings.HasPrefix(r.URL.Path, base+"/compare/"):
			if f.compare == "" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": f.compare})
		case strings.HasSuffix(r.URL.Path, "/zip"):
			_, _ = w.Write(f.zip)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv(releaseAPIBaseURLEnv, srv.URL)
	t.Setenv(forkTokenEnv, "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	return srv
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{
		runs:      []map[string]any{goodRun()},
		artifacts: []map[string]any{goodArtifact("multica-daemon-linux-amd64", sha256Digest([]byte("x")))},
		compare:   "ahead",
	}
	f.serve(t)
	return f
}

func TestResolveForkUpdateSelectsEligibleMainArtifact(t *testing.T) {
	f := newFake(t)
	t.Setenv(forkTokenEnv, "tok-123")

	u, err := ResolveForkUpdate(context.Background(), "linux", "amd64", testCurrent)
	if err != nil {
		t.Fatalf("ResolveForkUpdate: %v", err)
	}
	want := ForkUpdate{RunID: testRunID, RunURL: "https://example/run", HeadSHA: testHead, ArtifactID: testArtID,
		Name: "multica-daemon-linux-amd64", Digest: strings.TrimPrefix(sha256Digest([]byte("x")), "sha256:")}
	if *u != want {
		t.Fatalf("got %+v, want %+v", *u, want)
	}
	if got := f.paths[0]; !strings.Contains(got, "branch=main") || !strings.Contains(got, "event=push") || !strings.Contains(got, "status=success") {
		t.Fatalf("run query not restricted to successful main pushes: %s", got)
	}
	for _, a := range f.gotAuth {
		if a != "Bearer tok-123" {
			t.Fatalf("Authorization = %q", a)
		}
	}
}

func TestResolveForkUpdateRejectsIneligibleRuns(t *testing.T) {
	mutate := map[string]func(map[string]any){
		"pull request":   func(r map[string]any) { r["event"] = "pull_request" },
		"failed":         func(r map[string]any) { r["conclusion"] = "failure" },
		"cancelled":      func(r map[string]any) { r["conclusion"] = "cancelled" },
		"unfinished":     func(r map[string]any) { r["status"] = "in_progress"; r["conclusion"] = nil },
		"other branch":   func(r map[string]any) { r["head_branch"] = "agent/x" },
		"other workflow": func(r map[string]any) { r["path"] = ".github/workflows/cd-deploy.yml" },
		"other repo":     func(r map[string]any) { r["repository"] = map[string]any{"full_name": "multica-ai/multica"} },
		"fork head repo": func(r map[string]any) { r["head_repository"] = map[string]any{"full_name": "evil/multica"} },
		"short sha":      func(r map[string]any) { r["head_sha"] = "abc" },
	}
	for name, m := range mutate {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			m(f.runs[0])
			if _, err := ResolveForkUpdate(context.Background(), "linux", "amd64", testCurrent); !errors.Is(err, ErrForkNoArtifact) {
				t.Fatalf("err = %v, want ErrForkNoArtifact", err)
			}
		})
	}
	t.Run("no runs", func(t *testing.T) {
		f := newFake(t)
		f.runs = nil
		if _, err := ResolveForkUpdate(context.Background(), "linux", "amd64", testCurrent); !errors.Is(err, ErrForkNoArtifact) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestResolveForkUpdateRejectsIneligibleArtifacts(t *testing.T) {
	mutate := map[string]func(map[string]any){
		"expired":           func(a map[string]any) { a["expired"] = true },
		"wrong name":        func(a map[string]any) { a["name"] = "multica-daemon-linux-arm64" },
		"other run":         func(a map[string]any) { a["workflow_run"].(map[string]any)["id"] = 1 },
		"other commit":      func(a map[string]any) { a["workflow_run"].(map[string]any)["head_sha"] = strings.Repeat("b", 40) },
		"other branch":      func(a map[string]any) { a["workflow_run"].(map[string]any)["head_branch"] = "x" },
		"head repo differs": func(a map[string]any) { a["workflow_run"].(map[string]any)["head_repository_id"] = 2 },
		"no digest":         func(a map[string]any) { a["digest"] = nil },
		"non-sha256 digest": func(a map[string]any) { a["digest"] = "sha1:abcd" },
		"short digest":      func(a map[string]any) { a["digest"] = "sha256:abcd" },
	}
	for name, m := range mutate {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			m(f.artifacts[0])
			if _, err := ResolveForkUpdate(context.Background(), "linux", "amd64", testCurrent); !errors.Is(err, ErrForkNoArtifact) {
				t.Fatalf("err = %v, want ErrForkNoArtifact", err)
			}
		})
	}
}

// macOS daemon CI is paused, so CI publishes no darwin artifact: the resolver
// must report unavailable (not fall back) and say why. Mocked — real macOS
// execution is NOT-RUN.
func TestResolveForkUpdateMacOSUnavailableWhilePaused(t *testing.T) {
	newFake(t) // only the linux artifact exists
	for _, arch := range []string{"arm64", "amd64"} {
		_, err := ResolveForkUpdate(context.Background(), "darwin", arch, testCurrent)
		if !errors.Is(err, ErrForkNoArtifact) || !strings.Contains(err.Error(), "macOS daemon CI is paused") {
			t.Fatalf("darwin/%s: err = %v", arch, err)
		}
	}
}

// Same contract as Linux once a darwin artifact does exist.
func TestResolveForkUpdateMacOSSelectsArtifactWhenPublished(t *testing.T) {
	f := newFake(t)
	f.artifacts = []map[string]any{goodArtifact("multica-daemon-darwin-arm64", sha256Digest([]byte("x")))}
	u, err := ResolveForkUpdate(context.Background(), "darwin", "arm64", testCurrent)
	if err != nil || u.Name != "multica-daemon-darwin-arm64" {
		t.Fatalf("u=%v err=%v", u, err)
	}
}

func TestResolveForkUpdateOrdering(t *testing.T) {
	cases := []struct {
		name, compare, current string
		want                   error
	}{
		{"identical", "identical", testCurrent, ErrForkUpToDate},
		{"same commit by prefix", "ahead", testHead[:10], ErrForkUpToDate},
		{"behind", "behind", testCurrent, ErrForkNotNewer},
		{"diverged", "diverged", testCurrent, ErrForkNotNewer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			f.compare = c.compare
			if _, err := ResolveForkUpdate(context.Background(), "linux", "amd64", c.current); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	t.Run("running commit unknown to fork", func(t *testing.T) {
		f := newFake(t)
		f.compare = ""
		_, err := ResolveForkUpdate(context.Background(), "linux", "amd64", testCurrent)
		if err == nil || !strings.Contains(err.Error(), "install-cli-from-ref.sh") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no build commit", func(t *testing.T) {
		newFake(t)
		for _, c := range []string{"unknown", "", "dev", "abc"} {
			if _, err := ResolveForkUpdate(context.Background(), "linux", "amd64", c); err == nil {
				t.Fatalf("commit %q accepted", c)
			}
		}
	})
}

func zipOf(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, b := range entries {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(b)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fakeBinary(commit string) []byte {
	return []byte("#!/bin/sh\necho 'multica 0.0.0-main (commit: " + commit + ", built: now)'\n")
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

func forkUpdateFor(zipData []byte) *ForkUpdate {
	return &ForkUpdate{RunID: testRunID, HeadSHA: testHead, ArtifactID: testArtID, Name: "multica-daemon-linux-amd64",
		Digest: strings.TrimPrefix(sha256Digest(zipData), "sha256:")}
}

func TestApplyForkUpdateInstallsVerifiedArtifact(t *testing.T) {
	f := newFake(t)
	bin := fakeBinary(testHead)
	f.zip = zipOf(t, map[string][]byte{"multica": bin})
	exe, _ := newInstallTarget(t)

	out, err := applyForkUpdateTo(context.Background(), forkUpdateFor(f.zip), time.Minute, exe)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, bin) {
		t.Fatalf("installed bytes differ")
	}
	if info, _ := os.Stat(exe); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", info.Mode())
	}
	if !strings.Contains(out, testHead) || !strings.Contains(out, "11215971428") {
		t.Fatalf("output lacks provenance: %s", out)
	}
}

func TestApplyForkUpdateFailuresKeepInstalledBinary(t *testing.T) {
	good := zipOf(t, map[string][]byte{"multica": fakeBinary(testHead)})
	cases := map[string]struct {
		zip    []byte
		digest string // overrides ForkUpdate.Digest when set
		want   string
	}{
		"digest mismatch": {zip: good, digest: strings.Repeat("0", 64), want: "checksum mismatch"},
		"traversal entry": {zip: zipOf(t, map[string][]byte{"../multica": fakeBinary(testHead)}), want: "unexpected entry"},
		"nested entry":    {zip: zipOf(t, map[string][]byte{"bin/multica": fakeBinary(testHead)}), want: "unexpected entry"},
		"extra entry":     {zip: zipOf(t, map[string][]byte{"multica": fakeBinary(testHead), "evil": []byte("x")}), want: "exactly one entry"},
		"empty entry":     {zip: zipOf(t, map[string][]byte{"multica": nil}), want: "out of range"},
		"not a zip":       {zip: []byte("not a zip"), want: "zip reader"},
		"wrong commit":    {zip: zipOf(t, map[string][]byte{"multica": fakeBinary("deadbeef")}), want: "does not report commit"},
		"not executable":  {zip: zipOf(t, map[string][]byte{"multica": []byte("\x7fELF-garbage")}), want: "does not run"},
		"short commit":    {zip: zipOf(t, map[string][]byte{"multica": fakeBinary("a6f62")}), want: "does not report commit"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.zip = c.zip
			exe, old := newInstallTarget(t)
			u := forkUpdateFor(c.zip)
			if c.digest != "" {
				u.Digest = c.digest
			}
			_, err := applyForkUpdateTo(context.Background(), u, time.Minute, exe)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
			if got, _ := os.ReadFile(exe); !bytes.Equal(got, old) {
				t.Fatalf("installed binary was modified")
			}
		})
	}
	t.Run("expired artifact", func(t *testing.T) {
		f := newFake(t)
		_ = f
		exe, old := newInstallTarget(t)
		u := forkUpdateFor(good)
		u.ArtifactID = 1 // server 404s any non-zip path below
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		t.Setenv(releaseAPIBaseURLEnv, srv.URL)
		if _, err := applyForkUpdateTo(context.Background(), u, time.Minute, exe); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("err = %v", err)
		}
		if got, _ := os.ReadFile(exe); !bytes.Equal(got, old) {
			t.Fatalf("installed binary was modified")
		}
	})
}

// The API token must never reach the artifact blob host the zip endpoint redirects to.
func TestForkDownloadDropsTokenOnCrossHostRedirect(t *testing.T) {
	good := zipOf(t, map[string][]byte{"multica": fakeBinary(testHead)})
	var blobAuth string
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blobAuth = r.Header.Get("Authorization")
		_, _ = w.Write(good)
	}))
	defer blob.Close()
	// 127.0.0.1 vs localhost: same server, different host string.
	blobURL := strings.Replace(blob.URL, "127.0.0.1", "localhost", 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-secret" {
			t.Errorf("API call missing token")
		}
		http.Redirect(w, r, blobURL, http.StatusFound)
	}))
	defer api.Close()
	t.Setenv(releaseAPIBaseURLEnv, api.URL)
	t.Setenv(forkTokenEnv, "tok-secret")
	exe, _ := newInstallTarget(t)

	if _, err := applyForkUpdateTo(context.Background(), forkUpdateFor(good), time.Minute, exe); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if blobAuth != "" {
		t.Fatalf("token leaked to blob host: %q", blobAuth)
	}
}

func TestForkUpdateSupported(t *testing.T) {
	for goos, want := range map[string]bool{"linux": true, "darwin": true, "windows": false} {
		if ForkUpdateSupported(goos) != want {
			t.Fatalf("ForkUpdateSupported(%s) != %v", goos, want)
		}
	}
}
