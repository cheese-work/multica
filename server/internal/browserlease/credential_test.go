package browserlease

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-8883 (A1b of plan CHE-8829 r16): per-gateway credentials on the internal routes.

// plainCredential inserts an unrevoked credential with a known plaintext, so a test can present
// it as a bearer.
func (e *ackEnv) plainCredential(t *testing.T, ws pgtype.UUID, gateway string) (string, Principal) {
	t.Helper()
	plaintext := CredentialPrefix + uuid.NewString()
	row, err := db.New(e.pool).CreateBrowserGatewayCredential(context.Background(), db.CreateBrowserGatewayCredentialParams{
		WorkspaceID: ws, Gateway: gateway, CredentialHash: HashCredential(plaintext),
	})
	if err != nil {
		t.Fatal(err)
	}
	return plaintext, Principal{CredentialID: row.ID, WorkspaceID: ws, Gateway: gateway}
}

func (e *ackEnv) revoke(t *testing.T, p Principal) {
	t.Helper()
	e.exec(t, `UPDATE browser_gateway_credential SET revoked_at = clock_timestamp() WHERE id = $1`, p.CredentialID)
}

// instanceOn inserts an idle_clean instance of the workspace on a gateway.
func (e *ackEnv) instanceOn(t *testing.T, ws pgtype.UUID, name, gateway string) pgtype.UUID {
	t.Helper()
	id := pgUUID(uuid.New())
	classes := []string{"S2", "H"}
	if gateway == "x99" {
		classes = []string{"S0"}
	}
	e.exec(t, `INSERT INTO browser_instance (id, workspace_id, name, host, gateway, allowed_classes, epoch, phase) VALUES ($1, $2, $3, $4, $4, $5, 1, 'idle_clean')`, id, ws, name, gateway, classes)
	return id
}

// realRoutes is the ack route set behind the real credential authenticator.
func (e *ackEnv) realRoutes() http.Handler {
	return e.svc.Routes(CredentialAuth{Queries: db.New(e.pool)}.Authenticate)
}

func bearerPost(h http.Handler, path, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A plugin install token ("mpi_") authenticates nothing on the internal routes, even when its hash
// happens to sit in the credential table: the prefix alone refuses it. Missing, malformed and
// unknown bearers are the same 401.
func TestInstallTokenOfAnyPluginIs401(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instanceOn(t, e.ws, "steel-1", "c00")
	good, _ := e.plainCredential(t, e.ws, "c00")
	planted := "mpi_" + uuid.NewString()
	e.exec(t, `INSERT INTO browser_gateway_credential (workspace_id, gateway, credential_hash) VALUES ($1, 'c00', $2)`, e.ws, HashCredential(planted))

	ack := e.realRoutes()
	events := &Gateway{
		Store: DBStore{Queries: db.New(e.pool)}, Broker: &Broker{},
		Authenticate: CredentialAuth{Queries: db.New(e.pool)}.Authenticate,
		Owns:         CredentialAuth{Queries: db.New(e.pool)}.Owns,
	}
	r := chi.NewRouter()
	r.Method(http.MethodGet, EventsPath, events.EventsHandler())

	for _, bearer := range []string{planted, "mpi_" + uuid.NewString(), "mpi_", "mbc_", "mbc_" + uuid.NewString(), "", good + "x"} {
		if rec := bearerPost(ack, ackPath(inst), bearer, `{"epoch":1}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("cleanup-ack with %q: %d, want 401", bearer, rec.Code)
		}
		if rec := bearerPost(ack, quarantinePath(inst), bearer, `{"epoch":1}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("quarantine with %q: %d, want 401", bearer, rec.Code)
		}
		if rec := get(r, "/internal/browser/instances/"+uuid.UUID(inst.Bytes).String()+"/events", bearer); rec.Code != http.StatusUnauthorized {
			t.Errorf("events with %q: %d, want 401", bearer, rec.Code)
		}
	}
	// The bearer scheme is required.
	req := httptest.NewRequest(http.MethodPost, ackPath(inst), strings.NewReader(`{"epoch":1}`))
	req.Header.Set("Authorization", good)
	rec := httptest.NewRecorder()
	ack.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a credential without the Bearer scheme: %d, want 401", rec.Code)
	}
}

// A credential resolves only to instances of its own workspace and gateway; every other instance
// is the one 404, on every internal route, whether it exists or not.
func TestCredentialSeesOnlyOwnGatewayInstances(t *testing.T) {
	e := newAckEnv(t)
	c00 := e.instanceOn(t, e.ws, "c00-1", "c00")
	x99 := e.instanceOn(t, e.ws, "x99-1", "x99")
	plain, p := e.plainCredential(t, e.ws, "c00")
	auth := CredentialAuth{Queries: db.New(e.pool)}

	if ok, err := auth.Owns(context.Background(), p, uuid.UUID(c00.Bytes).String()); err != nil || !ok {
		t.Fatalf("own gateway instance: owns=%v err=%v, want true", ok, err)
	}
	for name, id := range map[string]string{
		"other gateway": uuid.UUID(x99.Bytes).String(), "unknown": uuid.NewString(), "malformed": "not-a-uuid",
	} {
		if ok, err := auth.Owns(context.Background(), p, id); err != nil || ok {
			t.Errorf("%s: owns=%v err=%v, want false", name, ok, err)
		}
	}

	h := e.realRoutes()
	var bodies []string
	for _, inst := range []pgtype.UUID{x99, pgUUID(uuid.New())} {
		for _, path := range []string{ackPath(inst), quarantinePath(inst)} {
			rec := bearerPost(h, path, plain, `{"epoch":1}`)
			if rec.Code != http.StatusNotFound {
				t.Errorf("POST %s: %d, want 404", path, rec.Code)
			}
			bodies = append(bodies, rec.Body.String())
		}
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Errorf("a foreign instance and a missing one answer differently: %q vs %q", bodies[0], b)
		}
	}
	if _, err := e.svc.Quarantine(context.Background(), p, x99, 1); !errors.Is(err, ErrInstanceNotFound) {
		t.Errorf("quarantine of another gateway's instance: %v, want ErrInstanceNotFound", err)
	}
	if got := e.phase(t, x99); got != "idle_clean" {
		t.Errorf("the other gateway's instance changed to %q", got)
	}
}

// A grant needs an unrevoked credential for the instance's gateway (eligibility E): with none, or
// only another gateway's, the request queues and no instance leaves idle_clean; issuing one lets
// the next grant through.
func TestNoCredentialNoGrant(t *testing.T) {
	e := newEnv(t, "c00-1")
	if _, err := testPool.Exec(context.Background(), `UPDATE browser_gateway_credential SET revoked_at = now() WHERE workspace_id = $1`, e.ws); err != nil {
		t.Fatal(err)
	}
	p := e.profile(ClassS2)
	idle := func(why string) {
		t.Helper()
		var phase string
		var epoch int64
		if err := testPool.QueryRow(context.Background(), `SELECT phase, epoch FROM browser_instance WHERE workspace_id = $1`, e.ws).Scan(&phase, &epoch); err != nil {
			t.Fatal(err)
		}
		if phase != "idle_clean" || epoch != 0 {
			t.Fatalf("%s: instance is %s at epoch %d, want idle_clean at 0", why, phase, epoch)
		}
	}
	res, err := e.svc.Mint(context.Background(), e.req(p))
	if err != nil && !errors.Is(err, ErrNoInstance) {
		t.Fatal(err)
	}
	if err == nil && res.Lease.State == "active" {
		t.Fatal("granted with only a revoked credential")
	}
	idle("revoked credential only")

	e.credential("x99") // another gateway's credential does not cover a c00 instance
	res, err = e.svc.Mint(context.Background(), e.req(p))
	if err != nil && !errors.Is(err, ErrNoInstance) {
		t.Fatal(err)
	}
	if err == nil && res.Lease.State == "active" {
		t.Fatal("granted a c00 instance on an x99 credential")
	}
	idle("x99 credential for a c00 instance")

	e.credential("c00")
	// The earlier requests are still queued ahead of this one (FIFO), so the head takes the grant.
	if _, err = e.svc.Mint(context.Background(), e.req(p)); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM browser_lease WHERE workspace_id = $1 AND state = 'active'`, e.ws).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("with an unrevoked c00 credential: %d active leases, want 1", active)
	}
}

// A held instance (ended lease, not yet acked) is released only by a valid ack: not by a revoked
// credential, another gateway's or another workspace's.
func TestHeldInstanceReleasedOnlyByValidAck(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 3, "verifying")
	held := e.lease(t, inst, "ended", 3)
	revokedPlain, revoked := e.plainCredential(t, e.ws, "c00")
	e.revoke(t, revoked)
	otherGwPlain, _ := e.plainCredential(t, e.ws, "x99")
	otherWsPlain, _ := e.plainCredential(t, pgUUID(uuid.New()), "c00")
	h := e.realRoutes()

	for name, c := range map[string]struct {
		bearer string
		want   int
	}{
		"revoked credential": {revokedPlain, http.StatusUnauthorized},
		"other gateway":      {otherGwPlain, http.StatusNotFound},
		"other workspace":    {otherWsPlain, http.StatusNotFound},
	} {
		if rec := bearerPost(h, ackPath(inst), c.bearer, `{"epoch":3}`); rec.Code != c.want {
			t.Errorf("%s: %d, want %d", name, rec.Code, c.want)
		}
		if e.acked(t, held) || e.canGrant(t, inst) || e.phase(t, inst) != "verifying" {
			t.Fatalf("%s released the held instance", name)
		}
	}

	// A credential revoked after authentication is refused under the lock, not just at the door.
	if _, err := e.svc.CleanupAck(context.Background(), revoked, inst, 3); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("revoked principal at the service: err = %v, want ErrCredentialRevoked", err)
	}
	if e.acked(t, held) {
		t.Fatal("a revoked credential acked the lease")
	}

	validPlain, _ := e.plainCredential(t, e.ws, "c00")
	if rec := bearerPost(h, ackPath(inst), validPlain, `{"epoch":3}`); rec.Code != http.StatusOK {
		t.Fatalf("valid ack: %d %s", rec.Code, rec.Body.String())
	}
	if !e.acked(t, held) || e.phase(t, inst) != "idle_clean" || !e.canGrant(t, inst) {
		t.Fatal("a valid ack did not release the held instance")
	}
}

// The same instance name in two workspaces is two instances: a credential never reaches the other
// workspace's, and the other's state does not change.
func TestSameHostNameInTwoWorkspacesNeverCrosses(t *testing.T) {
	e := newAckEnv(t)
	wsB := pgUUID(uuid.New())
	a := e.instanceOn(t, e.ws, "steel-1", "c00")
	b := e.instanceOn(t, wsB, "steel-1", "c00")
	e.exec(t, `UPDATE browser_instance SET phase = 'verifying' WHERE id = $1`, b)
	leaseB := pgUUID(uuid.New())
	e.exec(t, `
		INSERT INTO browser_lease (id, workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id, class, state, epoch, expires_at, ended_at, end_reason)
		VALUES ($1, $2, $3, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'S0', 'ended', 1, now() + interval '2 hours', now(), 'release')`, leaseB, wsB, b)

	plainA, pA := e.plainCredential(t, e.ws, "c00")
	plainB, _ := e.plainCredential(t, wsB, "c00")
	auth := CredentialAuth{Queries: db.New(e.pool)}
	if ok, _ := auth.Owns(context.Background(), pA, uuid.UUID(a.Bytes).String()); !ok {
		t.Fatal("A's credential does not own A's instance")
	}
	if ok, _ := auth.Owns(context.Background(), pA, uuid.UUID(b.Bytes).String()); ok {
		t.Fatal("A's credential owns B's same-named instance")
	}
	h := e.realRoutes()
	if rec := bearerPost(h, ackPath(b), plainA, `{"epoch":1}`); rec.Code != http.StatusNotFound {
		t.Fatalf("A's credential on B's instance: %d, want 404", rec.Code)
	}
	if e.acked(t, leaseB) || e.phase(t, b) != "verifying" {
		t.Fatal("A's credential changed B's instance")
	}
	if rec := bearerPost(h, ackPath(b), plainB, `{"epoch":1}`); rec.Code != http.StatusOK {
		t.Fatalf("B's credential on B's instance: %d %s", rec.Code, rec.Body.String())
	}
	if !e.acked(t, leaseB) {
		t.Fatal("B's own credential could not ack B's lease")
	}
}

// Another workspace's credential gets the same 404 as an unknown instance, with the same body.
func TestOtherWorkspaceCredentialIs404(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instanceOn(t, e.ws, "steel-1", "c00")
	otherPlain, _ := e.plainCredential(t, pgUUID(uuid.New()), "c00")
	h := e.realRoutes()
	foreign := bearerPost(h, ackPath(inst), otherPlain, `{"epoch":1}`)
	missing := bearerPost(h, ackPath(pgUUID(uuid.New())), otherPlain, `{"epoch":1}`)
	if foreign.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || foreign.Body.String() != missing.Body.String() {
		t.Fatalf("foreign = %d %q, missing = %d %q; want one identical 404", foreign.Code, foreign.Body.String(), missing.Code, missing.Body.String())
	}

	events := &Gateway{
		Store: DBStore{Queries: db.New(e.pool)}, Broker: &Broker{},
		Authenticate: CredentialAuth{Queries: db.New(e.pool)}.Authenticate,
		Owns:         CredentialAuth{Queries: db.New(e.pool)}.Owns,
	}
	r := chi.NewRouter()
	r.Method(http.MethodGet, EventsPath, events.EventsHandler())
	fe := get(r, "/internal/browser/instances/"+uuid.UUID(inst.Bytes).String()+"/events", otherPlain)
	me := get(r, "/internal/browser/instances/"+uuid.NewString()+"/events", otherPlain)
	if fe.Code != http.StatusNotFound || me.Code != http.StatusNotFound || fe.Body.String() != me.Body.String() {
		t.Fatalf("events: foreign = %d %q, missing = %d %q; want one identical 404", fe.Code, fe.Body.String(), me.Code, me.Body.String())
	}
}
