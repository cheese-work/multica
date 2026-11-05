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
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// operationDeadline bounds one broker operation (plan 3.9). It is the
// authority lease, not a token lifetime: GitHub fixes that at one hour.
const operationDeadline = 10 * time.Minute

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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second) // not b.bg: Close still gets a last try
	defer cancel()
	err := t.drop(ctx, g)
	if err == nil {
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
			b.alert(t, g, err)
			return
		case <-b.bg.Done(): // one last try before the broker goes away
			if err = b.dropOnce(t, g); err != nil {
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

// handleReadToken hands the run a read-only token for its own repository. The
// token is the one credential that enters the sandbox (plan 3.2); the broker
// keeps its own copy and revokes it when the run ends. An elevated run reads
// through the read proxy instead, so it gets none.
func (b *Broker) handleReadToken(w http.ResponseWriter, r *http.Request) {
	switch {
	case b.pin.Tokens == nil:
		http.Error(w, "this run has no GitHub access", http.StatusNotImplemented)
		return
	case b.pin.Elevation != nil:
		http.Error(w, "an elevated run reads through the read proxy", http.StatusForbidden)
		return
	}
	g, status, err := b.mintWith(r.Context(), readPerms)
	if err != nil {
		b.log.Warn("broker read token", "task", b.pin.Task, "repo", b.pin.Repo, "status", status, "error", err)
		http.Error(w, err.Error(), status)
		return
	}
	b.rdmu.Lock()
	b.reads = append(b.reads, g)
	b.rdmu.Unlock()
	_ = json.NewEncoder(w).Encode(ReadToken{Token: g.token, ExpiresAt: g.expires})
}

// revokeReads drops every read token handed out. Close calls it after the last
// request has finished, so no new token can appear behind it.
func (b *Broker) revokeReads() {
	b.rdmu.Lock()
	reads := b.reads
	b.reads = nil
	b.rdmu.Unlock()
	for _, g := range reads {
		b.revoke(g)
	}
}
