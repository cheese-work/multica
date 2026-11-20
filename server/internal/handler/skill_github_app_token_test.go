package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// githubAppTokenFixture serves the GitHub App token endpoints and records what
// the server asked for.
type githubAppTokenFixture struct {
	mu       sync.Mutex
	mintBody string
	mints    int
	revoked  []string
}

func newGitHubAppTokenFixture(t *testing.T, instID string, mintStatus int) *githubAppTokenFixture {
	t.Helper()
	fx := &githubAppTokenFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/"+instID+"/access_tokens":
			b, _ := io.ReadAll(r.Body)
			fx.mintBody = string(b)
			fx.mints++
			if mintStatus != http.StatusCreated {
				http.Error(w, "nope", mintStatus)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_install_token"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			fx.revoked = append(fx.revoked, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })
	pemBytes, _ := generateTestRSAKeyPEM(t)
	t.Setenv("GITHUB_APP_ID", "8787")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pemBytes))
	return fx
}

func seedSkillInstallation(t *testing.T, instID int64, login string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO github_installation (workspace_id, installation_id, account_login, account_type) VALUES ($1, $2, $3, 'Organization')`, testWorkspaceID, instID, login)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_installation WHERE installation_id = $1`, instID)
	})
}

// authFor returns the Authorization header the skill fetch would send to the
// GitHub API and to the raw content host for ctx.
func authFor(t *testing.T, ctx context.Context) (api, raw string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/acme/x/git/trees/main", nil)
	if err != nil {
		t.Fatal(err)
	}
	addGitHubAuthHeader(req)
	rawReq, err := newRawFileRequest(ctx, "https://raw.githubusercontent.com/acme/x/main/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	return req.Header.Get("Authorization"), rawReq.Header.Get("Authorization")
}

func TestGitHubSkillAuthContext_TokenPrecedence(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const instID int64 = 87870001
	const url = "https://github.com/che8787-org/private-dotfiles/tree/main/skills/foo"

	cases := []struct {
		name       string
		seed       bool
		mintStatus int
		envToken   string
		wantAuth   string
		wantMints  int
	}{
		{name: "installation token wins over env", seed: true, mintStatus: http.StatusCreated, envToken: "ghp_env", wantAuth: "Bearer ghs_install_token", wantMints: 1},
		{name: "no installation falls back to env", seed: false, mintStatus: http.StatusCreated, envToken: "ghp_env", wantAuth: "Bearer ghp_env"},
		{name: "no installation and no env is unauthenticated", seed: false, mintStatus: http.StatusCreated, wantAuth: ""},
		{name: "mint failure falls back to env", seed: true, mintStatus: http.StatusInternalServerError, envToken: "ghp_env", wantAuth: "Bearer ghp_env", wantMints: 1},
		{name: "mint failure without env is unauthenticated", seed: true, mintStatus: http.StatusForbidden, wantAuth: "", wantMints: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGitHubAppTokenFixture(t, "87870001", tc.mintStatus)
			t.Setenv("GITHUB_TOKEN", tc.envToken)
			if tc.seed {
				seedSkillInstallation(t, instID, "Che8787-Org") // owner match is case-insensitive
			}

			ctx, release := testHandler.githubSkillAuthContext(context.Background(), parseUUID(testWorkspaceID), url)
			api, raw := authFor(t, ctx)
			release()

			if api != tc.wantAuth || raw != tc.wantAuth {
				t.Fatalf("Authorization api=%q raw=%q, want %q", api, raw, tc.wantAuth)
			}
			fx.mu.Lock()
			defer fx.mu.Unlock()
			if fx.mints != tc.wantMints {
				t.Fatalf("token mints = %d, want %d", fx.mints, tc.wantMints)
			}
			if tc.wantMints > 0 && !strings.Contains(fx.mintBody, `"contents":"read"`) {
				t.Fatalf("mint body %q must request contents:read", fx.mintBody)
			}
			wantRevoked := 0
			if tc.mintStatus == http.StatusCreated && tc.seed {
				wantRevoked = 1
			}
			if len(fx.revoked) != wantRevoked {
				t.Fatalf("revoked = %v, want %d revocation(s)", fx.revoked, wantRevoked)
			}
		})
	}
}

// The installation token must reach only GitHub hosts, even though
// fetchRawFile also serves clawhub.ai and skills.sh.
func TestGitHubSkillAuthContext_TokenNeverReachesNonGitHubHost(t *testing.T) {
	ctx := withGitHubSkillToken(context.Background(), "ghs_install_token")
	for _, u := range []string{"https://clawhub.ai/api/skills/foo/file", "https://skills.sh/acme/foo/SKILL.md"} {
		req, err := newRawFileRequest(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "" {
			t.Fatalf("%s got Authorization %q, want none", u, got)
		}
	}
}

// A mint failure must not leak the token or the App JWT into the returned
// context's logs/errors: the helper returns no error at all and the failing
// body is never surfaced.
func TestGitHubSkillAuthContext_NonGitHubSourceSkipsLookup(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	fx := newGitHubAppTokenFixture(t, "87870001", http.StatusCreated)
	_, release := testHandler.githubSkillAuthContext(context.Background(), parseUUID(testWorkspaceID), "https://clawhub.ai/foo")
	release()
	if fx.mints != 0 {
		t.Fatalf("mints = %d for a non-GitHub URL, want 0", fx.mints)
	}
}
