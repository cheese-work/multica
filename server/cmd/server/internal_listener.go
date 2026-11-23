package main

import (
	"log/slog"
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

// internalMounts are the gateway-facing route groups served on the internal listener. The JWKS
// needs the signer only. Events, cleanup-ack and quarantine sit behind a per-gateway credential
// (CHE-8883); a plugin install token authenticates nothing here, and with no credential issued
// every request to those routes is 401.
func internalMounts(h *handler.Handler) []func(chi.Router) {
	if h == nil || h.BrowserLeases == nil {
		return nil
	}
	var mounts []func(chi.Router)
	if src, ok := h.BrowserLeases.Signer.(jwksSource); ok && src != nil {
		jwks := src.JWKSHandler()
		mounts = append(mounts, func(r chi.Router) { r.Handle("/jwks", jwks) })
	}
	if h.Queries != nil {
		creds := browserlease.CredentialAuth{Queries: h.Queries}
		acks := browserlease.NewAckService(h.BrowserLeases.Pool)
		acks.Queue = h.BrowserLeases
		events := &browserlease.Gateway{
			Store:        browserlease.DBStore{Queries: h.Queries},
			Broker:       &browserlease.Broker{},
			Authenticate: creds.Authenticate,
			Owns:         creds.Owns,
		}
		mounts = append(mounts,
			acks.Mount(creds.Authenticate),
			func(r chi.Router) {
				r.Method(http.MethodGet, strings.TrimPrefix(browserlease.EventsPath, "/internal/browser"), events.EventsHandler())
			},
		)
	}
	return mounts
}
