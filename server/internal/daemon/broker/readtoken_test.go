package broker

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Restricted read token (CHE-2216 slice A4a; plan 3.9, T16).

var wantReadPerms = map[string]string{
	"contents": "read", "pull_requests": "read", "issues": "read",
	"checks": "read", "actions": "read", "metadata": "read",
}

// readRig starts a broker with GitHub access and returns it, so a test can end the run.
func readRig(t *testing.T, mut func(*Pin)) (*rig, *fakeGitHub, *Broker) {
	t.Helper()
	r := newRig(t)
	f := newFakeGitHub(t, r.remote)
	r.pin.RemoteURL, r.pin.Slug, r.pin.Bases, r.pin.Tokens = f.srv.URL+"/git/remote.git", "o/r", []string{"main"}, f.tokens()
	if mut != nil {
		mut(&r.pin)
	}
	b, err := New(r.pin, r.socket, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return r, f, b
}

func TestReadTokenScopedAndRevoked(t *testing.T) {
	t.Parallel()
	r, f, b := readRig(t, nil)
	f.addPR("agent/x/task-1", 42)
	tok, err := GetReadToken(context.Background(), r.socket)
	if err != nil {
		t.Fatalf("GetReadToken: %v", err)
	}
	if len(f.mints) != 1 || !reflect.DeepEqual(f.mints[0], mintReq{RepositoryIDs: []int64{42}, Permissions: wantReadPerms}) {
		t.Fatalf("mint requests %+v, want one repository and exactly %v", f.mints, wantReadPerms)
	}
	if !f.isLive(tok.Token) || tok.ExpiresAt.IsZero() {
		t.Fatalf("token %+v not live, or no expiry", tok)
	}
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/repos/o/r/pulls/1", nil)
	req.Header.Set("Authorization", "token "+tok.Token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("read with the token during the run: %v %v", resp, err)
	}
	b.Close() // run end
	if f.liveCount() != 0 || !reflect.DeepEqual(f.revoked, f.minted) {
		t.Fatalf("minted %v, revoked %v, live %d: the token must die with the run", f.minted, f.revoked, f.liveCount())
	}
}

func TestAllReadTokensRevoked(t *testing.T) {
	t.Parallel()
	r, f, b := readRig(t, nil)
	for range 3 {
		if _, err := GetReadToken(context.Background(), r.socket); err != nil {
			t.Fatal(err)
		}
	}
	b.Close()
	if len(f.revoked) != 3 || f.liveCount() != 0 {
		t.Fatalf("revoked %d of 3, live %d", len(f.revoked), f.liveCount())
	}
}

func TestReadTokenNeverLogged(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	r := newRig(t)
	f := newFakeGitHub(t, r.remote)
	r.pin.RemoteURL, r.pin.Slug, r.pin.Bases, r.pin.Tokens = f.srv.URL+"/git/remote.git", "o/r", []string{"main"}, f.tokens()
	b, err := New(r.pin, r.socket, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := GetReadToken(context.Background(), r.socket)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	if strings.Contains(logs.String(), tok.Token) || !strings.Contains(logs.String(), "token minted") {
		t.Fatalf("log must name the mint by hash prefix only:\n%s", logs.String())
	}
}

func TestReadTokenRefusals(t *testing.T) {
	t.Parallel()
	t.Run("no GitHub access", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		b := r.start(t)
		defer b.Close()
		if _, err := GetReadToken(context.Background(), r.socket); err == nil || !strings.Contains(err.Error(), "501") {
			t.Fatalf("err %v, want 501", err)
		}
	})
	t.Run("elevated", func(t *testing.T) {
		t.Parallel()
		r, f, _ := readRig(t, func(p *Pin) { p.Elevation = &Elevation{Authority: &fakeAuthority{}, Scope: "repo_write:42"} })
		if _, err := GetReadToken(context.Background(), r.socket); err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("err %v, want 403", err)
		}
		if len(f.minted) != 0 {
			t.Fatalf("minted %v for an elevated run", f.minted)
		}
	})
	t.Run("mint failure", func(t *testing.T) {
		t.Parallel()
		r, f, _ := readRig(t, nil)
		f.failMint = 1
		if _, err := GetReadToken(context.Background(), r.socket); err == nil || !strings.Contains(err.Error(), "502") {
			t.Fatalf("err %v, want 502", err)
		}
		if f.liveCount() != 0 {
			t.Fatal("live token after a failed mint")
		}
	})
	t.Run("closed broker", func(t *testing.T) {
		t.Parallel()
		r, f, b := readRig(t, nil)
		b.Close()
		if _, err := GetReadToken(context.Background(), r.socket); err == nil {
			t.Fatal("token from a closed broker")
		}
		if len(f.minted) != 0 {
			t.Fatalf("minted %v after Close", f.minted)
		}
	})
}

func TestReadTokenRevokeAlerts(t *testing.T) {
	t.Parallel()
	got := make(chan Alert, 1)
	r, f, b := readRig(t, func(p *Pin) {
		p.Tokens.RetryFor = 30 * time.Millisecond
		p.Tokens.Alert = func(a Alert) { got <- a }
	})
	if _, err := GetReadToken(context.Background(), r.socket); err != nil {
		t.Fatal(err)
	}
	f.failRevoke = -1
	b.Close()
	select {
	case a := <-got:
		if a.Err == nil || a.ExpiresAt.IsZero() || strings.Contains(a.TokenSHA256, "ghs_") {
			t.Fatalf("alert %+v", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no alert for a read token left live")
	}
}

func TestReadTokenCap(t *testing.T) {
	t.Parallel()
	r, f, _ := readRig(t, nil)
	for range maxReadTokens {
		if _, err := GetReadToken(context.Background(), r.socket); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := GetReadToken(context.Background(), r.socket); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err %v, want 429 past %d tokens", err, maxReadTokens)
	}
	if len(f.minted) != maxReadTokens {
		t.Fatalf("minted %d, want %d", len(f.minted), maxReadTokens)
	}
}

// A stalled revoke endpoint must not stretch shutdown by one timeout per
// token, and every token left live must raise an alert.
func TestReadTokenStalledRevokeIsBounded(t *testing.T) {
	t.Parallel()
	const timeout = 300 * time.Millisecond
	got := make(chan Alert, maxReadTokens)
	r, f, b := readRig(t, func(p *Pin) {
		p.Tokens.RevokeTimeout = timeout
		p.Tokens.RetryFor = time.Minute
		p.Tokens.Alert = func(a Alert) { got <- a }
	})
	for range maxReadTokens {
		if _, err := GetReadToken(context.Background(), r.socket); err != nil {
			t.Fatal(err)
		}
	}
	f.stallRevoke = true
	start := time.Now()
	b.Close()
	// Serial revocation needs at least maxReadTokens*timeout for the first pass alone.
	if took := time.Since(start); took >= (maxReadTokens-1)*timeout+timeout/2 {
		t.Fatalf("Close took %v with %d stalled tokens, want well under %v", took, maxReadTokens, maxReadTokens*timeout)
	}
	if len(got) != maxReadTokens {
		t.Fatalf("%d alerts, want %d", len(got), maxReadTokens)
	}
}
