package broker

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type mintReq struct {
	RepositoryIDs []int64           `json:"repository_ids"`
	Permissions   map[string]string `json:"permissions"`
}

type fakePR struct {
	Number  int
	HeadRef string
	RepoID  int64
	Title   string
	Body    string
	Base    string
	Draft   bool
	Ready   bool
	Comment []string
}

// fakeGitHub stands in for api.github.com and for the git smart-HTTP remote.
// Every call that carries a token is checked against the set of live tokens,
// so a call after revoke fails the way it would on GitHub.
type fakeGitHub struct {
	t       testing.TB
	srv     *httptest.Server
	key     *rsa.PrivateKey
	mu      sync.Mutex
	seq     int
	live    map[string]time.Time
	mints   []mintReq
	minted  []string
	revoked []string
	calls   []string // "METHOD path" of every authorized API call
	prs     map[int]*fakePR
	failMint,
	failRevoke int // next N mint / DELETE calls answer 500; -1 means always
	// revokeSeen, when set, is signalled as a DELETE arrives; the DELETE then
	// waits for revokeGate to close.
	stallRevoke bool // DELETE hangs until the client gives up
	revokeSeen  chan struct{}
	revokeGate  chan struct{}
	// dropReply makes a comment land and then cut the connection before any
	// reply; refuse makes it answer 422 without landing.
	dropReply, refuse bool
	// gqlReply replaces the GraphQL answer; the mutation still applies.
	gqlReply string
}

func newFakeGitHub(t testing.TB, remote string) *fakeGitHub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{t: t, key: key, live: map[string]time.Time{}, prs: map[int]*fakePR{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/77/access_tokens", f.handleMint)
	mux.HandleFunc("DELETE /installation/token", f.handleRevoke)
	mux.HandleFunc("GET /repos/o/r/pulls/{n}", f.api(f.getPR))
	mux.HandleFunc("POST /repos/o/r/pulls", f.api(f.createPR))
	mux.HandleFunc("PATCH /repos/o/r/pulls/{n}", f.api(f.editPR))
	mux.HandleFunc("POST /repos/o/r/issues/{n}/comments", f.api(f.comment))
	mux.HandleFunc("POST /graphql", f.api(f.graphql))
	if remote != "" {
		exec.Command("git", "-C", remote, "config", "http.receivepack", "true").Run()
		git := &cgi.Handler{Path: mustLook(t, "git"), Root: "/git", Args: []string{"http-backend"},
			Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(remote), "GIT_HTTP_EXPORT_ALL=1"}, InheritEnv: []string{"PATH", "HOME"}, Stderr: io.Discard}
		mux.HandleFunc("/git/", func(w http.ResponseWriter, r *http.Request) {
			if user, pass, ok := r.BasicAuth(); !ok || user != "x-access-token" || !f.isLive(pass) {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
			git.ServeHTTP(w, r)
		})
	}
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func mustLook(t testing.TB, name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fakeGitHub) tokens() *Tokens {
	return &Tokens{APIBase: f.srv.URL, AppID: "123", InstallationID: 77, Key: f.key,
		RetryBase: time.Millisecond, RetryFor: time.Second}
}

func (f *fakeGitHub) isLive(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.live[tok]
	return ok
}

func (f *fakeGitHub) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
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

func (f *fakeGitHub) fail(counter *int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if *counter == 0 {
		return false
	}
	if *counter > 0 {
		*counter--
	}
	return true
}

func (f *fakeGitHub) handleMint(w http.ResponseWriter, r *http.Request) {
	_, err := jwt.Parse(bearer(r), func(*jwt.Token) (any, error) { return &f.key.PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer("123"))
	if err != nil {
		http.Error(w, "bad app jwt: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if f.fail(&f.failMint) {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	var req mintReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.seq++
	tok := fmt.Sprintf("ghs_fake%d", f.seq)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	f.live[tok] = exp
	f.mints = append(f.mints, req)
	f.minted = append(f.minted, tok)
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"token": tok, "expires_at": exp.Format(time.RFC3339)})
}

func (f *fakeGitHub) handleRevoke(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	if f.stallRevoke {
		<-r.Context().Done()
		return
	}
	if f.revokeSeen != nil {
		f.revokeSeen <- struct{}{}
		<-f.revokeGate
	}
	if f.fail(&f.failRevoke) {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.live[tok]; !ok {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
		return
	}
	delete(f.live, tok)
	f.revoked = append(f.revoked, tok)
	w.WriteHeader(http.StatusNoContent)
}

// api wraps a handler with the live-token check and the call log.
func (f *fakeGitHub) api(h func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !f.isLive(bearer(r)) {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		h(w, r, body)
	}
}

func (f *fakeGitHub) prJSON(p *fakePR) map[string]any {
	return map[string]any{
		"number": p.Number, "node_id": fmt.Sprintf("PR_node%d", p.Number),
		"html_url": fmt.Sprintf("https://github.com/o/r/pull/%d", p.Number),
		"head":     map[string]any{"ref": p.HeadRef, "repo": map[string]any{"id": p.RepoID}},
	}
}

func (f *fakeGitHub) addPR(headRef string, repoID int64) *fakePR {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &fakePR{Number: len(f.prs) + 1, HeadRef: headRef, RepoID: repoID, Title: "t", Body: "b", Base: "main"}
	f.prs[p.Number] = p
	return p
}

func (f *fakeGitHub) pr(r *http.Request) *fakePR {
	var n int
	fmt.Sscan(r.PathValue("n"), &n)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prs[n]
}

func (f *fakeGitHub) getPR(w http.ResponseWriter, r *http.Request, _ []byte) {
	p := f.pr(r)
	if p == nil {
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(f.prJSON(p))
}

func (f *fakeGitHub) createPR(w http.ResponseWriter, r *http.Request, body []byte) {
	var in struct {
		Title, Body, Head, Base string
		Draft                   bool
	}
	json.Unmarshal(body, &in)
	p := f.addPR(in.Head, 42)
	f.mu.Lock()
	p.Title, p.Body, p.Base, p.Draft = in.Title, in.Body, in.Base, in.Draft
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(f.prJSON(p))
}

func (f *fakeGitHub) editPR(w http.ResponseWriter, r *http.Request, body []byte) {
	var in map[string]any
	json.Unmarshal(body, &in)
	p := f.pr(r)
	f.mu.Lock()
	for k, v := range in {
		switch k {
		case "title":
			p.Title, _ = v.(string)
		case "body":
			p.Body, _ = v.(string)
		default:
			f.mu.Unlock()
			http.Error(w, "unexpected field "+k, http.StatusUnprocessableEntity)
			return
		}
	}
	f.mu.Unlock()
	json.NewEncoder(w).Encode(f.prJSON(p))
}

func (f *fakeGitHub) comment(w http.ResponseWriter, r *http.Request, body []byte) {
	var in struct{ Body string }
	json.Unmarshal(body, &in)
	p := f.pr(r)
	f.mu.Lock()
	refuse, drop := f.refuse, f.dropReply
	if !refuse {
		p.Comment = append(p.Comment, in.Body)
	}
	f.mu.Unlock()
	if refuse {
		http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
		return
	}
	if drop {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"id": 1})
}

// graphql knows one mutation, the only way GitHub marks a draft ready.
func (f *fakeGitHub) graphql(w http.ResponseWriter, _ *http.Request, body []byte) {
	var in struct {
		Query     string
		Variables map[string]string
	}
	json.Unmarshal(body, &in)
	if !strings.Contains(in.Query, "markPullRequestReadyForReview") {
		http.Error(w, "unexpected query", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	reply := f.gqlReply
	for _, p := range f.prs {
		if fmt.Sprintf("PR_node%d", p.Number) == in.Variables["id"] {
			p.Ready = true
		}
	}
	f.mu.Unlock()
	if reply == "" {
		reply = `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"id":"` + in.Variables["id"] + `"}}}}`
	}
	w.Write([]byte(reply))
}

// writeCalls lists the authorized calls that change something on GitHub.
func (f *fakeGitHub) writeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}
