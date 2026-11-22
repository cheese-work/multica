package main

import (
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/browserlease"
	"github.com/multica-ai/multica/server/internal/controlplane"
	"github.com/multica-ai/multica/server/internal/handler"
)

// newInternalServer builds the gateway-facing control-plane listener from
// MULTICA_INTERNAL_LISTEN_ADDR (unset = disabled). Gateway-facing route
// groups (events, introspection, JWKS, cleanup-ack, lease summary) are added
// to the mounts by their own issues; they must never be added to the
// public router in router.go.
func newInternalServer(publicPort string, mounts ...func(chi.Router)) (*http.Server, error) {
	return controlplane.NewServer(os.Getenv("MULTICA_INTERNAL_LISTEN_ADDR"), publicPort, mounts...)
}

// internalMounts are the gateway-facing route groups served on the internal listener: events,
// cleanup-ack and quarantine, all behind a per-gateway credential (CHE-8883). A plugin install
// token authenticates nothing here. With no credential issued, every request is 401.
func internalMounts(h *handler.Handler) []func(chi.Router) {
	if h == nil || h.BrowserLeases == nil || h.Queries == nil {
		return nil
	}
	creds := browserlease.CredentialAuth{Queries: h.Queries}
	acks := browserlease.NewAckService(h.BrowserLeases.Pool)
	acks.Queue = h.BrowserLeases
	events := &browserlease.Gateway{
		Store:        browserlease.DBStore{Queries: h.Queries},
		Broker:       &browserlease.Broker{},
		Authenticate: creds.Authenticate,
		Owns:         creds.Owns,
	}
	return []func(chi.Router){
		acks.Mount(creds.Authenticate),
		func(r chi.Router) {
			r.Method(http.MethodGet, strings.TrimPrefix(browserlease.EventsPath, "/internal/browser"), events.EventsHandler())
		},
	}
}
