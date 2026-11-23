package browserlease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func jtiHash(jti string) string { s := sha256.Sum256([]byte(jti)); return hex.EncodeToString(s[:]) }

func storedJTIHash(t *testing.T, e *env) pgtype.Text {
	t.Helper()
	var h pgtype.Text
	if err := testPool.QueryRow(context.Background(), `SELECT jti_hash FROM browser_lease WHERE workspace_id = $1`, e.ws).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

type mintOutcome struct {
	res Result
	err error
}

// TestInitialSigningInTxnReturnsTokenOnlyAfterCommit: the initial token is signed and its jti
// recorded inside one transaction, and the caller gets it only once that transaction commits.
func TestInitialSigningInTxnReturnsTokenOnlyAfterCommit(t *testing.T) {
	t.Run("returned after commit", func(t *testing.T) {
		e := newEnv(t, "x99-1")
		reached, release := make(chan struct{}), make(chan struct{})
		e.svc.hook = func(point string) {
			if point == "sign:before-commit" {
				close(reached)
				<-release
			}
		}
		done := make(chan mintOutcome, 1)
		go func() {
			res, err := e.svc.Mint(context.Background(), e.req(e.profile(ClassS0)))
			done <- mintOutcome{res, err}
		}()
		select {
		case <-reached:
		case <-time.After(3 * time.Second):
			t.Fatal("initial signing never reached the commit point inside a transaction")
		}
		if h := storedJTIHash(t, e); h.Valid {
			t.Fatalf("jti_hash %q is visible before the signing transaction commits", h.String)
		}
		select {
		case out := <-done:
			t.Fatalf("Mint returned (token %q, err %v) before the signing transaction committed", out.res.Token, out.err)
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		out := <-done
		if out.err != nil || out.res.Token == "" {
			t.Fatalf("token = %q, err = %v", out.res.Token, out.err)
		}
		if got, want := storedJTIHash(t, e).String, jtiHash(e.signer.claims[0].JTI); got != want {
			t.Fatalf("jti_hash = %q, want %q", got, want)
		}
	})

	t.Run("no token when the commit fails", func(t *testing.T) {
		e := newEnv(t, "x99-1")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fired := false
		e.svc.hook = func(point string) {
			if point == "sign:before-commit" {
				fired = true
				cancel() // the commit that follows cannot succeed
			}
		}
		res, err := e.svc.Mint(ctx, e.req(e.profile(ClassS0)))
		if !fired {
			t.Fatal("initial signing never reached the commit point inside a transaction")
		}
		if err == nil || res.Token != "" {
			t.Fatalf("token = %q, err = %v, want an error and no token", res.Token, err)
		}
		if h := storedJTIHash(t, e); h.Valid {
			t.Fatalf("jti_hash %q survived a failed commit", h.String)
		}
	})
}

// TestNoTokenForEndedOrStaleEpochLease: a lease that ended, or whose instance epoch moved on,
// between the grant and the signing transaction gets no token, at initial signing and at renewal.
func TestNoTokenForEndedOrStaleEpochLease(t *testing.T) {
	spoil := map[string]func(*testing.T, *env){
		"ended": func(t *testing.T, e *env) {
			l, err := e.svc.Queries.GetBrowserLease(context.Background(), leaseParams(t, e))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.svc.endActive(context.Background(), l, endReleased); err != nil {
				t.Fatal(err)
			}
		},
		"stale epoch": func(t *testing.T, e *env) {
			if _, err := testPool.Exec(context.Background(), `UPDATE browser_instance SET epoch = epoch + 1 WHERE workspace_id = $1`, e.ws); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, doSpoil := range spoil {
		t.Run("initial/"+name, func(t *testing.T) {
			e := newEnv(t, "x99-1")
			e.svc.hook = func(point string) {
				if point == "sign:begin" {
					doSpoil(t, e)
				}
			}
			res, err := e.svc.Mint(context.Background(), e.req(e.profile(ClassS0)))
			if !errors.Is(err, ErrLeaseEnded) || res.Token != "" || len(e.signer.claims) != 0 {
				t.Fatalf("err = %v, token = %q, signed = %d, want ErrLeaseEnded and no token", err, res.Token, len(e.signer.claims))
			}
			if h := storedJTIHash(t, e); h.Valid {
				t.Fatalf("jti_hash %q stored for a lease that got no token", h.String)
			}
		})
		t.Run("renewal/"+name, func(t *testing.T) {
			e := newEnv(t, "x99-1")
			first, r := e.mint(e.profile(ClassS0))
			e.svc.hook = func(point string) {
				if point == "sign:begin" {
					doSpoil(t, e)
				}
			}
			res, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, first.Lease.ID)
			if !errors.Is(err, ErrLeaseEnded) || res.Token != "" || len(e.signer.claims) != 1 {
				t.Fatalf("err = %v, token = %q, signed = %d, want ErrLeaseEnded, no token, one earlier token", err, res.Token, len(e.signer.claims))
			}
		})
	}
}

// TestValidateClaimsRefusesAudOutsideAllowlist: the allowlist, not a built-in pair of hosts,
// decides which `aud` is signed.
func TestValidateClaimsRefusesAudOutsideAllowlist(t *testing.T) {
	allow := NewAudienceSet("steel-gateway/c00", "steel-gateway/x99")
	c := claims()
	if err := validateClaims(c, allow); err != nil {
		t.Fatalf("allowlisted aud refused: %v", err)
	}
	c.Aud = "steel-gateway/h3"
	if err := validateClaims(c, allow); err == nil {
		t.Fatal("aud outside the allowlist signed")
	}
	if err := validateClaims(c, NewAudienceSet("steel-gateway/h3")); err != nil {
		t.Fatalf("a third host on the allowlist is refused: %v", err)
	}
	c.Aud = "steel-gateway/c00"
	if err := validateClaims(c, NewAudienceSet("steel-gateway/x99")); err == nil {
		t.Fatal("a legacy audience that the allowlist omits signed")
	}
}

// TestEmptyAllowlistSignsNothing: no allowlist, no token and no grant.
func TestEmptyAllowlistSignsNothing(t *testing.T) {
	for name, allow := range map[string]AudienceSet{"nil": nil, "empty": NewAudienceSet()} {
		if err := validateClaims(claims(), allow); err == nil {
			t.Errorf("%s allowlist: claims validated", name)
		}
	}
	kr, err := ParseKeyring("k1:" + seed(1)) // no WithAudiences
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := kr.Sign(claims()); err == nil || tok != "" {
		t.Fatal("a keyring without an allowlist signed")
	}

	e := newEnv(t, "x99-1")
	e.svc.Audiences = nil
	res, err := e.svc.Mint(context.Background(), e.req(e.profile(ClassS0)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Lease.State != "queued" || res.Token != "" || len(e.signer.claims) != 0 {
		t.Fatalf("lease = %s, token = %q, signed = %d, want queued, no token", res.Lease.State, res.Token, len(e.signer.claims))
	}
	if epoch := instanceEpoch(t, e, "x99-1"); epoch != 0 {
		t.Fatalf("instance epoch = %d: an instance was claimed with an empty allowlist", epoch)
	}
}

// TestPrefixLookalikeAudRefused: membership is exact; no prefix, case or whitespace variant of an
// allowlisted audience is signed.
func TestPrefixLookalikeAudRefused(t *testing.T) {
	allow := NewAudienceSet("steel-gateway/c00")
	for _, aud := range []string{
		"steel-gateway/c00x", "steel-gateway/c0", "steel-gateway/c00/", "steel-gateway/c00-1", "steel-gateway/",
		"steel-gateway", "Steel-Gateway/c00", "steel-gateway/C00", " steel-gateway/c00", "steel-gateway/c00 ", "steel-gateway/c00\n",
		"steel-gateway/c00,steel-gateway/x99", "",
	} {
		c := claims()
		c.Aud = aud
		if err := validateClaims(c, allow); err == nil {
			t.Errorf("aud %q signed against an allowlist of steel-gateway/c00", aud)
		}
	}
	if err := validateClaims(claims(), allow); err != nil {
		t.Fatalf("the exact audience is refused: %v", err)
	}
}

var _ = pgtype.UUID{}
