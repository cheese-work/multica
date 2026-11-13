package mint

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	defaultBaseURL = "https://api.github.com"
	httpTimeout    = 10 * time.Second
	maxBody        = 1 << 20
)

// UpstreamError is a GitHub call that failed. It carries the operation and the
// HTTP status only: never a response body, a header or a token.
type UpstreamError struct {
	Op     string
	Status int
}

func (e *UpstreamError) Error() string {
	if e.Status == 0 {
		return "github " + e.Op + " failed"
	}
	return fmt.Sprintf("github %s failed: status %d", e.Op, e.Status)
}

// GitHubConfig configures the CI App client. The installation comes from server
// config, never from a request.
type GitHubConfig struct {
	BaseURL        string // default https://api.github.com
	AppID          string // App ID or client ID, the App JWT issuer
	InstallationID int64
	PrivateKeyPEM  []byte
	HTTPClient     *http.Client
	Now            func() time.Time
}

// GitHubMinter mints installation tokens from the CI App.
type GitHubMinter struct {
	base   string
	appID  string
	instID int64
	key    *rsa.PrivateKey
	hc     *http.Client
	now    func() time.Time
}

// NewGitHubMinter validates config and parses the private key.
func NewGitHubMinter(c GitHubConfig) (*GitHubMinter, error) {
	if c.AppID == "" || c.InstallationID <= 0 {
		return nil, errors.New("ci app id and installation id are required")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(c.PrivateKeyPEM)
	if err != nil {
		return nil, errors.New("ci app private key is not a valid RSA PEM key")
	}
	m := &GitHubMinter{base: strings.TrimRight(c.BaseURL, "/"), appID: c.AppID, instID: c.InstallationID, key: key, hc: c.HTTPClient, now: c.Now}
	if m.base == "" {
		m.base = defaultBaseURL
	}
	if m.hc == nil {
		m.hc = &http.Client{Timeout: httpTimeout}
	}
	if m.now == nil {
		m.now = time.Now
	}
	return m, nil
}

func (m *GitHubMinter) appJWT() (string, error) {
	now := m.now()
	return jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    m.appID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).SignedString(m.key)
}

func (m *GitHubMinter) do(ctx context.Context, op, method, path, bearer string, body any, out any, want int) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return &UpstreamError{Op: op}
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, rd)
	if err != nil {
		return &UpstreamError{Op: op}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := m.hc.Do(req)
	if err != nil {
		return &UpstreamError{Op: op}
	}
	defer res.Body.Close()
	if res.StatusCode != want {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxBody))
		return &UpstreamError{Op: op, Status: res.StatusCode}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxBody))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(out); err != nil {
		return &UpstreamError{Op: op, Status: res.StatusCode}
	}
	return nil
}

type accessTokenResponse struct {
	Token               string            `json:"token"`
	ExpiresAt           time.Time         `json:"expires_at"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
	Repositories        []struct {
		ID int64 `json:"id"`
	} `json:"repositories"`
}

// Mint creates a token for exactly one repository with exactly perms. It
// refuses, and revokes, a token whose scope is not what was asked: any other
// repository selection, or any permission beyond the request.
func (m *GitHubMinter) Mint(ctx context.Context, repoID int64, perms Permissions) (Token, error) {
	return m.mint(ctx, "mint", []int64{repoID}, perms)
}

func (m *GitHubMinter) mint(ctx context.Context, op string, repoIDs []int64, perms Permissions) (Token, error) {
	jwtStr, err := m.appJWT()
	if err != nil {
		return Token{}, &UpstreamError{Op: op}
	}
	var res accessTokenResponse
	body := map[string]any{"permissions": perms}
	if repoIDs != nil {
		body["repository_ids"] = repoIDs
	}
	if err := m.do(ctx, op, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", m.instID), jwtStr, body, &res, http.StatusCreated); err != nil {
		return Token{}, err
	}
	tok := Token{Value: Secret(res.Token), ExpiresAt: res.ExpiresAt, Permissions: res.Permissions}
	if res.Token == "" || !scopeMatches(res, repoIDs, perms) {
		opName := op + " scope"
		if res.Token != "" {
			if err := m.revokeBounded(ctx, tok.Value); err != nil {
				opName += "; revoke failed, token may be live"
			}
		}
		return Token{}, &UpstreamError{Op: opName, Status: http.StatusCreated}
	}
	return tok, nil
}

func scopeMatches(res accessTokenResponse, repoIDs []int64, perms Permissions) bool {
	// A nil repoIDs is the rate-limit probe: metadata:read only, no repository content.
	if repoIDs != nil {
		if res.RepositorySelection != "selected" || len(res.Repositories) != len(repoIDs) {
			return false
		}
		for i, id := range repoIDs {
			if res.Repositories[i].ID != id {
				return false
			}
		}
	}
	for name, got := range res.Permissions {
		// GitHub may add metadata:read to a repository-scoped token. It grants
		// no repository content, so it is the one accepted extra.
		if _, asked := perms[name]; !asked && name == "metadata" && got == "read" {
			continue
		}
		if permRank(got) == 0 || permRank(got) > permRank(perms[name]) {
			return false
		}
	}
	return true
}

// revokeBounded revokes after the caller's context is done, with its own
// deadline so a stuck call cannot hold the broker lock.
func (m *GitHubMinter) revokeBounded(ctx context.Context, token Secret) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sideEffectTimeout)
	defer cancel()
	return m.Revoke(ctx, token)
}

// Revoke deletes an installation token (DELETE /installation/token).
func (m *GitHubMinter) Revoke(ctx context.Context, token Secret) error {
	return m.do(ctx, "revoke", http.MethodDelete, "/installation/token", token.Reveal(), nil, nil, http.StatusNoContent)
}

type rateLimitResponse struct {
	Resources struct {
		Core    *rateResource `json:"core"`
		GraphQL *rateResource `json:"graphql"`
	} `json:"resources"`
}

type rateResource struct {
	Remaining *int  `json:"remaining"`
	Reset     int64 `json:"reset"`
}

// RateLimit reads GET /rate_limit for the CI App installation. That call needs
// an installation token, so it mints a metadata:read probe token, reads, and
// revokes it. The quota caches the reading, so this runs at most about twice a
// minute.
func (m *GitHubMinter) RateLimit(ctx context.Context) (Reading, error) {
	// No repository_ids: the probe token reads no repository and only measures the installation.
	probe, err := m.mint(ctx, "rate_limit probe", nil, Permissions{"metadata": "read"})
	if err != nil {
		return Reading{}, err
	}
	defer func() { _ = m.revokeBounded(ctx, probe.Value) }()
	var rl rateLimitResponse
	if err := m.do(ctx, "rate_limit", http.MethodGet, "/rate_limit", probe.Value.Reveal(), nil, &rl, http.StatusOK); err != nil {
		return Reading{}, err
	}
	c, g := rl.Resources.Core, rl.Resources.GraphQL
	if c == nil || g == nil || c.Remaining == nil || g.Remaining == nil {
		return Reading{}, &UpstreamError{Op: "rate_limit", Status: http.StatusOK}
	}
	return Reading{
		Core:    Resource{Remaining: *c.Remaining, Reset: time.Unix(c.Reset, 0).UTC()},
		GraphQL: Resource{Remaining: *g.Remaining, Reset: time.Unix(g.Reset, 0).UTC()},
	}, nil
}
