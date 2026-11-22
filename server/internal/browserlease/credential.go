package browserlease

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Per-gateway credentials (CHE-8883, A1b of plan CHE-8829 r16). Every gateway-facing control
// route (events, cleanup-ack, quarantine) authenticates with one of these and nothing else: a
// plugin install token never reaches them. A credential names one gateway of one workspace, and
// resolves only to that gateway's instances; any other instance is the same 404 as a missing one.
// The plaintext exists once, in the issue response; only its SHA-256 is stored.

// CredentialPrefix marks a gateway credential, so it cannot be confused with a plugin install
// token ("mpi_") or any other bearer.
const CredentialPrefix = "mbc_"

var (
	ErrUnknownGateway       = errors.New("browserlease: unknown gateway")
	ErrCredentialNotFound   = errors.New("browserlease: credential not found")
	ErrCredentialRevoked    = errors.New("browserlease: credential is revoked")
	errMalformedCredential  = errors.New("browserlease: malformed credential")
	credentialSecretByteLen = 32
)

// Principal is an authenticated gateway: the credential, its workspace and its gateway.
type Principal struct {
	CredentialID pgtype.UUID
	WorkspaceID  pgtype.UUID
	Gateway      string
}

// HashCredential is the stored form of a credential: lower-case hex SHA-256.
func HashCredential(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// bearerCredential extracts a well-formed gateway credential from an Authorization header value.
// Anything else (no scheme, another prefix such as a plugin install token, an empty secret) is
// not a credential.
func bearerCredential(header string) (string, error) {
	header = strings.TrimSpace(header)
	if len(header) <= len("Bearer ") || !strings.EqualFold(header[:len("Bearer ")], "Bearer ") {
		return "", errMalformedCredential
	}
	token := strings.TrimSpace(header[len("Bearer "):])
	if !strings.HasPrefix(token, CredentialPrefix) || len(token) == len(CredentialPrefix) {
		return "", errMalformedCredential
	}
	return token, nil
}

// CredentialAuth resolves a request's bearer credential to a Principal against the credential
// table. The lookup is by hash, so a credential that was never issued matches nothing.
type CredentialAuth struct{ Queries *db.Queries }

// Authenticate reports the calling gateway, or ok=false for a missing, malformed, unknown or
// revoked credential.
func (a CredentialAuth) Authenticate(r *http.Request) (Principal, bool) {
	token, err := bearerCredential(r.Header.Get("Authorization"))
	if err != nil {
		return Principal{}, false
	}
	hash := HashCredential(token)
	row, err := a.Queries.GetBrowserGatewayCredentialByHash(r.Context(), hash)
	if err != nil || subtle.ConstantTimeCompare([]byte(row.CredentialHash), []byte(hash)) != 1 {
		return Principal{}, false
	}
	return Principal{CredentialID: row.ID, WorkspaceID: row.WorkspaceID, Gateway: row.Gateway}, true
}

// CredentialService issues and revokes gateway credentials. The owner-only HTTP routes call it.
type CredentialService struct {
	Pool *pgxpool.Pool
	Now  func() time.Time
	// Audit, when set, writes the audit row through q, which is bound to the mutation's
	// transaction: a failed audit write rolls the issue or revoke back.
	Audit func(ctx context.Context, q *db.Queries, kind string, c db.BrowserGatewayCredential) error
}

// Audit kinds written by CredentialService.
const (
	AuditCredentialIssue  = "browser.gateway_credential.issue"
	AuditCredentialRevoke = "browser.gateway_credential.revoke"
)

func (s *CredentialService) audit(ctx context.Context, q *db.Queries, kind string, c db.BrowserGatewayCredential) error {
	if s.Audit == nil {
		return nil
	}
	return s.Audit(ctx, q, kind, c)
}

// inTx runs fn in one transaction.
func (s *CredentialService) inTx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// IssuedCredential is returned once, at issue. Plaintext is never stored and never logged.
type IssuedCredential struct {
	ID        pgtype.UUID
	Gateway   string
	Plaintext string
	CreatedAt time.Time
}

// ValidGateway reports whether gateway is a gateway of the legacy host matrix.
func ValidGateway(gateway string) bool { return gateway == HostC00 || gateway == HostX99 }

// Issue creates a credential for gateway in the workspace. The insert runs under the table's
// existing gateway CHECK (no host rows exist before R-a; plan r16 lock table, "Credential
// issuance"). An unknown gateway is refused before any write.
func (s *CredentialService) Issue(ctx context.Context, ws pgtype.UUID, gateway string) (IssuedCredential, error) {
	if !ValidGateway(gateway) {
		return IssuedCredential{}, ErrUnknownGateway
	}
	secret := make([]byte, credentialSecretByteLen)
	if _, err := rand.Read(secret); err != nil {
		return IssuedCredential{}, err
	}
	plaintext := CredentialPrefix + base64.RawURLEncoding.EncodeToString(secret)
	var row db.BrowserGatewayCredential
	err := s.inTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateBrowserGatewayCredential(ctx, db.CreateBrowserGatewayCredentialParams{
			WorkspaceID: ws, Gateway: gateway, CredentialHash: HashCredential(plaintext),
		})
		if err != nil {
			return err
		}
		return s.audit(ctx, q, AuditCredentialIssue, row)
	})
	if err != nil {
		return IssuedCredential{}, err
	}
	return IssuedCredential{ID: row.ID, Gateway: row.Gateway, Plaintext: plaintext, CreatedAt: row.CreatedAt.Time}, nil
}

// Revoke marks the credential revoked. It is idempotent: revoking a revoked credential succeeds.
// A credential of another workspace or gateway is ErrCredentialNotFound, the same as a missing
// one. A1d extends this transaction to end the gateway's active leases.
func (s *CredentialService) Revoke(ctx context.Context, ws pgtype.UUID, gateway string, id pgtype.UUID) error {
	if !ValidGateway(gateway) {
		return ErrUnknownGateway
	}
	q := db.New(s.Pool)
	revoked := false
	err := s.inTx(ctx, func(q *db.Queries) error {
		row, err := q.RevokeBrowserGatewayCredential(ctx, db.RevokeBrowserGatewayCredentialParams{ID: id, WorkspaceID: ws, Gateway: gateway})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		revoked = true
		return s.audit(ctx, q, AuditCredentialRevoke, row)
	})
	if err != nil || revoked {
		return err
	}
	// No row updated: already revoked (success, no new audit row) or never there (not found).
	if _, err := q.GetBrowserGatewayCredential(ctx, db.GetBrowserGatewayCredentialParams{ID: id, WorkspaceID: ws, Gateway: gateway}); errors.Is(err, pgx.ErrNoRows) {
		return ErrCredentialNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// Owns reports whether instanceID is an instance of the principal's own gateway and workspace,
// through a credential that is still unrevoked. It is the events route's ownership check.
func (a CredentialAuth) Owns(ctx context.Context, p Principal, instanceID string) (bool, error) {
	id, err := uuid.Parse(instanceID)
	if err != nil {
		return false, nil
	}
	_, err = a.Queries.GetBrowserInstanceForCredential(ctx, db.GetBrowserInstanceForCredentialParams{ID: p.CredentialID, ID_2: pgtype.UUID{Bytes: id, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// DBStore is the Store over the lease table.
type DBStore struct{ Queries *db.Queries }

func (s DBStore) LiveLeases(ctx context.Context, instanceID string) ([]LiveLease, error) {
	id, err := uuid.Parse(instanceID)
	if err != nil {
		return nil, err
	}
	rows, err := s.Queries.ListLiveBrowserLeasesForInstance(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		return nil, err
	}
	out := make([]LiveLease, 0, len(rows))
	for _, r := range rows {
		out = append(out, LiveLease{LeaseID: uuid.UUID(r.ID.Bytes).String(), Epoch: r.Epoch.Int64, ExpiresAt: r.ExpiresAt.Time})
	}
	return out, nil
}

func (s DBStore) Lease(context.Context, string, string) (LeaseInfo, bool, error) {
	return LeaseInfo{}, false, errors.New("browserlease: lease introspection is not mounted")
}
