package passkey

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// FromEnv builds the relying party from MULTICA_WEBAUTHN_RP_ID and the exact
// origin allowlist MULTICA_WEBAUTHN_ORIGINS (comma separated, https only, no
// path). It returns nil, nil when either is unset: passkeys are then off and
// approval stays disabled.
func FromEnv(getenv func(string) string) (*webauthn.WebAuthn, error) {
	rpID, rawOrigins := strings.TrimSpace(getenv("MULTICA_WEBAUTHN_RP_ID")), strings.TrimSpace(getenv("MULTICA_WEBAUTHN_ORIGINS"))
	if rpID == "" || rawOrigins == "" {
		return nil, nil
	}
	var origins []string
	for _, origin := range strings.Split(rawOrigins, ",") {
		origin = strings.TrimSpace(origin)
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" || strings.Contains(parsed.Host, "*") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
			return nil, fmt.Errorf("MULTICA_WEBAUTHN_ORIGINS: %q is not an https origin", origin)
		}
		origins = append(origins, origin)
	}
	return webauthn.New(&webauthn.Config{
		RPDisplayName: "Multica", RPID: rpID, RPOrigins: origins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired},
	})
}
