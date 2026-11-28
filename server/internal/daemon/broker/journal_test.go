package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// jFake is a GitHub stand-in for the journal: DELETE /installation/token
// revokes the bearer, GET /rate_limit answers 200 for live tokens and 401
// otherwise. It records the bearer of every DELETE.
type jFake struct {
	srv        *httptest.Server
	mu         sync.Mutex
	live       map[string]bool
	deletes    []string
	requests   int
	failDelete int // next N DELETEs answer 503; -1 means always
	// block, when set for a token, is signalled on arrival of that token's
	// DELETE, which then waits for gate to close.
	blockTok string
	seen     chan struct{}
	gate     chan struct{}
	onDelete func(tok string)
}

func newJFake(t testing.TB) *jFake {
	f := &jFake{live: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /installation/token", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.requests++
		f.deletes = append(f.deletes, tok)
		block := f.blockTok != "" && f.blockTok == tok
		hook := f.onDelete
		f.mu.Unlock()
		if hook != nil {
			hook(tok)
		}
		if block {
			f.seen <- struct{}{}
			<-f.gate
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failDelete != 0 {
			if f.failDelete > 0 {
				f.failDelete--
			}
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if !f.live[tok] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		delete(f.live, tok)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /rate_limit", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		if !f.live[tok] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *jFake) mint(tok string) {
	f.mu.Lock()
	f.live[tok] = true
	f.mu.Unlock()
}

func (f *jFake) deleteCount(tok string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, d := range f.deletes {
		if d == tok {
			n++
		}
	}
	return n
}

func (f *jFake) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// rateLimitStatus is the verification probe a test uses on its own.
func (f *jFake) rateLimitStatus(t testing.TB, tok string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/rate_limit", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openTestJournal(t testing.TB, root, instance string, logs io.Writer, clock *testClock) *Journal {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	o := JournalOptions{Instance: instance, Logger: slog.New(slog.NewTextHandler(logs, nil)), insecureHTTP: true}
	if clock != nil {
		o.Now = clock.now
	}
	j, err := OpenJournal(root, o)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func tokHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])[:8]
}

func entryFor(f *jFake, tok string, ttl time.Duration, now time.Time) JournalEntry {
	return JournalEntry{Token: tok, Expires: now.Add(ttl), APIBase: f.srv.URL, TaskID: "task-1"}
}

func journalRoot(t testing.TB) string {
	return filepath.Join(t.TempDir(), "cred-journal", "default")
}

// entryFiles lists the entry files (not journal.key) in root.
func entryFiles(t testing.TB, root string) []string {
	t.Helper()
	des, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		if de.Name() != "journal.key" && !strings.HasPrefix(de.Name(), ".") {
			out = append(out, filepath.Join(root, de.Name()))
		}
	}
	return out
}

func TestJournalHoldsNoPlaintextToken(t *testing.T) {
	t.Parallel()
	root := journalRoot(t)
	f := newJFake(t)
	j := openTestJournal(t, root, NewInstanceID(), nil, nil)
	toks := []string{"ghs_plaintext_one_aaaaaaaaaaaaaaaaaaaaaaaa", "ghs_plaintext_two_bbbbbbbbbbbbbbbbbbbbbbbb"}
	for _, tok := range toks {
		if err := j.Put(entryFor(f, tok, time.Hour, time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if st, err := os.Stat(root); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("root %v mode %v, want 0700", err, st)
	}
	var files []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, p)
			if info.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v, want 0600", filepath.Base(p), info.Mode().Perm())
			}
			b, _ := os.ReadFile(p)
			for _, tok := range toks {
				if bytes.Contains(b, []byte(tok)) {
					t.Errorf("%s holds a plaintext token", filepath.Base(p))
				}
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 { // two entries and journal.key
		t.Fatalf("files %v, want two entries and journal.key", files)
	}
	key, err := os.ReadFile(filepath.Join(root, "journal.key"))
	if err != nil || len(key) != 32 {
		t.Fatalf("journal.key: %d bytes, err %v, want 32 bytes", len(key), err)
	}
}

func TestSweepDropsExpiredEntries(t *testing.T) {
	t.Parallel()
	root := journalRoot(t)
	f := newJFake(t)
	clock := &testClock{t: time.Now()}
	old := openTestJournal(t, root, NewInstanceID(), nil, clock)
	if err := old.Put(entryFor(f, "ghs_prior_expired_aaaaaaaaaaaaaaaaaaaaa", time.Hour, clock.now())); err != nil {
		t.Fatal(err)
	}
	cur := openTestJournal(t, root, NewInstanceID(), nil, clock)
	if err := cur.Put(entryFor(f, "ghs_held_expired_bbbbbbbbbbbbbbbbbbbbbbbb", time.Hour, clock.now())); err != nil {
		t.Fatal(err)
	}
	clock.add(2 * time.Hour)
	res := cur.Sweep(context.Background())
	if res.ExpiredUnrevoked != 2 {
		t.Fatalf("ExpiredUnrevoked = %d, want 2 (%+v)", res.ExpiredUnrevoked, res)
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("%d requests sent for expired entries, want 0", n)
	}
	if got := entryFiles(t, root); len(got) != 0 {
		t.Fatalf("entries left: %v", got)
	}
	for _, r := range res.Receipts {
		if r.Case != "expired_unrevoked" || len(r.Token) != 8 {
			t.Fatalf("receipt %+v, want case expired_unrevoked and an 8 char token id", r)
		}
	}
	if len(res.Receipts) != 2 {
		t.Fatalf("receipts %+v, want 2", res.Receipts)
	}
}

// testAppKeyEnv names the App key file the J7 mutation derives a seal from.
// Production code never reads it.
const testAppKeyEnv = "JOURNAL_TEST_APP_KEY_FILE"

// J7 is not parallel: it sets the environment variable the mutation reads.
func TestAppKeyRotationDuringOutageThenRecovery(t *testing.T) {
	dir := t.TempDir()
	keyA, keyB := filepath.Join(dir, "app-a.pem"), filepath.Join(dir, "app-b.pem")
	for p, body := range map[string]string{keyA: "app key A", keyB: "app key B"} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := journalRoot(t)
	f := newJFake(t)
	const tok = "ghs_rotation_token_cccccccccccccccccccccc"
	f.mint(tok)

	// Process 1: App key A is configured. The stub minter hands the token to
	// the run; the run ends; every revoke gets 503 for longer than RetryFor.
	t.Setenv(testAppKeyEnv, keyA)
	f.failDelete = -1
	j1 := openTestJournal(t, root, NewInstanceID(), nil, nil)
	if err := j1.Put(entryFor(f, tok, time.Hour, time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := j1.Retire(tok); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(50 * time.Millisecond) // RetryFor
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodDelete, f.srv.URL+"/installation/token", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := j1.Release(tok); err != nil { // retry give-up
		t.Fatal(err)
	}
	if got := entryFiles(t, root); len(got) != 1 {
		t.Fatalf("entries %v, want the unrevoked entry to stay", got)
	}

	// Process 2: only App key B exists; key A's file is gone. The service
	// recovers. The token is its own bearer, so the sweep needs no App key.
	if err := os.Remove(keyA); err != nil {
		t.Fatal(err)
	}
	t.Setenv(testAppKeyEnv, keyB)
	f.mu.Lock()
	f.failDelete = 0
	f.mu.Unlock()
	j2 := openTestJournal(t, root, NewInstanceID(), nil, nil)
	res := j2.Sweep(context.Background())
	if res.Revoked != 1 || res.UnsealFailed != 0 {
		t.Fatalf("sweep %+v, want 1 revoked and 0 unseal_failed", res)
	}
	if got := f.rateLimitStatus(t, tok); got != http.StatusUnauthorized {
		t.Fatalf("GET with the token after recovery: %d, want 401", got)
	}
	if got := entryFiles(t, root); len(got) != 0 {
		t.Fatalf("entries %v, want none after recovery", got)
	}
}

func TestJournalKeyNeverReplacedWhileEntriesExist(t *testing.T) {
	t.Parallel()
	root := journalRoot(t)
	f := newJFake(t)
	clock := &testClock{t: time.Now()}
	j1 := openTestJournal(t, root, NewInstanceID(), nil, clock)
	toks := []string{"ghs_lostkey_one_dddddddddddddddddddddd", "ghs_lostkey_two_eeeeeeeeeeeeeeeeeeeeee"}
	for _, tok := range toks {
		f.mint(tok)
		if err := j1.Put(entryFor(f, tok, time.Hour, clock.now())); err != nil {
			t.Fatal(err)
		}
	}
	keyPath := filepath.Join(root, "journal.key")
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	j2 := openTestJournal(t, root, NewInstanceID(), &logs, clock)
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("opening the journal created a key (stat err %v)", err)
	}
	if err := j2.createKey(); err == nil {
		t.Fatal("createKey replaced the key while entries exist")
	}
	if err := j2.Put(entryFor(f, "ghs_new_after_loss_ffffffffffffffffff", time.Hour, clock.now())); err == nil {
		t.Fatal("Put succeeded without the journal key while entries exist")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatal("a refused Put left a new key behind")
	}
	for i := 0; i < 2; i++ {
		res := j2.Sweep(context.Background())
		if res.UnsealFailed != 2 || res.Revoked != 0 {
			t.Fatalf("sweep %d: %+v, want 2 unseal_failed and 0 revoked", i, res)
		}
	}
	if st := j2.Status(); st.UnsealFailed != 2 || st.Pending != 2 {
		t.Fatalf("status %+v, want 2 pending, 2 unseal_failed", st)
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("%d requests sent without a key, want 0", n)
	}
	if got := entryFiles(t, root); len(got) != 2 {
		t.Fatalf("entries %v, want both kept until expiry", got)
	}
	for _, tok := range toks {
		if n := strings.Count(logs.String(), "level=ERROR"); n == 0 {
			t.Fatal("no error logged")
		}
		if got := strings.Count(logs.String(), "token="+tokHash(tok)); got != 1 {
			t.Fatalf("token %s logged %d times, want once", tokHash(tok), got)
		}
		if strings.Contains(logs.String(), tok) {
			t.Fatal("log holds a token value")
		}
	}
	clock.add(3 * time.Hour)
	res := j2.Sweep(context.Background())
	if res.ExpiredUnrevoked != 2 {
		t.Fatalf("after expiry: %+v, want 2 expired_unrevoked", res)
	}
	if got := entryFiles(t, root); len(got) != 0 {
		t.Fatalf("entries %v, want none after expiry", got)
	}
}

// editHeader rewrites one header field of an entry file without touching the
// sealed body.
func editHeader(t testing.TB, path, field string, value any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	h, ok := m["h"].(map[string]any)
	if !ok {
		t.Fatalf("no header in %s", filepath.Base(path))
	}
	if _, ok := h[field]; !ok {
		t.Fatalf("header has no field %q", field)
	}
	h[field] = value
	out, _ := json.Marshal(m)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTamperedHeaderSendsNothing(t *testing.T) {
	t.Parallel()
	real, evil := newJFake(t), newJFake(t)
	cases := map[string]func(current string) any{
		"base":          func(string) any { return evil.srv.URL },
		"expiry":        func(string) any { return time.Now().Add(48 * time.Hour).Unix() },
		"expiry-past":   func(string) any { return time.Now().Add(-time.Hour).Unix() },
		"instance":      func(string) any { return "00000000000000000000000000000001" },
		"instance-mine": func(current string) any { return current },
	}
	field := map[string]string{"base": "base", "expiry": "exp", "expiry-past": "exp", "instance": "owner", "instance-mine": "owner"}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			root := journalRoot(t)
			tok := "ghs_tamper_" + name + "_gggggggggggggggggggggg"
			real.mint(tok)
			evil.mint(tok)
			j1 := openTestJournal(t, root, NewInstanceID(), nil, nil)
			if err := j1.Put(entryFor(real, tok, time.Hour, time.Now())); err != nil {
				t.Fatal(err)
			}
			files := entryFiles(t, root)
			if len(files) != 1 {
				t.Fatalf("files %v", files)
			}
			current := NewInstanceID()
			editHeader(t, files[0], field[name], edit(current))
			beforeReal, beforeEvil := real.requestCount(), evil.requestCount()
			j2 := openTestJournal(t, root, current, nil, nil)
			res := j2.Sweep(context.Background())
			if res.UnsealFailed != 1 || res.Revoked != 0 || res.ExpiredUnrevoked != 0 || res.Orphan != 0 {
				t.Fatalf("sweep %+v, want 1 unseal_failed", res)
			}
			if real.requestCount() != beforeReal || evil.requestCount() != beforeEvil {
				t.Fatal("a request was sent for an entry with a tampered header")
			}
			if got := entryFiles(t, root); len(got) != 1 {
				t.Fatalf("entries %v, want the entry kept", got)
			}
		})
	}
}

func TestPutRefusesExpiryBeyondTokenLife(t *testing.T) {
	t.Parallel()
	root := journalRoot(t)
	f := newJFake(t)
	j := openTestJournal(t, root, NewInstanceID(), nil, nil)
	if err := j.Put(entryFor(f, "ghs_toolong_iiiiiiiiiiiiiiiiiiiiiiii", 3*time.Hour, time.Now())); err == nil {
		t.Fatal("Put accepted an expiry 3h out")
	}
	if got := entryFiles(t, root); len(got) != 0 {
		t.Fatalf("entries %v after a refused Put", got)
	}
	if err := j.Put(entryFor(f, "ghs_hour_jjjjjjjjjjjjjjjjjjjjjjjjjj", time.Hour, time.Now())); err != nil {
		t.Fatalf("Put of a one-hour token: %v", err)
	}
}

func TestKeyWriteFailureLeavesNoKeyAndRecovers(t *testing.T) {
	t.Parallel()
	fails := map[string]func(*os.File, []byte) error{
		"chmod": func(*os.File, []byte) error { return errors.New("injected chmod failure") },
		"short": func(f *os.File, k []byte) error {
			f.Write(k[:5])
			return errors.New("injected short write")
		},
		"sync": func(f *os.File, k []byte) error {
			f.Write(k)
			return errors.New("injected sync failure")
		},
	}
	for name, fail := range fails {
		t.Run(name, func(t *testing.T) {
			root := journalRoot(t)
			f := newJFake(t)
			tok := "ghs_keyfail_" + name + "_hhhhhhhhhhhhhhhhhhhh"
			f.mint(tok)
			j := openTestJournal(t, root, NewInstanceID(), nil, nil)
			j.writeKey = fail
			if err := j.Put(entryFor(f, tok, time.Hour, time.Now())); err == nil {
				t.Fatal("Put succeeded although the key could not be written")
			}
			if _, err := os.Stat(filepath.Join(root, "journal.key")); !os.IsNotExist(err) {
				t.Fatalf("a failed key write left a key file behind (stat err %v)", err)
			}
			if got := entryFiles(t, root); len(got) != 0 {
				t.Fatalf("entries %v after a refused Put", got)
			}
			j.writeKey = nil
			if err := j.Put(entryFor(f, tok, time.Hour, time.Now())); err != nil {
				t.Fatalf("Put after the failure: %v", err)
			}
			info, err := os.Stat(filepath.Join(root, "journal.key"))
			if err != nil || info.Size() != 32 || info.Mode().Perm() != 0o600 {
				t.Fatalf("key after recovery: %v, %v", info, err)
			}
			if err := j.Retire(tok); err != nil {
				t.Fatal(err)
			}
			if err := j.Release(tok); err != nil {
				t.Fatal(err)
			}
			res := openTestJournal(t, root, NewInstanceID(), nil, nil).Sweep(context.Background())
			if res.Revoked != 1 || f.deleteCount(tok) != 1 {
				t.Fatalf("restart sweep %+v, deletes %d, want 1 revoked", res, f.deleteCount(tok))
			}
		})
	}
}

func TestSweepEligibility(t *testing.T) {
	t.Parallel()
	root := journalRoot(t)
	f := newJFake(t)
	prior := openTestJournal(t, root, NewInstanceID(), nil, nil)
	cur := openTestJournal(t, root, NewInstanceID(), nil, nil)
	const (
		held     = "ghs_elig_held_hhhhhhhhhhhhhhhhhhhhhhhh"
		retiring = "ghs_elig_retiring_rrrrrrrrrrrrrrrrrrrrr"
		sweeping = "ghs_elig_sweeping_ssssssssssssssssssss"
		released = "ghs_elig_released_lllllllllllllllllllll"
		priorTok = "ghs_elig_prior_pppppppppppppppppppppp"
	)
	for _, tok := range []string{held, retiring, sweeping, released, priorTok} {
		f.mint(tok)
	}
	put := func(j *Journal, tok string) {
		if err := j.Put(entryFor(f, tok, time.Hour, time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	toReleased := func(tok string) {
		if err := cur.Retire(tok); err != nil {
			t.Fatal(err)
		}
		if err := cur.Release(tok); err != nil {
			t.Fatal(err)
		}
	}
	put(cur, held)
	put(cur, retiring)
	if err := cur.Retire(retiring); err != nil {
		t.Fatal(err)
	}
	put(cur, sweeping)
	toReleased(sweeping)

	// Sweep A takes the released entry and stalls on its DELETE: that entry
	// is now in state "sweeping".
	f.mu.Lock()
	f.blockTok, f.seen, f.gate = sweeping, make(chan struct{}, 1), make(chan struct{})
	f.mu.Unlock()
	doneA := make(chan SweepResult, 1)
	go func() { doneA <- cur.Sweep(context.Background()) }()
	select {
	case <-f.seen:
	case <-time.After(10 * time.Second):
		t.Fatal("sweep A never reached its DELETE")
	}

	// Now add one released entry and one prior-instance entry, and sweep B.
	put(cur, released)
	toReleased(released)
	put(prior, priorTok)
	resB := cur.Sweep(context.Background())
	if resB.Revoked != 2 {
		t.Fatalf("sweep B %+v, want exactly the released and prior-instance entries revoked", resB)
	}
	for tok, want := range map[string]int{held: 0, retiring: 0, sweeping: 1, released: 1, priorTok: 1} {
		if got := f.deleteCount(tok); got != want {
			t.Errorf("DELETEs for %s = %d, want %d", tokHash(tok), got, want)
		}
	}
	close(f.gate)
	resA := <-doneA
	if resA.Revoked != 1 {
		t.Fatalf("sweep A %+v, want 1 revoked", resA)
	}
	for tok, want := range map[string]int{held: 0, retiring: 0, sweeping: 1, released: 1, priorTok: 1} {
		if got := f.deleteCount(tok); got != want {
			t.Errorf("final DELETEs for %s = %d, want %d", tokHash(tok), got, want)
		}
	}
	if got := entryFiles(t, root); len(got) != 2 {
		t.Fatalf("entries %v, want only the held and retiring ones", got)
	}
}

func TestJournalRejectsIllegalTransitions(t *testing.T) {
	t.Parallel()
	root := journalRoot(t)
	f := newJFake(t)
	j := openTestJournal(t, root, NewInstanceID(), nil, nil)
	const tok = "ghs_edges_iiiiiiiiiiiiiiiiiiiiiiiiiiii"
	if err := j.Retire(tok); err == nil {
		t.Fatal("Retire of an unknown token succeeded")
	}
	if err := j.Put(entryFor(f, tok, time.Hour, time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := j.Put(entryFor(f, tok, time.Hour, time.Now())); err == nil {
		t.Fatal("second Put of the same token succeeded")
	}
	if err := j.Release(tok); err == nil {
		t.Fatal("held -> released succeeded; only held -> retiring -> released is legal")
	}
	if err := j.Retire(tok); err != nil {
		t.Fatal(err)
	}
	if err := j.Retire(tok); err == nil {
		t.Fatal("retiring -> retiring succeeded")
	}
	if err := j.Release(tok); err != nil {
		t.Fatal(err)
	}
	if err := j.Retire(tok); err == nil {
		t.Fatal("released -> retiring succeeded")
	}
	if err := j.Delete(tok); err != nil {
		t.Fatal(err)
	}
	if err := j.Delete(tok); err != nil {
		t.Fatalf("Delete of a deleted entry: %v, want a no-op", err)
	}
	if got := entryFiles(t, root); len(got) != 0 {
		t.Fatalf("entries %v, want none", got)
	}
}

func TestConcurrentPutRetireReleaseSweep(t *testing.T) {
	t.Parallel()
	const goroutines, perGoroutine = 8, 125 // 1,000 rounds
	root := journalRoot(t)
	f := newJFake(t)
	j := openTestJournal(t, root, NewInstanceID(), nil, nil)

	// phase is what the test knows about each token. A transition that makes
	// an entry sweepable is recorded before the call; the others after it.
	var pmu sync.Mutex
	phase := map[string]string{}
	set := func(tok, p string) { pmu.Lock(); phase[tok] = p; pmu.Unlock() }
	var violations atomic.Int64
	var firstViolation atomic.Value
	f.mu.Lock()
	f.onDelete = func(tok string) {
		pmu.Lock()
		p := phase[tok]
		pmu.Unlock()
		if p != "released" && p != "deleting" {
			violations.Add(1)
			firstViolation.CompareAndSwap(nil, fmt.Sprintf("DELETE for %s in phase %q", tokHash(tok), p))
		}
	}
	f.mu.Unlock()

	stop := make(chan struct{})
	var swept sync.WaitGroup
	var orphans, sweeps atomic.Int64
	swept.Add(1)
	go func() {
		defer swept.Done()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				res := j.Sweep(context.Background())
				orphans.Add(int64(res.Orphan))
				sweeps.Add(1)
			}
		}
	}()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				tok := fmt.Sprintf("ghs_race_%d_%d_zzzzzzzzzzzzzzzzzzzzzzzz", g, i)
				f.mint(tok)
				if err := j.Put(entryFor(f, tok, time.Hour, time.Now())); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				set(tok, "held")
				switch i % 3 {
				case 0: // revoked while held
					set(tok, "deleting")
					if err := j.Delete(tok); err != nil {
						t.Errorf("Delete: %v", err)
					}
				case 1: // retiring, then revoked by the broker
					if err := j.Retire(tok); err != nil {
						t.Errorf("Retire: %v", err)
					}
					set(tok, "retiring")
					set(tok, "deleting")
					if err := j.Delete(tok); err != nil {
						t.Errorf("Delete: %v", err)
					}
				default: // given up: the sweeper may take it
					if err := j.Retire(tok); err != nil {
						t.Errorf("Retire: %v", err)
					}
					set(tok, "retiring")
					set(tok, "released")
					if err := j.Release(tok); err != nil {
						t.Errorf("Release: %v", err)
					}
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	swept.Wait()
	// Drain: whatever the sweeper left released is taken now.
	for i := 0; i < 5; i++ {
		res := j.Sweep(context.Background())
		orphans.Add(int64(res.Orphan))
	}

	if v := violations.Load(); v != 0 {
		t.Fatalf("%d DELETEs arrived for a held or retiring token; first: %v", v, firstViolation.Load())
	}
	f.mu.Lock()
	seen := map[string]int{}
	for _, d := range f.deletes {
		seen[d]++
	}
	f.mu.Unlock()
	for tok, n := range seen {
		if n > 1 {
			t.Fatalf("token %s got %d DELETEs", tokHash(tok), n)
		}
	}
	if n := orphans.Load(); n != 0 {
		t.Fatalf("orphan count %d, want 0", n)
	}
	if st := j.Status(); st.Orphan != 0 || st.Pending != 0 {
		t.Fatalf("status %+v, want no orphan and nothing pending", st)
	}
	if sweeps.Load() == 0 {
		t.Fatal("the concurrent sweeper never ran")
	}
}
