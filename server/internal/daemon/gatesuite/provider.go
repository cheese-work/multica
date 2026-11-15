package gatesuite

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

type ProviderRequest struct{ Method, Path, Auth, Body string }

// Provider is a fake model-provider endpoint: it records every request and
// answers a fixed message, so no test depends on what a model decides.
type Provider struct {
	URL  string
	mu   sync.Mutex
	reqs []ProviderRequest
}

func NewProvider(t testing.TB) *Provider {
	p := &Provider{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.reqs = append(p.reqs, ProviderRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(body)})
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"fake reply"}]}`)
	}))
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

func (p *Provider) Requests() []ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.reqs)
}
