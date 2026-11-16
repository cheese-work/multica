package main

import (
	"net/http"
	"os"

	"github.com/multica-ai/multica/server/internal/controlplane"
)

// newInternalServer builds the gateway-facing control-plane listener from
// MULTICA_INTERNAL_LISTEN_ADDR (unset = disabled). Gateway-facing route
// groups (events, introspection, JWKS, cleanup-ack, lease summary) are added
// to the mounts below by their own issues; they must never be added to the
// public router in router.go.
func newInternalServer(publicPort string) (*http.Server, error) {
	return controlplane.NewServer(os.Getenv("MULTICA_INTERNAL_LISTEN_ADDR"), publicPort)
}
