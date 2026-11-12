package cibroker

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrUnknownKey means the JWKS has no key with that id (a 401). Any other
// KeySource error means the keys are unavailable (a 503).
var ErrUnknownKey = errors.New("unknown signing key")

// KeySource returns the RSA public key for a JWT key id.
type KeySource interface {
	Key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// Verifier checks signature and claims of a GitHub OIDC JWT, and rejects a
// replayed jti until exp plus the clock skew.
type Verifier struct {
	Keys KeySource
	Now  func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // jti -> exp
}

func NewVerifier(keys KeySource) *Verifier {
	return &Verifier{Keys: keys, Now: time.Now, seen: map[string]time.Time{}}
}

func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	mc := jwt.MapClaims{}
	_, err := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(Issuer),
		jwt.WithAudience(Audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(skew),
		jwt.WithTimeFunc(v.Now),
	).ParseWithClaims(token, mc, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, refuse(401, "missing kid")
		}
		return v.Keys.Key(ctx, kid)
	})
	if err != nil {
		if errors.Is(err, ErrUnknownKey) {
			return nil, refuse(401, "unknown signing key")
		}
		var keyErr *keyUnavailable
		if errors.As(err, &keyErr) {
			return nil, refuse(503, "signing keys unavailable")
		}
		return nil, refuse(401, "invalid token")
	}
	c, err := claimsFrom(mc)
	if err != nil {
		return nil, err
	}
	now := v.Now()
	if now.Sub(c.IssuedAt) > maxIAT {
		return nil, refuse(401, "iat too old")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	// A token is accepted until exp+skew, so remember its jti that long.
	for jti, exp := range v.seen {
		if !exp.Add(skew).After(now) {
			delete(v.seen, jti)
		}
	}
	if _, dup := v.seen[c.JTI]; dup {
		return nil, refuse(401, "jti replayed")
	}
	v.seen[c.JTI] = c.Expires
	return c, nil
}

type keyUnavailable struct{ err error }

func (k *keyUnavailable) Error() string { return k.err.Error() }
func (k *keyUnavailable) Unwrap() error { return k.err }

// claimsFrom extracts the required claims. Missing or non-string is a 401.
func claimsFrom(mc jwt.MapClaims) (*Claims, error) {
	exp, err := mc.GetExpirationTime()
	if err != nil || exp == nil {
		return nil, refuse(401, "missing exp")
	}
	iat, err := mc.GetIssuedAt()
	if err != nil || iat == nil {
		return nil, refuse(401, "missing iat")
	}
	c := &Claims{Expires: exp.Time, IssuedAt: iat.Time}
	var bad string
	req := func(name string, dst *string) {
		s, ok := mc[name].(string)
		if !ok || s == "" {
			bad = name
			return
		}
		*dst = s
	}
	opt := func(name string, dst *string) {
		if v, present := mc[name]; present {
			s, ok := v.(string)
			if !ok {
				bad = name
				return
			}
			*dst = s
		}
	}
	req("jti", &c.JTI)
	req("repository", &c.Repository)
	req("repository_id", &c.RepositoryID)
	req("repository_owner_id", &c.RepositoryOwnerID)
	req("ref", &c.Ref)
	req("event_name", &c.EventName)
	req("workflow_ref", &c.WorkflowRef)
	req("runner_environment", &c.RunnerEnvironment)
	req("run_id", &c.RunID)
	req("run_attempt", &c.RunAttempt)
	opt("job_workflow_ref", &c.JobWorkflowRef)
	opt("environment", &c.Environment)
	if bad != "" {
		return nil, refuse(401, "missing or malformed claim %s", bad)
	}
	return c, nil
}

// JWKSKeys fetches signing keys from GitHub's JWKS endpoint and caches them. A
// cached key is trusted for MaxAge. A kid it has not seen, or an expired cache,
// triggers at most one fetch attempt per MinRefresh, failed attempts included.
type JWKSKeys struct {
	URL        string // default JWKSURL; tests only
	Client     *http.Client
	MinRefresh time.Duration // default 1 minute
	MaxAge     time.Duration // default 1 hour
	Now        func() time.Time

	fetchMu sync.Mutex // serialises fetches; never held while reading the cache
	mu      sync.Mutex // guards the fields below
	keys    map[string]*rsa.PublicKey
	fetched time.Time // last successful fetch
	tried   time.Time // last attempt, successful or not
	lastErr error     // error of the last attempt, nil if it succeeded
}

func (j *JWKSKeys) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	now, minRefresh, maxAge := time.Now, j.MinRefresh, j.MaxAge
	if j.Now != nil {
		now = j.Now
	}
	if minRefresh == 0 {
		minRefresh = time.Minute
	}
	if maxAge == 0 {
		maxAge = time.Hour
	}
	// cached answers from the cache, or says a fetch is due.
	cached := func() (key *rsa.PublicKey, err error, due bool) {
		j.mu.Lock()
		defer j.mu.Unlock()
		if k, ok := j.keys[kid]; ok && now().Sub(j.fetched) < maxAge {
			return k, nil, false
		}
		if !j.tried.IsZero() && now().Sub(j.tried) < minRefresh {
			if j.lastErr != nil {
				return nil, &keyUnavailable{j.lastErr}, false
			}
			return nil, ErrUnknownKey, false
		}
		return nil, nil, true
	}
	if k, err, due := cached(); !due {
		return k, err
	}
	j.fetchMu.Lock()
	defer j.fetchMu.Unlock()
	if k, err, due := cached(); !due { // another request fetched while we waited
		return k, err
	}
	keys, err := j.fetch(ctx)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.tried, j.lastErr = now(), err
	if err != nil {
		return nil, &keyUnavailable{err}
	}
	j.keys, j.fetched = keys, now()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, ErrUnknownKey
}

func (j *JWKSKeys) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	url, client := j.URL, j.Client
	if url == "" {
		url = JWKSURL
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty, Kid, N, E string
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return keys, nil
}
