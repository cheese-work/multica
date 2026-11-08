package broker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Restricted read token refresh (CHE-2216 slice A4b; plan 3.9, T16 refresh rows).

func TestRefreshRevokesPrevious(t *testing.T) {
	t.Parallel()
	r, f, b := readRig(t, nil)
	a, err := GetReadToken(context.Background(), r.socket)
	if err != nil {
		t.Fatal(err)
	}
	n, err := RefreshReadToken(context.Background(), r.socket, a.Token)
	if err != nil {
		t.Fatalf("RefreshReadToken: %v", err)
	}
	if n.Token == a.Token || !f.isLive(n.Token) {
		t.Fatalf("refresh returned the old token, or the new one is not live")
	}
	if f.isLive(a.Token) {
		t.Fatal("the previous token is still live after a refresh")
	}
	for _, m := range f.mints {
		if len(m.RepositoryIDs) != 1 || m.RepositoryIDs[0] != 42 || len(m.Permissions) != len(wantReadPerms) {
			t.Fatalf("mint %+v, want one repository and the read permissions only", m)
		}
	}
	b.Close()
	if f.liveCount() != 0 || len(f.revoked) != 2 {
		t.Fatalf("revoked %v, live %d: exactly the old token then the new one", f.revoked, f.liveCount())
	}
}

func TestRefreshHoldsOneToken(t *testing.T) {
	t.Parallel()
	r, f, _ := readRig(t, nil)
	tok, err := GetReadToken(context.Background(), r.socket)
	if err != nil {
		t.Fatal(err)
	}
	for range maxReadTokens + 2 { // refresh must not consume cap slots
		if tok, err = RefreshReadToken(context.Background(), r.socket, tok.Token); err != nil {
			t.Fatal(err)
		}
	}
	if f.liveCount() != 1 || !f.isLive(tok.Token) {
		t.Fatalf("live %d, want only the newest token", f.liveCount())
	}
}

// Several gh processes may see the same token as stale at once. One mints; the
// others get that token and nobody is left holding a revoked one.
func TestRefreshConcurrentIsIdempotent(t *testing.T) {
	t.Parallel()
	r, f, _ := readRig(t, nil)
	a, err := GetReadToken(context.Background(), r.socket)
	if err != nil {
		t.Fatal(err)
	}
	const n = 6
	got := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := RefreshReadToken(context.Background(), r.socket, a.Token)
			if err != nil {
				t.Error(err)
			}
			got[i] = tok.Token
		}()
	}
	wg.Wait()
	for _, g := range got {
		if g != got[0] || !f.isLive(g) {
			t.Fatalf("callers got %v: all must share one live token", got)
		}
	}
	if len(f.minted) != 2 || f.liveCount() != 1 {
		t.Fatalf("minted %d, live %d, want 2 and 1", len(f.minted), f.liveCount())
	}
}

func TestRefreshMintFailureKeepsOld(t *testing.T) {
	t.Parallel()
	r, f, _ := readRig(t, nil)
	a, err := GetReadToken(context.Background(), r.socket)
	if err != nil {
		t.Fatal(err)
	}
	f.failMint = 1
	if _, err := RefreshReadToken(context.Background(), r.socket, a.Token); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err %v, want 502", err)
	}
	if !f.isLive(a.Token) {
		t.Fatal("a failed refresh must not revoke the token the run still holds")
	}
	if n, err := RefreshReadToken(context.Background(), r.socket, a.Token); err != nil || f.isLive(a.Token) || !f.isLive(n.Token) {
		t.Fatalf("retry after failure: %v", err)
	}
}

func TestRefreshWithoutTokenMints(t *testing.T) {
	t.Parallel()
	r, f, _ := readRig(t, nil)
	tok, err := RefreshReadToken(context.Background(), r.socket, "")
	if err != nil || !f.isLive(tok.Token) {
		t.Fatalf("refresh with no previous token: %v", err)
	}
}

func TestRefreshRefusals(t *testing.T) {
	t.Parallel()
	t.Run("no GitHub access", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		defer r.start(t).Close()
		if _, err := RefreshReadToken(context.Background(), r.socket, ""); err == nil || !strings.Contains(err.Error(), "501") {
			t.Fatalf("err %v, want 501", err)
		}
	})
	t.Run("elevated", func(t *testing.T) {
		t.Parallel()
		r, f, _ := readRig(t, func(p *Pin) { p.Elevation = &Elevation{Authority: &fakeAuthority{}, Scope: "repo_write:42"} })
		if _, err := RefreshReadToken(context.Background(), r.socket, ""); err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("err %v, want 403", err)
		}
		if len(f.minted) != 0 {
			t.Fatalf("minted %v for an elevated run", f.minted)
		}
	})
}

func TestRefreshRevokeFailureAlerts(t *testing.T) {
	t.Parallel()
	got := make(chan Alert, 2)
	r, f, _ := readRig(t, func(p *Pin) {
		p.Tokens.RetryFor = 30 * time.Millisecond
		p.Tokens.Alert = func(a Alert) { got <- a }
	})
	a, err := GetReadToken(context.Background(), r.socket)
	if err != nil {
		t.Fatal(err)
	}
	f.failRevoke = -1
	n, err := RefreshReadToken(context.Background(), r.socket, a.Token)
	if err != nil || n.Token == a.Token {
		t.Fatalf("a revoke failure must not fail the refresh: %v", err)
	}
	select {
	case al := <-got:
		if al.Err == nil || al.ExpiresAt.IsZero() || strings.Contains(al.TokenSHA256, "ghs_") {
			t.Fatalf("alert %+v", al)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no alert for the previous token left live")
	}
}

func TestCachedReadToken(t *testing.T) {
	t.Parallel()
	r, f, _ := readRig(t, nil)
	cache := filepath.Join(t.TempDir(), "read-token")
	ctx := context.Background()
	a, err := CachedReadToken(ctx, r.socket, cache, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(cache); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache %v %v, want mode 0600", fi, err)
	}
	if b, err := CachedReadToken(ctx, r.socket, cache, 10*time.Minute); err != nil || b != a || len(f.minted) != 1 {
		t.Fatalf("a fresh token must be reused: %v, minted %d", err, len(f.minted))
	}
	// Near expiry: within the margin, the token is refreshed and the old one dies.
	n, err := CachedReadToken(ctx, r.socket, cache, 2*time.Hour)
	if err != nil || n == a || len(f.minted) != 2 {
		t.Fatalf("expiring token must be refreshed: %v, minted %d", err, len(f.minted))
	}
	if f.isLive(a) || !f.isLive(n) {
		t.Fatal("refresh through the cache must revoke the previous token")
	}
	if c, err := CachedReadToken(ctx, r.socket, cache, 10*time.Minute); err != nil || c != n {
		t.Fatalf("cache must hold the new token: %v", err)
	}
}

func TestCachedReadTokenGarbageCache(t *testing.T) {
	t.Parallel()
	r, f, _ := readRig(t, nil)
	cache := filepath.Join(t.TempDir(), "read-token")
	if err := os.WriteFile(cache, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := CachedReadToken(context.Background(), r.socket, cache, time.Minute)
	if err != nil || !f.isLive(tok) {
		t.Fatalf("a corrupt cache must be replaced: %v", err)
	}
}

func TestWriteGHShim(t *testing.T) {
	// Not parallel: a fork while a script is still open for writing fails with "text file busy".
	dir := t.TempDir()
	fakeGH := filepath.Join(dir, "realgh")
	fakeMultica := filepath.Join(dir, "multica")
	os.WriteFile(fakeGH, []byte("#!/bin/sh\necho \"token=$GH_TOKEN args=$*\"\n"), 0o755)
	os.WriteFile(fakeMultica, []byte("#!/bin/sh\n[ \"$1 $2\" = \"git read-token\" ] || exit 9\necho ghs_fromcache\n"), 0o755)
	path, err := WriteGHShim(dir, fakeGH, fakeMultica)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(path, "pr", "view", "1").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "token=ghs_fromcache args=pr view 1" {
		t.Fatalf("shim output %q err %v", out, err)
	}
	// No token: gh must not run, and never with an ambient credential.
	os.WriteFile(fakeMultica, []byte("#!/bin/sh\necho refused >&2\nexit 1\n"), 0o755)
	cmd := exec.Command(path, "pr", "view", "1")
	cmd.Env = append(os.Environ(), "GH_TOKEN=ambient", "GITHUB_TOKEN=ambient")
	out, err = cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "token=") {
		t.Fatalf("shim ran gh without a broker token: %q %v", out, err)
	}
	for _, bad := range []string{"relative/gh", "/x/it's/gh"} {
		if _, err := WriteGHShim(dir, bad, fakeMultica); err == nil {
			t.Fatalf("accepted path %q", bad)
		}
	}
}
