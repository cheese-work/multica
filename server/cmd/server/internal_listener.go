package main

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

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

// installTokenAuthenticator resolves "Authorization: Bearer mpi_..." to the workspace of the
// install it belongs to. lookup reports an unknown, disabled or malformed token as an error.
func installTokenAuthenticator(lookup func(ctx context.Context, token string) (pgtype.UUID, error)) browserlease.Authenticator {
	return func(r *http.Request) (pgtype.UUID, bool) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			return pgtype.UUID{}, false
		}
		ws, err := lookup(r.Context(), token)
		return ws, err == nil && ws.Valid
	}
}

// internalMounts are the gateway-facing route groups served on the internal listener. They stay
// off while the plugin service is absent: with no way to authenticate, nothing is served.
func internalMounts(h *handler.Handler) []func(chi.Router) {
	if h == nil || h.PluginService == nil || h.BrowserLeases == nil {
		return nil
	}
	auth := installTokenAuthenticator(func(ctx context.Context, token string) (pgtype.UUID, error) {
		inst, err := h.PluginService.AuthenticateInstallToken(ctx, token)
		return inst.WorkspaceID, err
	})
	acks := browserlease.NewAckService(h.BrowserLeases.Pool)
	acks.Queue = h.BrowserLeases
	return []func(chi.Router){acks.Mount(auth)}
}
