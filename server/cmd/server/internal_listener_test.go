package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/controlplane"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
)

// Gateway-facing routes: event stream, introspection, JWKS, cleanup-ack, lease summary.
var gatewayFacingPaths = []string{
	controlplane.Prefix + "/jwks",
	controlplane.Prefix + "/instances/inst-1/events",
	controlplane.Prefix + "/introspect",
	controlplane.Prefix + "/instances/inst-1/cleanup-ack",
	controlplane.Prefix + "/lease-summary",
}

// CHE-4746 (W3-17): the public and tailnet routers are the same router; neither
// may serve the gateway-facing routes, for any method.
func TestPublicRouterReturns404ForGatewayRoutes(t *testing.T) {
	router := NewRouter(nil, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	for _, path := range gatewayFacingPaths {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, path, rec.Code)
			}
		}
	}
}

// Guard against a later PR mounting a gateway route on the public router.
func TestPublicRouterMountsNothingUnderInternalPrefix(t *testing.T) {
	router := NewRouter(nil, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, controlplane.Prefix) {
			t.Errorf("public router mounts %s %s", method, route)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInternalListenerAddrFromEnv(t *testing.T) {
	t.Setenv("MULTICA_INTERNAL_LISTEN_ADDR", "")
	if srv, err := newInternalServer("8080"); srv != nil || err != nil {
		t.Fatalf("unset env: got (%v, %v), want (nil, nil)", srv, err)
	}
	t.Setenv("MULTICA_INTERNAL_LISTEN_ADDR", "0.0.0.0:9000")
	if srv, err := newInternalServer("8080"); srv != nil || err == nil {
		t.Fatalf("wildcard env: got (%v, %v), want an error", srv, err)
	}
	t.Setenv("MULTICA_INTERNAL_LISTEN_ADDR", "172.30.0.2:9000")
	if srv, err := newInternalServer("8080"); srv == nil || err != nil {
		t.Fatalf("explicit env: got (%v, %v), want a server", srv, err)
	}
}

// CHE-4736 (W3-7): cleanup-ack and quarantine are served on the internal listener, behind the
// per-gateway credential authenticator, and nowhere else.
func ackListener(t *testing.T) *http.Server {
	t.Helper()
	p := browserlease.Principal{
		CredentialID: pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
		WorkspaceID:  pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		Gateway:      "c00",
	}
	auth := func(r *http.Request) (browserlease.Principal, bool) {
		if r.Header.Get("Authorization") != "Bearer mbc_good" {
			return browserlease.Principal{}, false
		}
		return p, true
	}
	t.Setenv("MULTICA_INTERNAL_LISTEN_ADDR", "172.30.0.2:9000")
	srv, err := newInternalServer("8080", browserlease.NewAckService(nil).Mount(auth))
	if err != nil || srv == nil {
		t.Fatalf("newInternalServer: (%v, %v)", srv, err)
	}
	return srv
}

func TestInternalListenerServesCleanupAckBehindTheGatewayCredential(t *testing.T) {
	srv := ackListener(t)
	const inst = "6f1d3b8e-1b6e-4f0e-9d0a-0c1c2b3a4d5e"
	for _, route := range []string{"cleanup-ack", "quarantine"} {
		path := controlplane.Prefix + "/instances/" + inst + "/" + route
		cases := []struct {
			name, auth string
			want       int
		}{
			{"no header", "", http.StatusUnauthorized},
			{"wrong scheme", "Basic mbc_good", http.StatusUnauthorized},
			{"unknown token", "Bearer mbc_bad", http.StatusUnauthorized},
			{"good credential reaches the handler", "Bearer mbc_good", http.StatusBadRequest}, // empty body: epoch missing
		}
		for _, c := range cases {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			rec := httptest.NewRecorder()
			srv.Handler.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("%s %s: status %d, want %d", route, c.name, rec.Code, c.want)
			}
		}
	}
}

func TestPublicRouterDoesNotServeCleanupAckOrQuarantine(t *testing.T) {
	router := NewRouter(nil, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	for _, route := range []string{"cleanup-ack", "quarantine"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, controlplane.Prefix+"/instances/inst-1/"+route, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("public POST %s = %d, want 404", route, rec.Code)
		}
	}
}
