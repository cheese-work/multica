// Package passkeytest is a software passkey for tests: it signs what a real
// authenticator would, so the verifier is tested end to end.
package passkeytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

const (
	FlagUserPresent  = 0x01
	FlagUserVerified = 0x04
)

// Authenticator holds one credential key.
type Authenticator struct {
	key          *ecdsa.PrivateKey
	CredentialID []byte
}

func New(test *testing.T) *Authenticator {
	test.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		test.Fatal(err)
	}
	id := make([]byte, 32)
	rand.Read(id)
	return &Authenticator{key: key, CredentialID: id}
}

func (a *Authenticator) COSEPublicKey(test *testing.T) []byte {
	test.Helper()
	point, err := a.key.PublicKey.Bytes() // 0x04 || X || Y, each coordinate 32 bytes for P-256
	if err != nil {
		test.Fatal(err)
	}
	x, y := point[1:33], point[33:65]
	data, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
		Curve:         int64(webauthncose.P256), XCoord: x, YCoord: y,
	})
	if err != nil {
		test.Fatal(err)
	}
	return data
}

// Signing is one assertion to sign.
type Signing struct {
	Challenge string // base64url, as the server sent it
	RPID      string
	Origin    string
	Flags     byte
	Counter   uint32
	UserID    []byte
}

func (a *Authenticator) Assert(test *testing.T, s Signing) json.RawMessage {
	test.Helper()
	encode := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(s.RPID))
	authData := append(rpHash[:], s.Flags, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(authData[33:], s.Counter)
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": s.Challenge, "origin": s.Origin})
	if err != nil {
		test.Fatal(err)
	}
	clientHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		test.Fatal(err)
	}
	credential, err := json.Marshal(map[string]any{
		"id": encode(a.CredentialID), "rawId": encode(a.CredentialID), "type": "public-key",
		"response": map[string]string{
			"clientDataJSON": encode(clientData), "authenticatorData": encode(authData),
			"signature": encode(signature), "userHandle": encode(s.UserID),
		},
	})
	if err != nil {
		test.Fatal(err)
	}
	return credential
}

const flagAttestedData = 0x40

// Registration is one "none"-attestation credential creation to sign.
type Registration struct {
	Challenge string // base64url, as the server sent it
	RPID      string
	Origin    string
	Flags     byte // FlagUserPresent|FlagUserVerified; the attested-data bit is added
}

// Register answers navigator.credentials.create for this credential.
func (a *Authenticator) Register(test *testing.T, r Registration) json.RawMessage {
	test.Helper()
	encode := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(r.RPID))
	authData := append(rpHash[:], r.Flags|flagAttestedData, 0, 0, 0, 0)
	authData = append(authData, make([]byte, 16)...) // AAGUID
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(a.CredentialID)))
	authData = append(authData, a.CredentialID...)
	authData = append(authData, a.COSEPublicKey(test)...)
	attestation, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		test.Fatal(err)
	}
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": r.Challenge, "origin": r.Origin})
	if err != nil {
		test.Fatal(err)
	}
	credential, err := json.Marshal(map[string]any{
		"id": encode(a.CredentialID), "rawId": encode(a.CredentialID), "type": "public-key",
		"response": map[string]string{"clientDataJSON": encode(clientData), "attestationObject": encode(attestation)},
	})
	if err != nil {
		test.Fatal(err)
	}
	return credential
}
