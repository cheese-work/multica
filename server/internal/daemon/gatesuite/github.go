// Package gatesuite is the X99 harness of the CHE-2216 gate suite (slice
// C1a): a fake agent that performs actions directly, a fake GitHub that
// asserts token scope and revocation, and a fake provider endpoint. Tests
// only; nothing here talks to a real service or runs a real agent CLI.
package gatesuite

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Perms is a GitHub permission set as sent in a token request.
type Perms = map[string]string

// The permission sets of plan CHE-2216 section 3.9.
var (
	PushPerms   = Perms{"contents": "write"}
	PRPushPerms = Perms{"contents": "write", "pull_requests": "write"}
	ReadPerms   = Perms{"contents": "read", "pull_requests": "read", "issues": "read", "checks": "read", "actions": "read", "metadata": "read"}
)

type GitHubOptions struct {
	Repos  map[string]int64 // "owner/name" to numeric repository ID
	Remote string           // root of the bare repositories, <root>/<owner>/<name>.git; empty: no git endpoint
}

// Mint is one installation-token request as GitHub received it. The token
// value stays inside the fake; tests refer to a token by Seq.
type Mint struct {
	Seq     int
	RepoIDs []int64
	Perms   Perms
}

type mintRec struct {
	Mint
	token   string
	revoked bool
}

// GitHub stands in for api.github.com and the git smart-HTTP remote. Every
// call that carries a token is checked against that token's repositories and
// permissions, so a refusal comes from the token scope like on GitHub.
type GitHub struct {
	URL            string
	AppID          string
	InstallationID int64
	Key            *rsa.PrivateKey

	repos map[string]int64
	mu    sync.Mutex
	mints []*mintRec
	calls []string
}

func NewGitHub(t testing.TB, o GitHubOptions) *GitHub {
	t.Helper()
	g := &GitHub{AppID: "123", InstallationID: 77, Key: appKey(t), repos: o.Repos}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", g.handleMint)
	mux.HandleFunc("DELETE /installation/token", g.handleRevoke)
	mux.HandleFunc("/repos/{owner}/{repo}/{rest...}", g.handleAPI)
	if o.Remote != "" {
		git, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		h := &cgi.Handler{Path: git, Root: "/git", Args: []string{"http-backend"}, Stderr: io.Discard,
			Env: []string{"GIT_PROJECT_ROOT=" + o.Remote, "GIT_HTTP_EXPORT_ALL=1"}, InheritEnv: []string{"PATH", "HOME"}}
		mux.HandleFunc("/git/{owner}/{repo}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
			if g.authorize(w, r, strings.TrimSuffix(r.PathValue("repo"), ".git"), gitNeeds(r)) {
				h.ServeHTTP(w, r)
			}
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	g.URL = srv.URL
	return g
}

var (
	keyOnce sync.Once
	key     *rsa.PrivateKey
	keyErr  error
)

// appKey is one throwaway App key for the whole test binary: generating 2048
// bits takes seconds under -race.
func appKey(t testing.TB) *rsa.PrivateKey {
	keyOnce.Do(func() { key, keyErr = rsa.GenerateKey(rand.Reader, 2048) })
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	return key
}

// NewBareRepo creates <root>/<repo>.git and returns its path.
func NewBareRepo(t testing.TB, root, repo string) string {
	t.Helper()
	dir := filepath.Join(root, repo+".git")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "config", "http.receivepack", "true").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	return dir
}

// GitURL is the smart-HTTP URL of a repository; a client adds the token as
// the password of user x-access-token.
func (g *GitHub) GitURL(repo string) string { return g.URL + "/git/" + repo + ".git" }

func (g *GitHub) handleMint(w http.ResponseWriter, r *http.Request) {
	if err := g.verifyAppJWT(bearer(r)); err != nil {
		http.Error(w, "bad app jwt: "+err.Error(), http.StatusUnauthorized)
		return
	}
	var req struct {
		RepositoryIDs []int64 `json:"repository_ids"`
		Permissions   Perms   `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	seq := len(g.mints) + 1
	rec := &mintRec{Mint: Mint{Seq: seq, RepoIDs: req.RepositoryIDs, Perms: req.Permissions}, token: fmt.Sprintf("ghs_fake%d", seq)}
	g.mints = append(g.mints, rec)
	g.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"token": rec.token, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
}

// verifyAppJWT checks the RS256 signature and issuer of the App JWT by hand.
// This is a GitHub App credential, not a Multica session token, so it does not
// go through the session-generation check that the repo guards on jwt.Parse.
func (g *GitHub) verifyAppJWT(tok string) error {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return errors.New("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&g.Key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var claims struct{ Iss string }
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Iss != g.AppID {
		return errors.New("wrong issuer")
	}
	return nil
}

func (g *GitHub) handleRevoke(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.find(bearer(r))
	if rec == nil {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
		return
	}
	rec.revoked = true
	g.calls = append(g.calls, fmt.Sprintf("DELETE /installation/token %d", http.StatusNoContent))
	w.WriteHeader(http.StatusNoContent)
}

// find returns the live mint holding tok. The caller holds g.mu.
func (g *GitHub) find(tok string) *mintRec {
	for _, m := range g.mints {
		if m.token == tok && !m.revoked {
			return m
		}
	}
	return nil
}

// need is one permission requirement: any name in anyOf at the right level.
// write needs "write"; otherwise "read" or "write" will do.
type need struct {
	anyOf []string
	write bool
}

func (n need) met(m *mintRec) bool {
	return slices.ContainsFunc(n.anyOf, func(name string) bool {
		return m.Perms[name] == "write" || (!n.write && m.Perms[name] == "read")
	})
}

func gitNeeds(r *http.Request) []need {
	return []need{{[]string{"contents"}, isPush(r)}}
}

// apiNeeds maps an API path to the permission GitHub asks for it. An
// endpoint the fake does not know needs "metadata" at the call's level, which
// no write token holds, so an unknown write is refused.
func apiNeeds(r *http.Request) []need {
	write := r.Method != http.MethodGet
	rest := strings.Split(r.PathValue("rest"), "/")
	if write && len(rest) == 3 && rest[0] == "pulls" && rest[2] == "merge" {
		return []need{{[]string{"contents"}, true}}
	}
	anyOf, ok := map[string][]string{
		"pulls": {"pull_requests"}, "issues": {"issues", "pull_requests"}, "actions": {"actions"},
		"check-runs": {"checks"}, "check-suites": {"checks"}, "contents": {"contents"}, "git": {"contents"}, "commits": {"contents"},
	}[rest[0]]
	if !ok {
		anyOf = []string{"metadata"}
	}
	return []need{{anyOf, write}}
}

// authorize answers 401 for a missing, unknown or revoked token, 404 for a
// repository outside the token, and 403 when the permissions do not cover
// what the call needs, and reports whether the call may go on.
func (g *GitHub) authorize(w http.ResponseWriter, r *http.Request, repo string, needs []need) bool {
	tok := bearer(r)
	if _, pass, ok := r.BasicAuth(); ok {
		tok = pass
	}
	full := r.PathValue("owner") + "/" + repo
	g.mu.Lock()
	defer g.mu.Unlock()
	status := http.StatusOK
	switch m := g.find(tok); {
	case m == nil:
		status = http.StatusUnauthorized
	case !slices.Contains(m.RepoIDs, g.repos[full]):
		status = http.StatusNotFound
	case slices.ContainsFunc(needs, func(n need) bool { return !n.met(m) }):
		status = http.StatusForbidden
	}
	g.calls = append(g.calls, fmt.Sprintf("%s /%s %d", r.Method, strings.TrimPrefix(r.URL.Path, "/"), status))
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`) // git retries with credentials only after a challenge
	}
	if status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return false
	}
	return true
}

func isPush(r *http.Request) bool {
	return r.URL.Query().Get("service") == "git-receive-pack" || strings.HasSuffix(r.URL.Path, "/git-receive-pack")
}

// handleAPI answers 200 {} to every call that passes the scope check, so a
// merge the token may not do is refused by scope alone.
func (g *GitHub) handleAPI(w http.ResponseWriter, r *http.Request) {
	if g.authorize(w, r, r.PathValue("repo"), apiNeeds(r)) {
		_, _ = io.WriteString(w, "{}")
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	for _, p := range []string{"Bearer ", "token "} {
		if s, ok := strings.CutPrefix(h, p); ok {
			return s
		}
	}
	return ""
}

// Token returns the value of mint seq, for a test to hand to the code under
// test. Never print it.
func (g *GitHub) Token(seq int) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mints[seq-1].token
}

func (g *GitHub) Mints() []Mint {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Mint, len(g.mints))
	for i, m := range g.mints {
		out[i] = m.Mint
	}
	return out
}

// Calls lists every token-carrying request as "METHOD /path STATUS", in order.
func (g *GitHub) Calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// Violations lists every mint that did not ask for exactly repoID and one of
// the allowed permission sets.
func (g *GitHub) Violations(repoID int64, allowed ...Perms) []string {
	var out []string
	for _, m := range g.Mints() {
		if !slices.Equal(m.RepoIDs, []int64{repoID}) {
			out = append(out, fmt.Sprintf("mint %d: repositories %v, want [%d]", m.Seq, m.RepoIDs, repoID))
		}
		if !slices.ContainsFunc(allowed, func(p Perms) bool { return maps.Equal(p, m.Perms) }) {
			out = append(out, fmt.Sprintf("mint %d: permissions %v not in the allowed sets", m.Seq, m.Perms))
		}
	}
	return out
}

// Unrevoked lists the Seq of every mint with no matching DELETE /installation/token.
func (g *GitHub) Unrevoked() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []int
	for _, m := range g.mints {
		if !m.revoked {
			out = append(out, m.Seq)
		}
	}
	return out
}

// AssertClean fails t on any scope violation or any unrevoked mint.
func (g *GitHub) AssertClean(t testing.TB, repoID int64, allowed ...Perms) {
	t.Helper()
	for _, v := range g.Violations(repoID, allowed...) {
		t.Error(v)
	}
	if u := g.Unrevoked(); len(u) > 0 {
		t.Errorf("mints never revoked: %v", u)
	}
}
