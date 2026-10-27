package passkey

import (
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// waUser adapts a Multica user and their usable passkeys to the library.
type waUser struct {
	id          pgtype.UUID
	credentials []webauthn.Credential
}

func newWebAuthnUser(id pgtype.UUID, rows []db.UserPasskey) waUser {
	user := waUser{id: id}
	for _, row := range rows {
		transports := make([]protocol.AuthenticatorTransport, len(row.Transports))
		for i, transport := range row.Transports {
			transports[i] = protocol.AuthenticatorTransport(transport)
		}
		user.credentials = append(user.credentials, webauthn.Credential{
			ID: row.CredentialID, PublicKey: row.PublicKey, AttestationType: row.AttestationType, Transport: transports,
			Flags:         webauthn.NewCredentialFlags(protocol.AuthenticatorFlags(row.Flags)),
			Authenticator: webauthn.Authenticator{AAGUID: row.Aaguid, SignCount: uint32(row.SignCount)},
		})
	}
	return user
}

func (user waUser) WebAuthnID() []byte                         { return user.id.Bytes[:] }
func (user waUser) WebAuthnName() string                       { return "multica" }
func (user waUser) WebAuthnDisplayName() string                { return "Multica" }
func (user waUser) WebAuthnCredentials() []webauthn.Credential { return user.credentials }
