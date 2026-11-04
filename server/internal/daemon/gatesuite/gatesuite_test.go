package gatesuite

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestMain(m *testing.M) {
	if code, ok := Main(os.Args[1:]); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

const repoID = 42

func newGH(t *testing.T) (*GitHub, string) {
	t.Helper()
	root := t.TempDir()
	NewBareRepo(t, root, "o/r")
	NewBareRepo(t, root, "o/other")
	return NewGitHub(t, GitHubOptions{Repos: map[string]int64{"o/r": repoID, "o/other": 43}, Remote: root}), root
}

// mint asks for an installation token the way the broker does: App JWT, one body.
func mint(t *testing.T, g *GitHub, repoIDs []int64, perms Perms) int {
	t.Helper()
	jwtStr, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer: g.AppID, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	}).SignedString(g.Key)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"repository_ids": repoIDs, "permissions": perms})
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/app/installations/%d/access_tokens", g.URL, g.InstallationID), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwtStr)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint status %d", resp.StatusCode)
	}
	return len(g.Mints())
}

func api(t *testing.T, g *GitHub, seq int, method, path string) int {
	t.Helper()
	req, _ := http.NewRequest(method, g.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+g.Token(seq))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func revoke(t *testing.T, g *GitHub, seq int) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", g.URL+"/installation/token", nil)
	req.Header.Set("Authorization", "Bearer "+g.Token(seq))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status %d", resp.StatusCode)
	}
}

func TestMintScopeAssertions(t *testing.T) {
	g, _ := newGH(t)
	mint(t, g, []int64{repoID}, PushPerms)
	if v := g.Violations(repoID, PushPerms); len(v) != 0 {
		t.Fatalf("exact scope flagged: %v", v)
	}
	mint(t, g, []int64{repoID, 43}, PushPerms) // two repositories
	mint(t, g, []int64{repoID}, Perms{"contents": "write", "administration": "write"})
	if v := g.Violations(repoID, PushPerms); len(v) != 2 {
		t.Fatalf("want 2 violations (extra repo, extra permission), got %v", v)
	}
}

func TestRevocationIsTracked(t *testing.T) {
	g, _ := newGH(t)
	a := mint(t, g, []int64{repoID}, PushPerms)
	b := mint(t, g, []int64{repoID}, ReadPerms)
	if got := g.Unrevoked(); len(got) != 2 {
		t.Fatalf("both live, got %v", got)
	}
	revoke(t, g, a)
	if got := g.Unrevoked(); len(got) != 1 || got[0] != b {
		t.Fatalf("want only mint %d live, got %v", b, got)
	}
	if code := api(t, g, a, "GET", "/repos/o/r/pulls/1"); code != http.StatusUnauthorized {
		t.Fatalf("revoked token answered %d, want 401", code)
	}
}

func TestTokenScopeIsEnforcedOnTheAPI(t *testing.T) {
	g, _ := newGH(t)
	read := mint(t, g, []int64{repoID}, ReadPerms)
	cases := []struct {
		name, method, path string
		want               int
	}{
		{"read on scoped repo", "GET", "/repos/o/r/pulls/1", 200},
		{"merge with read-only token", "PUT", "/repos/o/r/pulls/1/merge", 403},
		{"other repository", "GET", "/repos/o/other/pulls/1", 404},
	}
	for _, c := range cases {
		if got := api(t, g, read, c.method, c.path); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	push := mint(t, g, []int64{repoID}, PRPushPerms)
	if got := api(t, g, push, "PATCH", "/repos/o/r/pulls/1"); got != 200 {
		t.Errorf("write token write: status %d, want 200", got)
	}
	if calls := g.Calls(); len(calls) != 4 || !strings.HasPrefix(calls[1], "PUT /repos/o/r/pulls/1/merge 403") {
		t.Errorf("call log %v", calls)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("git %v: %s", args[0], out)
	}
	if err != nil {
		return "ERR"
	}
	return strings.TrimSpace(string(out))
}

func TestGitRemoteChecksTheToken(t *testing.T) {
	g, root := newGH(t)
	work := t.TempDir()
	gitIn(t, work, "init", "-b", "main")
	os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644)
	gitIn(t, work, "add", ".")
	gitIn(t, work, "commit", "-m", "c")
	url := func(seq int) string {
		return strings.Replace(g.GitURL("o/r"), "http://", "http://x-access-token:"+g.Token(seq)+"@", 1)
	}
	read := mint(t, g, []int64{repoID}, ReadPerms)
	if gitIn(t, work, "push", url(read), "main") != "ERR" {
		t.Fatal("push with read-only token must fail")
	}
	push := mint(t, g, []int64{repoID}, PushPerms)
	if gitIn(t, work, "push", url(push), "main") == "ERR" {
		t.Fatalf("push with write token must work: %v", g.Calls())
	}
	if got := gitIn(t, filepath.Join(root, "o", "r.git"), "rev-parse", "main"); got != gitIn(t, work, "rev-parse", "main") {
		t.Fatalf("bare repo has %s", got)
	}
	revoke(t, g, push)
	if gitIn(t, work, "push", url(push), "main") != "ERR" {
		t.Fatal("push after revoke must fail")
	}
	other := mint(t, g, []int64{43}, PushPerms)
	if gitIn(t, work, "push", url(other), "main") != "ERR" {
		t.Fatal("push to a repository outside the token must fail")
	}
}

func TestProviderRecordsRequests(t *testing.T) {
	p := NewProvider(t)
	req, _ := http.NewRequest("POST", p.URL+"/v1/messages", strings.NewReader(`{"m":1}`))
	req.Header.Set("Authorization", "Bearer fake-cred")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("provider: %v %v", resp, err)
	}
	resp.Body.Close()
	got := p.Requests()
	if len(got) != 1 || got[0].Path != "/v1/messages" || got[0].Auth != "Bearer fake-cred" || got[0].Body != `{"m":1}` {
		t.Fatalf("requests %+v", got)
	}
}

func TestAgentPerformsActionsDirectlyAndReportsEachOutcome(t *testing.T) {
	p := NewProvider(t)
	dir := t.TempDir()
	steps := []Step{
		{Op: "exec", Data: "echo hi"},
		{Op: "write", Path: filepath.Join(dir, "a"), Data: "hostile text"},
		{Op: "read", Path: filepath.Join(dir, "a")},
		{Op: "read", Path: filepath.Join(dir, "missing")},
		{Op: "env", Name: "GATE_SET"},
		{Op: "env", Name: "GATE_UNSET"},
		{Op: "http", Method: "POST", URL: p.URL + "/v1/messages", Header: map[string]string{"Authorization": "Bearer c"}},
		{Op: "http", Method: "GET", URL: "http://127.0.0.1:1/"},
	}
	got := RunAgent(t, steps, "GATE_SET=secret-value")
	if len(got) != len(steps) {
		t.Fatalf("%d outcomes for %d steps: %+v", len(got), len(steps), got)
	}
	want := []struct {
		ok  bool
		out string
	}{{true, "hi"}, {true, ""}, {true, "hostile text"}, {false, ""}, {true, "set"}, {true, "unset"}, {true, ""}, {false, ""}}
	for i, w := range want {
		if got[i].OK != w.ok || (w.out != "" && got[i].Out != w.out) {
			t.Errorf("step %d (%s): %+v, want ok=%v out=%q", i, steps[i].Op, got[i], w.ok, w.out)
		}
	}
	if strings.Contains(got[4].Out, "secret-value") {
		t.Error("env step leaked the value")
	}
	if got[3].Err == "" || got[7].Err == "" {
		t.Error("failed steps must carry an error")
	}
	if r := p.Requests(); len(r) != 1 || r[0].Auth != "Bearer c" {
		t.Errorf("provider saw %+v", r)
	}
}

func TestPermissionMustMatchTheOperation(t *testing.T) {
	g, _ := newGH(t)
	work := t.TempDir()
	gitIn(t, work, "init", "-b", "main")
	os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644)
	gitIn(t, work, "add", ".")
	gitIn(t, work, "commit", "-m", "c")
	issuesW := mint(t, g, []int64{repoID}, Perms{"issues": "write"})
	prW := mint(t, g, []int64{repoID}, Perms{"pull_requests": "write"})
	contentsW := mint(t, g, []int64{repoID}, PushPerms)
	cases := []struct {
		name         string
		seq          int
		method, path string
		want         int
	}{
		{"issues:write cannot merge", issuesW, "PUT", "/repos/o/r/pulls/1/merge", 403},
		{"issues:write cannot edit a PR", issuesW, "PATCH", "/repos/o/r/pulls/1", 403},
		{"issues:write can comment", issuesW, "POST", "/repos/o/r/issues/1/comments", 200},
		{"pull_requests:write alone cannot merge", prW, "PUT", "/repos/o/r/pulls/1/merge", 403},
		{"pull_requests:write can edit a PR", prW, "PATCH", "/repos/o/r/pulls/1", 200},
		{"contents:write alone can merge", contentsW, "PUT", "/repos/o/r/pulls/1/merge", 200},
		{"contents:write cannot edit a PR", contentsW, "PATCH", "/repos/o/r/pulls/1", 403},
		{"contents:write cannot write to an unknown endpoint", contentsW, "POST", "/repos/o/r/hooks", 403},
	}
	for _, c := range cases {
		if got := api(t, g, c.seq, c.method, c.path); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	url := strings.Replace(g.GitURL("o/r"), "http://", "http://x-access-token:"+g.Token(issuesW)+"@", 1)
	if gitIn(t, work, "push", url, "main") != "ERR" {
		t.Error("issues:write must not push")
	}
}

func TestAgentExecRunsAShellScript(t *testing.T) {
	got := RunAgent(t, []Step{{Op: "exec", Data: "echo a | tr a b"}, {Op: "exec", Data: "echo out; exit 3"}})
	if len(got) != 2 || !got[0].OK || got[0].Out != "b" || got[1].OK || got[1].Out != "out" {
		t.Fatalf("outcomes %+v", got)
	}
}

func TestMintRefusesAnAppJWTSignedByAnotherKey(t *testing.T) {
	g, _ := newGH(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]*rsa.PrivateKey{"other key": other} {
		tok, _ := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{Issuer: g.AppID}).SignedString(key)
		for _, bearer := range []string{tok, "garbage"} {
			req, _ := http.NewRequest("POST", fmt.Sprintf("%s/app/installations/%d/access_tokens", g.URL, g.InstallationID), strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+bearer)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
			}
		}
	}
	if len(g.Mints()) != 0 {
		t.Error("a refused request must not mint")
	}
}
