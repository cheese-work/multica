package main

import (
	"context"
	"log/slog"
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

// jwksSource is the signer side of the JWKS route: the keyring serves its own public keys.
type jwksSource interface{ JWKSHandler() http.Handler }

// loadBrowserSigner builds the lease signer from MULTICA_BROWSER_SIGNING_KEYS and
// MULTICA_BROWSER_GATEWAY_AUDIENCES. Unset or malformed keys, or a malformed allowlist, give no
// signer (the error is logged without the secret). An unset allowlist gives a signer that signs
// nothing but still serves its JWKS.
func loadBrowserSigner(getenv func(string) string) browserlease.Signer {
	if strings.TrimSpace(getenv(browserlease.SigningKeysEnv)) == "" {
		return nil
	}
	kr, err := browserlease.KeyringFromEnv(getenv)
	if err != nil {
		slog.Error("browser lease: signer not loaded; leases stay unsigned", "error", err)
		return nil
	}
	return kr
}

// internalMounts are the gateway-facing route groups served on the internal listener. Each is
// mounted on its own dependency: the JWKS needs the signer only, so it is served with no plugin
// service; the cleanup-ack routes need the plugin service to authenticate and stay off without it.
func internalMounts(h *handler.Handler) []func(chi.Router) {
	if h == nil || h.BrowserLeases == nil {
		return nil
	}
	var mounts []func(chi.Router)
	if src, ok := h.BrowserLeases.Signer.(jwksSource); ok && src != nil {
		jwks := src.JWKSHandler()
		mounts = append(mounts, func(r chi.Router) { r.Handle("/jwks", jwks) })
	}
	if h.PluginService != nil {
		auth := installTokenAuthenticator(func(ctx context.Context, token string) (pgtype.UUID, error) {
			inst, err := h.PluginService.AuthenticateInstallToken(ctx, token)
			return inst.WorkspaceID, err
		})
		acks := browserlease.NewAckService(h.BrowserLeases.Pool)
		acks.Queue = h.BrowserLeases
		mounts = append(mounts, acks.Mount(auth))
	}
	return mounts
}
