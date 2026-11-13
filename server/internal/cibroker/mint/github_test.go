package mint

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type fakeGitHub struct {
	*httptest.Server
	mu        sync.Mutex
	key       *rsa.PrivateKey
	appJWTs   []string
	mintBody  map[string]any
	mintCalls int
	revokes   []string
	// overrides
	mintStatus   int
	mintResponse func(body map[string]any) map[string]any
	rate         string
	rateStatus   int
	revokeStatus int
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *GitHubMinter) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGitHub{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/777/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.mintCalls++
		g.appJWTs = append(g.appJWTs, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		g.mintBody = body
		if g.mintStatus != 0 {
			w.WriteHeader(g.mintStatus)
			_, _ = io.WriteString(w, `{"message":"nope ghs_LEAKYBODY"}`)
			return
		}
		resp := map[string]any{
			"token":                fmt.Sprintf("ghs_minted%d", g.mintCalls),
			"expires_at":           "2026-11-12T11:00:00Z",
			"permissions":          body["permissions"],
			"repository_selection": "selected",
			"repositories":         []map[string]any{{"id": 42, "full_name": "cheese-work/multica"}},
		}
		if g.mintResponse != nil {
			resp = g.mintResponse(body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("DELETE /installation/token", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.revokes = append(g.revokes, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		status := g.revokeStatus
		g.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /rate_limit", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.rateStatus != 0 {
			w.WriteHeader(g.rateStatus)
			return
		}
		_, _ = io.WriteString(w, g.rate)
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	der := x509.MarshalPKCS1PrivateKey(key)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	m, err := NewGitHubMinter(GitHubConfig{
		BaseURL: g.URL, AppID: "123456", InstallationID: 777, PrivateKeyPEM: pemKey,
		Now: func() time.Time { return t0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	return g, m
}

func TestGitHubMintScopesToCallerRepoAndPermissions(t *testing.T) {
	g, m := newFakeGitHub(t)
	tok, err := m.Mint(context.Background(), 42, Permissions{"contents": "read", "actions": "read"})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value.Reveal() != "ghs_minted1" || !tok.ExpiresAt.Equal(time.Date(2026, 11, 12, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("token = %+v", tok)
	}
	ids, _ := g.mintBody["repository_ids"].([]any)
	if len(ids) != 1 || ids[0].(float64) != 42 {
		t.Fatalf("repository_ids = %v, want [42]", g.mintBody["repository_ids"])
	}
	perms, _ := g.mintBody["permissions"].(map[string]any)
	if len(perms) != 2 || perms["contents"] != "read" || perms["actions"] != "read" {
		t.Fatalf("permissions = %v", g.mintBody["permissions"])
	}
	// The mint call is authenticated with an RS256 App JWT for this App.
	parsed, err := jwt.Parse(g.appJWTs[0], func(*jwt.Token) (any, error) { return &g.key.PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithTimeFunc(func() time.Time { return t0 }))
	if err != nil {
		t.Fatalf("app jwt: %v", err)
	}
	if iss, _ := parsed.Claims.GetIssuer(); iss != "123456" {
		t.Fatalf("iss = %q", iss)
	}
}

func TestGitHubMintFailsClosedOnBadResponses(t *testing.T) {
	cases := []struct {
		name string
		set  func(g *fakeGitHub)
	}{
		{"github 500", func(g *fakeGitHub) { g.mintStatus = 500 }},
		{"github 403", func(g *fakeGitHub) { g.mintStatus = 403 }},
		{"empty token", func(g *fakeGitHub) {
			g.mintResponse = func(b map[string]any) map[string]any {
				return map[string]any{"token": "", "expires_at": "2026-11-12T11:00:00Z", "permissions": b["permissions"],
					"repository_selection": "selected", "repositories": []map[string]any{{"id": 42}}}
			}
		}},
		{"all repositories", func(g *fakeGitHub) {
			g.mintResponse = func(b map[string]any) map[string]any {
				return map[string]any{"token": "ghs_x", "expires_at": "2026-11-12T11:00:00Z", "permissions": b["permissions"],
					"repository_selection": "all"}
			}
		}},
		{"other repository", func(g *fakeGitHub) {
			g.mintResponse = func(b map[string]any) map[string]any {
				return map[string]any{"token": "ghs_x", "expires_at": "2026-11-12T11:00:00Z", "permissions": b["permissions"],
					"repository_selection": "selected", "repositories": []map[string]any{{"id": 43}}}
			}
		}},
		{"two repositories", func(g *fakeGitHub) {
			g.mintResponse = func(b map[string]any) map[string]any {
				return map[string]any{"token": "ghs_x", "expires_at": "2026-11-12T11:00:00Z", "permissions": b["permissions"],
					"repository_selection": "selected", "repositories": []map[string]any{{"id": 42}, {"id": 43}}}
			}
		}},
		{"broader permission granted", func(g *fakeGitHub) {
			g.mintResponse = func(b map[string]any) map[string]any {
				return map[string]any{"token": "ghs_x", "expires_at": "2026-11-12T11:00:00Z",
					"permissions":          map[string]any{"contents": "write", "actions": "read"},
					"repository_selection": "selected", "repositories": []map[string]any{{"id": 42}}}
			}
		}},
		{"extra permission granted", func(g *fakeGitHub) {
			g.mintResponse = func(b map[string]any) map[string]any {
				return map[string]any{"token": "ghs_x", "expires_at": "2026-11-12T11:00:00Z",
					"permissions":          map[string]any{"contents": "read", "actions": "read", "issues": "read"},
					"repository_selection": "selected", "repositories": []map[string]any{{"id": 42}}}
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, m := newFakeGitHub(t)
			c.set(g)
			_, err := m.Mint(context.Background(), 42, Permissions{"contents": "read", "actions": "read"})
			if err == nil {
				t.Fatal("want error")
			}
			if strings.Contains(err.Error(), "ghs_") {
				t.Fatalf("error leaks a token or response body: %v", err)
			}
		})
	}
}

func TestGitHubMintRevokesATokenItRefuses(t *testing.T) {
	g, m := newFakeGitHub(t)
	g.mintResponse = func(b map[string]any) map[string]any {
		return map[string]any{"token": "ghs_toobroad", "expires_at": "2026-11-12T11:00:00Z",
			"permissions":          map[string]any{"contents": "write"},
			"repository_selection": "selected", "repositories": []map[string]any{{"id": 42}}}
	}
	if _, err := m.Mint(context.Background(), 42, Permissions{"contents": "read"}); err == nil {
		t.Fatal("want error")
	}
	if len(g.revokes) != 1 || g.revokes[0] != "ghs_toobroad" {
		t.Fatalf("revokes = %v, want the refused token revoked", g.revokes)
	}
}

func TestGitHubMintAcceptsMetadataReadAddedByGitHub(t *testing.T) {
	g, m := newFakeGitHub(t)
	g.mintResponse = func(b map[string]any) map[string]any {
		return map[string]any{"token": "ghs_withmeta", "expires_at": "2026-11-12T11:00:00Z",
			"permissions":          map[string]any{"contents": "read", "metadata": "read"},
			"repository_selection": "selected", "repositories": []map[string]any{{"id": 42}}}
	}
	if _, err := m.Mint(context.Background(), 42, Permissions{"contents": "read"}); err != nil {
		t.Fatalf("metadata:read must be accepted: %v", err)
	}
	if len(g.revokes) != 0 {
		t.Fatalf("revokes = %v, want none", g.revokes)
	}
	g.mintResponse = func(b map[string]any) map[string]any {
		return map[string]any{"token": "ghs_metawrite", "expires_at": "2026-11-12T11:00:00Z",
			"permissions":          map[string]any{"contents": "read", "metadata": "write"},
			"repository_selection": "selected", "repositories": []map[string]any{{"id": 42}}}
	}
	if _, err := m.Mint(context.Background(), 42, Permissions{"contents": "read"}); err == nil {
		t.Fatal("metadata:write must be refused")
	}
}

func TestGitHubRefusedTokenWithFailedRevokeIsFlagged(t *testing.T) {
	g, m := newFakeGitHub(t)
	g.revokeStatus = 500
	g.mintResponse = func(b map[string]any) map[string]any {
		return map[string]any{"token": "ghs_toobroad", "expires_at": "2026-11-12T11:00:00Z",
			"permissions":          map[string]any{"contents": "write"},
			"repository_selection": "selected", "repositories": []map[string]any{{"id": 42}}}
	}
	_, err := m.Mint(context.Background(), 42, Permissions{"contents": "read"})
	if err == nil || !strings.Contains(err.Error(), "revoke failed") || strings.Contains(err.Error(), "ghs_") {
		t.Fatalf("err = %v, want a flagged failed revoke with no token", err)
	}
}

func TestGitHubRevoke(t *testing.T) {
	g, m := newFakeGitHub(t)
	if err := m.Revoke(context.Background(), "ghs_abc"); err != nil {
		t.Fatal(err)
	}
	if len(g.revokes) != 1 || g.revokes[0] != "ghs_abc" {
		t.Fatalf("revokes = %v", g.revokes)
	}
}

func TestGitHubRateLimitReadsCoreAndGraphQLWithAProbeToken(t *testing.T) {
	g, m := newFakeGitHub(t)
	g.rate = `{"resources":{"core":{"limit":5000,"remaining":4321,"reset":1794481200},"graphql":{"limit":5000,"remaining":321,"reset":1794481300}},"rate":{"limit":5000,"remaining":4321,"reset":1794481200}}`
	got, err := m.RateLimit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Core.Remaining != 4321 || got.GraphQL.Remaining != 321 ||
		got.Core.Reset.Unix() != 1794481200 || got.GraphQL.Reset.Unix() != 1794481300 {
		t.Fatalf("reading = %+v", got)
	}
	if perms, _ := g.mintBody["permissions"].(map[string]any); len(perms) != 1 || perms["metadata"] != "read" {
		t.Fatalf("probe permissions = %v, want metadata:read only", g.mintBody["permissions"])
	}
	if len(g.revokes) != 1 || g.revokes[0] != "ghs_minted1" {
		t.Fatalf("probe token not revoked: %v", g.revokes)
	}
}

func TestGitHubRateLimitFailsClosed(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(g *fakeGitHub)
	}{
		{"http 500", func(g *fakeGitHub) { g.rateStatus = 500 }},
		{"not json", func(g *fakeGitHub) { g.rate = "<html>" }},
		{"missing graphql", func(g *fakeGitHub) { g.rate = `{"resources":{"core":{"remaining":5000,"reset":1794481200}}}` }},
		{"missing core", func(g *fakeGitHub) { g.rate = `{"resources":{"graphql":{"remaining":5000,"reset":1794481200}}}` }},
		{"probe mint fails", func(g *fakeGitHub) { g.mintStatus = 500 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, m := newFakeGitHub(t)
			g.rate = `{"resources":{"core":{"remaining":5000,"reset":1794481200},"graphql":{"remaining":5000,"reset":1794481200}}}`
			c.set(g)
			if _, err := m.RateLimit(context.Background()); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestNewGitHubMinterRejectsBadConfig(t *testing.T) {
	if _, err := NewGitHubMinter(GitHubConfig{AppID: "1", InstallationID: 1, PrivateKeyPEM: []byte("junk")}); err == nil {
		t.Fatal("want error for a bad key")
	}
	if _, err := NewGitHubMinter(GitHubConfig{PrivateKeyPEM: nil}); err == nil {
		t.Fatal("want error for missing config")
	}
}
