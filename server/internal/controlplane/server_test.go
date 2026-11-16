package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestNewServerDisabledWhenAddrEmpty(t *testing.T) {
	srv, err := NewServer("", "8080")
	if err != nil || srv != nil {
		t.Fatalf("empty addr: got (%v, %v), want (nil, nil)", srv, err)
	}
}

func TestNewServerRejectsUnsafeAddr(t *testing.T) {
	for _, addr := range []string{
		":9000", "0.0.0.0:9000", "[::]:9000", "[::0]:9000", // wildcard binds reach every network
		"172.30.0.2", "172.30.0.2:0", "172.30.0.2:70000", "172.30.0.2:abc", // bad port
		"172.30.0.2:8080", // same port as the public listener
		"not a host:9000",
	} {
		if srv, err := NewServer(addr, "8080"); err == nil || srv != nil {
			t.Errorf("addr %q: got (%v, %v), want an error", addr, srv, err)
		}
	}
}

func TestNewServerAcceptsExplicitHost(t *testing.T) {
	srv, err := NewServer("172.30.0.2:9000", "8080")
	if err != nil || srv == nil {
		t.Fatalf("got (%v, %v), want a server", srv, err)
	}
	if srv.Addr != "172.30.0.2:9000" {
		t.Errorf("Addr = %q", srv.Addr)
	}
	if srv.WriteTimeout != 0 || srv.ReadTimeout != 0 {
		t.Error("Read/WriteTimeout must stay zero: the event stream is long-lived")
	}
	if srv.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout must be set (Slowloris)")
	}
}

func TestHandlerServesOnlyMountedRoutesUnderPrefix(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv, err := NewServer("172.30.0.2:9000", "8080", func(r chi.Router) {
		r.Get("/jwks", ok)
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, Prefix + "/jwks", http.StatusNoContent},
		{http.MethodGet, Prefix + "/missing", http.StatusNotFound},
		{http.MethodGet, "/jwks", http.StatusNotFound},       // outside the prefix
		{http.MethodGet, "/api/config", http.StatusNotFound}, // public API is not served here
		{http.MethodGet, "/health", http.StatusNotFound},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
}
