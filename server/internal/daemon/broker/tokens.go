package broker

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// operationDeadline bounds one broker operation (plan 3.9). It is the
// authority lease, not a token lifetime: GitHub fixes that at one hour.
const operationDeadline = 10 * time.Minute

// maxReadTokens bounds the read tokens one run may hold at once, so run-end
// revocation has a fixed amount of work.
const maxReadTokens = 4

// Tokens is how a broker gets and drops its GitHub credential (plan 3.9). The
// token lives in broker memory for one operation and never enters the sandbox.
type Tokens struct {
	APIBase        string // https://api.github.com
	AppID          string
	InstallationID int64
	Key            *rsa.PrivateKey
	HTTP           *http.Client // nil means http.DefaultClient
	// Alert is told when a token could not be revoked before RetryFor ran out
	// or the broker closed. The broker also logs it.
	Alert func(Alert)
	// RetryBase and RetryFor shape the revoke retry; zero means 1s and 5 minutes.
	RetryBase, RetryFor time.Duration
	// RevokeTimeout bounds one revoke call; zero means 15s.
	RevokeTimeout time.Duration
	// ReadMargin is how close to expiry the run's read token is replaced; zero means 10 minutes.
	ReadMargin time.Duration
}

// Alert says which token may still be live and until when. TokenSHA256 is a
// short hash prefix, never the token.
type Alert struct {
	Repo, TokenSHA256 string
	ExpiresAt         time.Time
	Err               error
}

type grant struct {
	token   string
	expires time.Time
}

// id is the log-safe name of a token.
func (g grant) id() string {
	sum := sha256.Sum256([]byte(g.token))
	return hex.EncodeToString(sum[:])[:8]
}

func (t *Tokens) client() *http.Client {
	if t.HTTP != nil {
		return t.HTTP
	}
	return http.DefaultClient
}

func (t *Tokens) call(ctx context.Context, method, path, auth string, body any) (*http.Response, error) {
	var in bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&in).Encode(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.APIBase, "/")+path, &in)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+auth)
	return t.client().Do(req)
}

// readPerms is the restricted read token (plan 3.9): read-only, no write.
var readPerms = map[string]string{
	"contents": "read", "pull_requests": "read", "issues": "read",
	"checks": "read", "actions": "read", "metadata": "read",
}

// mint asks for a token naming one repository with exactly perms.
func (t *Tokens) mint(ctx context.Context, repoID int64, perms map[string]string) (grant, error) {
	now := time.Now()
	appJWT, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(), // clock skew
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": t.AppID,
	}).SignedString(t.Key)
	if err != nil {
		return grant{}, errors.New("sign App JWT failed")
	}
	resp, err := t.call(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", t.InstallationID), appJWT,
		map[string]any{"repository_ids": []int64{repoID}, "permissions": perms})
	if err != nil {
		return grant{}, fmt.Errorf("mint token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return grant{}, fmt.Errorf("mint token: status %d", resp.StatusCode)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		return grant{}, errors.New("mint token: malformed response")
	}
	return grant{out.Token, out.ExpiresAt}, nil
}

// drop calls DELETE /installation/token. A 401 means the token is already
// dead, which is what we want.
func (t *Tokens) drop(ctx context.Context, g grant) error {
	resp, err := t.call(ctx, http.MethodDelete, "/installation/token", g.token, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("revoke token: status %d", resp.StatusCode)
	}
	return nil
}

// revoke drops g right after the operation, success or failure. A failure
// retries in the background with doubling backoff for RetryFor, then alerts
// with the expiry time. Close cuts the retry short and alerts the same way.
func (b *Broker) revoke(g grant) {
	t := b.pin.Tokens
	err := b.dropOnce(t, g)
	if err == nil {
		return
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.retryRevoke(t, g, err)
	}()
}

func (b *Broker) dropOnce(t *Tokens, g grant) error {
	ctx, cancel := context.WithTimeout(context.Background(), or(t.RevokeTimeout, 15*time.Second)) // not b.bg: Close still gets a last try
	defer cancel()
	err := t.drop(ctx, g)
	if err == nil {
		b.settle(g)
		b.log.Info("broker token revoked", "repo", b.pin.Repo, "token", g.id())
	}
	return err
}

func (b *Broker) retryRevoke(t *Tokens, g grant, err error) {
	wait, giveUp := or(t.RetryBase, time.Second), time.NewTimer(or(t.RetryFor, 5*time.Minute))
	defer giveUp.Stop()
	for {
		b.log.Warn("broker token revoke failed, retrying", "repo", b.pin.Repo, "token", g.id(), "error", err)
		select {
		case <-time.After(wait):
			if err = b.dropOnce(t, g); err == nil {
				return
			}
			wait = min(wait*2, 30*time.Second)
		case <-giveUp.C:
			b.markAlerted(g)
			b.alert(t, g, err)
			return // still tracked: Close tries once more, after this worker has ended
		case <-b.bg.Done(): // one last try before the broker goes away
			if err = b.dropOnce(t, g); err != nil {
				b.markAlerted(g)
				b.alert(t, g, err)
			}
			return
		}
	}
}

func (b *Broker) alert(t *Tokens, g grant, err error) {
	a := Alert{Repo: b.pin.Repo, TokenSHA256: g.id(), ExpiresAt: g.expires, Err: err}
	b.log.Error("broker token not revoked", "repo", a.Repo, "token", a.TokenSHA256, "expires_at", a.ExpiresAt, "error", err)
	if t.Alert != nil {
		t.Alert(a)
	}
}

func or(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

// readTokenAllowed refuses a run that may not hold a read token. The token is
// the one credential that enters the sandbox (plan 3.2). An elevated run reads
// through the read proxy instead, so it gets none.
func (b *Broker) readTokenAllowed(w http.ResponseWriter) bool {
	switch {
	case b.pin.Tokens == nil:
		http.Error(w, "this run has no GitHub access", http.StatusNotImplemented)
		return false
	case b.pin.Elevation != nil:
		http.Error(w, "an elevated run reads through the read proxy", http.StatusForbidden)
		return false
	}
	return true
}

// handleReadToken hands the run a read-only token for its own repository. The
// broker keeps its own copy and revokes it when the run ends.
func (b *Broker) handleReadToken(w http.ResponseWriter, r *http.Request) {
	if !b.readTokenAllowed(w) {
		return
	}
	if !b.reserveRead() {
		http.Error(w, "this run holds the most read tokens it may", http.StatusTooManyRequests)
		return
	}
	g, status, err := b.mintWith(r.Context(), readPerms)
	if err != nil {
		b.rdmu.Lock()
		b.pending--
		b.rdmu.Unlock()
		b.log.Warn("broker read token", "task", b.pin.Task, "repo", b.pin.Repo, "status", status, "error", err)
		http.Error(w, err.Error(), status)
		return
	}
	b.rdmu.Lock()
	b.pending--
	b.reads = append(b.reads, g)
	b.rdmu.Unlock()
	_ = json.NewEncoder(w).Encode(ReadToken{Token: g.token, ExpiresAt: g.expires})
}

// staleRead is a replaced read token that GitHub may still accept. It stays
// tracked until a revoke succeeds. Close makes the last attempt, after every
// retry worker has ended, so the worker and Close never race for the token.
type staleRead struct {
	g       grant
	alerted bool // the expiry alert is out; Close does not repeat it
}

func (b *Broker) settle(g grant) {
	b.rdmu.Lock()
	delete(b.unrevoked, g.id())
	b.rdmu.Unlock()
}

func (b *Broker) markAlerted(g grant) {
	b.rdmu.Lock()
	if s := b.unrevoked[g.id()]; s != nil {
		s.alerted = true
	}
	b.rdmu.Unlock()
}

// handleReadRefresh replaces the read token named in staleTokenHeader (plan
// 3.9: revoked on every refresh of the previous token). The new token is
// minted first, so a failed mint leaves the old one working; the old one is
// revoked before the reply, so a reply means the old token is dead. Refreshes
// run one at a time: a caller whose stale token is already gone gets the
// newest token instead of causing a third mint. A refresh needs a free slot
// (the old token holds its own until its revoke succeeds); with none free it
// answers 429 and mints nothing, and the caller keeps its current token.
func (b *Broker) handleReadRefresh(w http.ResponseWriter, r *http.Request) {
	if !b.readTokenAllowed(w) {
		return
	}
	b.refmu.Lock()
	defer b.refmu.Unlock()
	b.refreshRead(w, r, r.Header.Get(staleTokenHeader))
}

// handleReadCurrent hands the run its read token and keeps it in the broker's
// memory only (plan 3.9: no token value on disk). The token is reused until it
// is within ReadMargin of expiry; then it is replaced and the old one revoked.
func (b *Broker) handleReadCurrent(w http.ResponseWriter, r *http.Request) {
	if !b.readTokenAllowed(w) {
		return
	}
	b.refmu.Lock()
	defer b.refmu.Unlock()
	stale := ""
	if g, ok := b.newestRead(); ok {
		if time.Until(g.expires) > or(b.pin.Tokens.ReadMargin, 10*time.Minute) {
			_ = json.NewEncoder(w).Encode(ReadToken{Token: g.token, ExpiresAt: g.expires})
			return
		}
		stale = g.id()
	}
	b.refreshRead(w, r, stale)
}

// refreshRead mints a replacement and revokes stale. The caller holds refmu.
func (b *Broker) refreshRead(w http.ResponseWriter, r *http.Request, stale string) {
	idx := b.readIndex(stale)
	if stale != "" && idx < 0 {
		if g, ok := b.newestRead(); ok { // another caller already replaced it
			_ = json.NewEncoder(w).Encode(ReadToken{Token: g.token, ExpiresAt: g.expires})
			return
		}
	}
	if !b.reserveRead() { // a swap needs a slot too: the old token stays live until its revoke succeeds
		http.Error(w, "this run holds the most read tokens it may", http.StatusTooManyRequests)
		return
	}
	g, status, err := b.mintWith(r.Context(), readPerms)
	b.rdmu.Lock()
	b.pending--
	if err != nil {
		b.rdmu.Unlock()
		b.log.Warn("broker read token refresh", "task", b.pin.Task, "repo", b.pin.Repo, "status", status, "error", err)
		http.Error(w, err.Error(), status)
		return
	}
	var old *grant
	if i := b.readIndexLocked(stale); i >= 0 {
		o := b.reads[i]
		old = &o
		b.reads = append(b.reads[:i], b.reads[i+1:]...)
		if b.unrevoked == nil { // tracked until a revoke succeeds, so Close can retry it
			b.unrevoked = map[string]*staleRead{}
		}
		b.unrevoked[o.id()] = &staleRead{g: o}
	}
	b.reads = append(b.reads, g)
	b.rdmu.Unlock()
	if old != nil {
		b.revoke(*old) // a failure retries in the background, then alerts; the refresh already succeeded
	}
	_ = json.NewEncoder(w).Encode(ReadToken{Token: g.token, ExpiresAt: g.expires})
}

func (b *Broker) readIndex(id string) int {
	b.rdmu.Lock()
	defer b.rdmu.Unlock()
	return b.readIndexLocked(id)
}

func (b *Broker) readIndexLocked(id string) int {
	if id == "" {
		return -1
	}
	for i, g := range b.reads {
		if g.id() == id {
			return i
		}
	}
	return -1
}

func (b *Broker) newestRead() (grant, bool) {
	b.rdmu.Lock()
	defer b.rdmu.Unlock()
	if len(b.reads) == 0 {
		return grant{}, false
	}
	return b.reads[len(b.reads)-1], true
}

// reserveRead takes one of the maxReadTokens slots before a mint starts, so
// concurrent requests cannot pass the cap together. A replaced token whose
// revoke has not succeeded still holds its slot, so a revoke outage cannot
// grow the number of live tokens.
func (b *Broker) reserveRead() bool {
	b.rdmu.Lock()
	defer b.rdmu.Unlock()
	if len(b.reads)+len(b.unrevoked)+b.pending >= maxReadTokens {
		return false
	}
	b.pending++
	return true
}

// revokeReads drops every read token handed out, all at once, so a stalled
// GitHub costs one timeout and not one per token. Close calls it after the
// last request has finished, so no new token can appear behind it.
func (b *Broker) revokeReads() {
	b.rdmu.Lock()
	reads := b.reads
	b.reads = nil
	b.rdmu.Unlock()
	var wg sync.WaitGroup
	for _, g := range reads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.revoke(g)
		}()
	}
	wg.Wait()
}

// revokeLeftovers makes one last try at every replaced token still tracked.
// Close calls it after the retry workers have ended: whatever is left here
// outlived its retries, and nothing else can still be revoking it.
func (b *Broker) revokeLeftovers() {
	b.rdmu.Lock()
	left := make([]*staleRead, 0, len(b.unrevoked))
	for _, s := range b.unrevoked {
		left = append(left, s)
	}
	b.rdmu.Unlock()
	t := b.pin.Tokens
	var wg sync.WaitGroup
	for _, s := range left {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.dropOnce(t, s.g); err != nil && !s.alerted {
				b.alert(t, s.g, err)
			}
		}()
	}
	wg.Wait()
}
