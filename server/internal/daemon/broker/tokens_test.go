package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// tokenRig is the A3a rig with the remote served over HTTP by the fake GitHub.
func tokenRig(t testing.TB, mut func(*Pin)) (*rig, *fakeGitHub, *bytes.Buffer) {
	t.Helper()
	r := newRig(t)
	f := newFakeGitHub(t, r.remote)
	r.pin.RemoteURL = f.srv.URL + "/git/remote.git"
	r.pin.Tokens = f.tokens()
	r.pin.Slug = "o/r"
	r.pin.Bases = []string{"main"}
	if mut != nil {
		mut(&r.pin)
	}
	var logs bytes.Buffer
	b, err := New(r.pin, r.socket, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return r, f, &logs
}

func TestPushTokenIsOneRepositoryAndRevoked(t *testing.T) {
	t.Parallel()
	r, f, logs := tokenRig(t, nil)
	r.commits(t, 2)
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got, want := r.remoteRef(t, r.pin.Ref), git(t, r.work, "rev-parse", "HEAD"); got != want {
		t.Fatalf("remote %s = %q, want %q", r.pin.Ref, got, want)
	}
	want := mintReq{RepositoryIDs: []int64{42}, Permissions: map[string]string{"contents": "write"}}
	if len(f.mints) != 1 || !reflect.DeepEqual(f.mints[0], want) {
		t.Fatalf("mint requests %+v, want exactly %+v", f.mints, want)
	}
	if !reflect.DeepEqual(f.revoked, f.minted) || f.liveCount() != 0 {
		t.Fatalf("minted %v, revoked %v, live %d: every token must be revoked", f.minted, f.revoked, f.liveCount())
	}
	if strings.Contains(logs.String(), f.minted[0]) {
		t.Fatal("log holds a token value")
	}
	sum := sha256.Sum256([]byte(f.minted[0]))
	if !strings.Contains(logs.String(), hex.EncodeToString(sum[:])[:8]) {
		t.Fatalf("log lacks the token hash prefix:\n%s", logs)
	}
}

func TestPushFailureStillRevokes(t *testing.T) {
	t.Parallel()
	r, f, _ := tokenRig(t, nil)
	r.commits(t, 1)
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err != nil {
		t.Fatal(err)
	}
	git(t, r.work, "reset", "--hard", r.base)
	r.commits(t, 2) // forked history: the remote refuses it
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err == nil {
		t.Fatal("non-fast-forward push succeeded")
	}
	if len(f.minted) != 2 || len(f.revoked) != 2 || f.liveCount() != 0 {
		t.Fatalf("minted %d, revoked %d, live %d", len(f.minted), len(f.revoked), f.liveCount())
	}
}

func TestRefusedPushMintsNothing(t *testing.T) {
	t.Parallel()
	r, f, _ := tokenRig(t, nil)
	if code, _ := r.post(t, "ref=refs/heads/main", strings.NewReader("junk")); code != 403 {
		t.Fatalf("status %d, want 403", code)
	}
	if len(f.minted) != 0 {
		t.Fatalf("minted %v for a refused push", f.minted)
	}
}

func TestMintFailureFailsPush(t *testing.T) {
	t.Parallel()
	r, f, _ := tokenRig(t, nil)
	r.commits(t, 1)
	f.failMint = 1
	if err := Push(context.Background(), r.socket, r.work, io.Discard); err == nil {
		t.Fatal("push succeeded without a token")
	}
	if got := r.remoteRef(t, r.pin.Ref); got != "" {
		t.Fatalf("remote gained %s", got)
	}
}

func TestOperationDeadlineAbortsAndRevokes(t *testing.T) {
	t.Parallel()
	// The deadline starts at mint, so a slow host cannot eat it; the hook
	// holds the push open for four times as long.
	r, f, _ := tokenRig(t, func(p *Pin) { p.Deadline = time.Second })
	hook := filepath.Join(r.remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.commits(t, 1)
	err := Push(context.Background(), r.socket, r.work, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("push outlived its deadline: %v", err)
	}
	if len(f.revoked) != 1 || f.liveCount() != 0 {
		t.Fatalf("revoked %v, live %d after a timed-out push", f.revoked, f.liveCount())
	}
}

func TestRevokeRetriesThenAlerts(t *testing.T) {
	t.Parallel()
	t.Run("recovers", func(t *testing.T) {
		t.Parallel()
		r, f, _ := tokenRig(t, nil)
		var alerts int
		var mu sync.Mutex
		r.pin.Tokens.Alert = func(Alert) { mu.Lock(); alerts++; mu.Unlock() }
		f.failRevoke = 2
		r.commits(t, 1)
		if err := Push(context.Background(), r.socket, r.work, io.Discard); err != nil {
			t.Fatalf("a revoke failure must not fail the push: %v", err)
		}
		waitFor(t, func() bool { return f.liveCount() == 0 })
		mu.Lock()
		defer mu.Unlock()
		if alerts != 0 {
			t.Fatalf("%d alerts for a revoke that recovered", alerts)
		}
	})
	t.Run("gives up", func(t *testing.T) {
		t.Parallel()
		got := make(chan Alert, 1)
		r, f, _ := tokenRig(t, func(p *Pin) {
			p.Tokens.RetryFor = 30 * time.Millisecond
			p.Tokens.Alert = func(a Alert) { got <- a }
		})
		f.failRevoke = -1
		r.commits(t, 1)
		if err := Push(context.Background(), r.socket, r.work, io.Discard); err != nil {
			t.Fatal(err)
		}
		select {
		case a := <-got:
			f.mu.Lock()
			defer f.mu.Unlock()
			if exp := f.live[f.minted[0]]; !a.ExpiresAt.Equal(exp) || a.Repo != "42" || a.Err == nil {
				t.Fatalf("alert %+v, want expiry %v and repo 42", a, exp)
			}
			if strings.Contains(a.TokenSHA256, "ghs_") {
				t.Fatal("alert holds a token value")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no alert after the retry window")
		}
	})
	t.Run("close alerts", func(t *testing.T) {
		t.Parallel()
		got := make(chan Alert, 1)
		r := newRig(t)
		f := newFakeGitHub(t, r.remote)
		r.pin.RemoteURL, r.pin.Slug, r.pin.Bases = f.srv.URL+"/git/remote.git", "o/r", []string{"main"}
		r.pin.Tokens = f.tokens()
		r.pin.Tokens.RetryFor = time.Hour
		r.pin.Tokens.Alert = func(a Alert) { got <- a }
		b, err := New(r.pin, r.socket, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.failRevoke = -1
		r.commits(t, 1)
		if err := Push(context.Background(), r.socket, r.work, io.Discard); err != nil {
			t.Fatal(err)
		}
		b.Close()
		select {
		case <-got:
		case <-time.After(3 * time.Second):
			t.Fatal("Close dropped a live token without an alert")
		}
	})
}

func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met in 3s")
}
