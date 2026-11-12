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

func TestCurrentReadToken(t *testing.T) {
	t.Parallel()
	r, f, _ := readRig(t, nil)
	ctx := context.Background()
	a, err := CurrentReadToken(ctx, r.socket)
	if err != nil || !f.isLive(a) {
		t.Fatalf("first call must mint a live token: %v", err)
	}
	if b, err := CurrentReadToken(ctx, r.socket); err != nil || b != a || len(f.minted) != 1 {
		t.Fatalf("a fresh token must be reused: %v, minted %d", err, len(f.minted))
	}
}

// Within the margin, the broker replaces the token and revokes the old one.
func TestCurrentReadTokenRefreshesNearExpiry(t *testing.T) {
	t.Parallel()
	r, f, _ := readRig(t, func(p *Pin) { p.Tokens.ReadMargin = 2 * time.Hour })
	ctx := context.Background()
	a, err := CurrentReadToken(ctx, r.socket)
	if err != nil {
		t.Fatal(err)
	}
	n, err := CurrentReadToken(ctx, r.socket)
	if err != nil || n == a || len(f.minted) != 2 {
		t.Fatalf("an expiring token must be refreshed: %v, minted %d", err, len(f.minted))
	}
	if f.isLive(a) || !f.isLive(n) {
		t.Fatal("the refresh must revoke the previous token")
	}
	if f.liveCount() != 1 {
		t.Fatalf("live %d, want 1", f.liveCount())
	}
}

// Plan 3.9: no token value is written to disk. The client keeps nothing.
func TestCurrentReadTokenWritesNoFile(t *testing.T) {
	r, _, _ := readRig(t, nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(home)
	if _, err := CurrentReadToken(context.Background(), r.socket); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Fatalf("the client wrote %d entries under HOME", len(ents))
	}
}

// A revoke that outlives its retries stays tracked, so Close tries once more.
func TestRefreshRevokeGaveUpRetriedAtClose(t *testing.T) {
	t.Parallel()
	got := make(chan Alert, 4)
	r, f, b := readRig(t, func(p *Pin) {
		p.Tokens.RetryFor = 30 * time.Millisecond
		p.Tokens.Alert = func(a Alert) { got <- a }
	})
	a, err := GetReadToken(context.Background(), r.socket)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failRevoke = -1
	f.mu.Unlock()
	if _, err := RefreshReadToken(context.Background(), r.socket, a.Token); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("no alert after the retries ran out")
	}
	if !f.isLive(a.Token) {
		t.Fatal("setup: the previous token should still be live")
	}
	f.mu.Lock()
	f.failRevoke = 0 // GitHub recovers before the run ends
	f.mu.Unlock()
	b.Close()
	if f.isLive(a.Token) || f.liveCount() != 0 {
		t.Fatalf("Close left a token live (previous live: %v, total %d)", f.isLive(a.Token), f.liveCount())
	}
}

// While revokes fail, every replaced token stays live and tracked. Refreshing
// again and again must stop at the cap and mint nothing more; the run keeps its
// newest token.
func TestRefreshDuringRevokeOutageStaysUnderCap(t *testing.T) {
	t.Parallel()
	r, f, _ := readRig(t, nil)
	tok, err := GetReadToken(context.Background(), r.socket)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failRevoke = -1
	f.mu.Unlock()
	refused := 0
	for range 2 * maxReadTokens {
		n, err := RefreshReadToken(context.Background(), r.socket, tok.Token)
		if err != nil {
			refused++
			continue
		}
		tok = n
	}
	if f.liveCount() > maxReadTokens {
		t.Fatalf("live %d tokens during the outage, want at most %d", f.liveCount(), maxReadTokens)
	}
	if refused == 0 || len(f.minted) != maxReadTokens {
		t.Fatalf("refused %d, minted %d, want refusals and exactly %d mints", refused, len(f.minted), maxReadTokens)
	}
	if !f.isLive(tok.Token) {
		t.Fatal("the run's newest token must stay live after a refusal")
	}
}

// Forces the ordering where Close sweeps the unrevoked tokens while the retry
// worker is still between "gave up" and "marked as given up" (it is held in the
// Alert callback). The old token must still be revoked before Close returns.
func TestCloseRevokesGivingUpToken(t *testing.T) {
	t.Parallel()
	inAlert, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var f *fakeGitHub
	r, f, b := readRig(t, func(p *Pin) {
		p.Tokens.RetryFor = 30 * time.Millisecond
		p.Tokens.Alert = func(Alert) {
			once.Do(func() {
				f.mu.Lock()
				f.failRevoke = 0 // GitHub recovers before the run ends
				f.mu.Unlock()
				close(inAlert)
			})
			<-release
		}
	})
	a, err := GetReadToken(context.Background(), r.socket)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failRevoke = -1
	f.mu.Unlock()
	cur, err := RefreshReadToken(context.Background(), r.socket, a.Token)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-inAlert:
	case <-time.After(3 * time.Second):
		t.Fatal("retries never ran out")
	}
	closed := make(chan struct{})
	go func() { b.Close(); close(closed) }()
	// Close revokes the current token after it has taken its snapshot of the
	// unrevoked ones, so this marks the point to let the retry worker go on.
	for deadline := time.Now().Add(3 * time.Second); f.isLive(cur.Token); {
		if time.Now().After(deadline) {
			t.Fatal("Close never revoked the current token")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	if f.isLive(a.Token) || f.liveCount() != 0 {
		t.Fatalf("Close left a token live (previous live: %v, total %d)", f.isLive(a.Token), f.liveCount())
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

// Helper process: stands in for `multica git read-token` inside the fake run.
func TestHelperReadToken(t *testing.T) {
	if os.Getenv("BROKER_TEST_HELPER") != "1" {
		t.Skip("helper process")
	}
	tok, err := CurrentReadToken(context.Background(), os.Getenv(SocketEnv))
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString(tok + "\n")
	os.Exit(0)
}

// A fake agent runs `gh` through the installed shims. The real gh stand-in
// sees only the broker's token; a refresh between two calls revokes the first.
func TestFakeAgentRunUsesGHShim(t *testing.T) {
	// Not parallel: scripts are written just before they are run.
	r, f, b := readRig(t, func(p *Pin) { p.Tokens.ReadMargin = 2 * time.Hour })
	dir := t.TempDir()
	multica := filepath.Join(dir, "multica")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(multica, []byte("#!/bin/sh\n[ \"$1 $2\" = \"git read-token\" ] || exit 9\nBROKER_TEST_HELPER=1 exec '"+self+"' -test.run='^TestHelperReadToken$'\n"), 0o755)
	realGH := filepath.Join(dir, "realgh")
	os.WriteFile(realGH, []byte("#!/bin/sh\necho \"$GH_TOKEN\"\n"), 0o755)
	realGit := filepath.Join(dir, "realgit")
	os.WriteFile(realGit, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	shims := t.TempDir()
	if err := InstallShims(shims, realGit, realGH, multica); err != nil {
		t.Fatalf("InstallShims: %v", err)
	}
	agent := filepath.Join(dir, "agent")
	os.WriteFile(agent, []byte("#!/bin/sh\ngh pr view 1\ngh pr view 2\n"), 0o755)
	cmd := exec.Command(agent)
	cmd.Env = []string{"PATH=" + shims + ":/usr/bin:/bin", SocketEnv + "=" + r.socket, "HOME=" + t.TempDir(), "GH_TOKEN=ambient", "GITHUB_TOKEN=ambient"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("agent: %v\n%s", err, out)
	}
	seen := strings.Fields(string(out))
	if len(seen) != 2 || seen[0] == seen[1] || strings.Contains(string(out), "ambient") {
		t.Fatalf("gh saw tokens %q: want two broker tokens, never the ambient one", seen)
	}
	if f.isLive(seen[0]) || !f.isLive(seen[1]) {
		t.Fatal("the first token must be revoked by the refresh, the second live")
	}
	b.Close()
	if f.liveCount() != 0 {
		t.Fatalf("live %d after run end", f.liveCount())
	}
}
