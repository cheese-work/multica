package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/analytics"
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
