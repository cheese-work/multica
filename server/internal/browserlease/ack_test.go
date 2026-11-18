package browserlease

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4736 (W3-7; plan W1 r3, "Lease handover (fenced)"): cleanup-ack(E) ends a handover, the
// queue advances only after it, a stale epoch is rejected and a quarantined instance gets no grant.

type ackEnv struct {
	pool *pgxpool.Pool
	svc  *AckService
	ws   pgtype.UUID
}

func newAckEnv(t *testing.T) *ackEnv {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("explicit disposable DATABASE_URL required")
	}
	pool := testutil.IsolatedDB(t, dsn)
	return &ackEnv{pool: pool, svc: NewAckService(pool), ws: pgUUID(uuid.New())}
}

func pgUUID(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }

func (e *ackEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// instance inserts an instance at the given epoch and phase.
func (e *ackEnv) instance(t *testing.T, epoch int64, phase string) pgtype.UUID {
	t.Helper()
	id := pgUUID(uuid.New())
	e.exec(t, `INSERT INTO browser_instance (id, workspace_id, name, epoch, phase) VALUES ($1, $2, $3, $4, $5)`,
		id, e.ws, "steel-"+uuid.NewString()[:8], epoch, phase)
	return id
}

// lease inserts a lease on the instance: state queued, active or ended (ended and not acked).
func (e *ackEnv) lease(t *testing.T, instance pgtype.UUID, state string, epoch int64) pgtype.UUID {
	t.Helper()
	id := pgUUID(uuid.New())
	var ep any
	if state != "queued" {
		ep = epoch
	}
	e.exec(t, `
		INSERT INTO browser_lease (id, workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id,
			class, state, epoch, expires_at, ended_at, end_reason)
		VALUES ($1, $2, $3, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
			'S0', $4, $5, now() + interval '2 hours',
			CASE WHEN $4 = 'ended' THEN now() END, CASE WHEN $4 = 'ended' THEN 'release' END)`,
		id, e.ws, instance, state, ep)
	return id
}

func (e *ackEnv) phase(t *testing.T, instance pgtype.UUID) string {
	t.Helper()
	var p string
	if err := e.pool.QueryRow(context.Background(), `SELECT phase FROM browser_instance WHERE id = $1`, instance).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *ackEnv) acked(t *testing.T, lease pgtype.UUID) bool {
	t.Helper()
	var ok bool
	if err := e.pool.QueryRow(context.Background(), `SELECT cleanup_acked_at IS NOT NULL FROM browser_lease WHERE id = $1`, lease).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func (e *ackEnv) canGrant(t *testing.T, instance pgtype.UUID) bool {
	t.Helper()
	ok, err := e.svc.CanGrant(context.Background(), e.ws, instance)
	if err != nil {
		t.Fatalf("CanGrant: %v", err)
	}
	return ok
}

func TestCleanupAckAdvancesQueueOnlyAfterAck(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 3, "verifying")
	held := e.lease(t, inst, "ended", 3)
	e.lease(t, inst, "queued", 0)

	if e.canGrant(t, inst) {
		t.Fatal("instance grantable while the ended lease has no cleanup-ack")
	}
	res, err := e.svc.CleanupAck(context.Background(), e.ws, inst, 3)
	if err != nil {
		t.Fatalf("CleanupAck: %v", err)
	}
	if res.Epoch != 3 || res.Phase != "idle_clean" {
		t.Errorf("result = %+v, want epoch 3 phase idle_clean", res)
	}
	if !e.acked(t, held) || e.phase(t, inst) != "idle_clean" {
		t.Error("ack must stamp cleanup_acked_at and set phase idle_clean")
	}
	if !e.canGrant(t, inst) {
		t.Error("instance not grantable after cleanup-ack")
	}
}

func TestCleanupAckRejectsStaleAndFutureEpoch(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 5, "verifying")
	held := e.lease(t, inst, "ended", 5)

	for _, epoch := range []int64{4, 6} {
		_, err := e.svc.CleanupAck(context.Background(), e.ws, inst, epoch)
		if !errors.Is(err, ErrStaleEpoch) {
			t.Errorf("epoch %d: err = %v, want ErrStaleEpoch", epoch, err)
		}
	}
	if e.acked(t, held) || e.phase(t, inst) != "verifying" || e.canGrant(t, inst) {
		t.Error("a rejected ack must change nothing")
	}
}

func TestCleanupAckRefusesActiveLease(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 2, "active")
	held := e.lease(t, inst, "active", 2)

	_, err := e.svc.CleanupAck(context.Background(), e.ws, inst, 2)
	if !errors.Is(err, ErrLeaseActive) {
		t.Fatalf("err = %v, want ErrLeaseActive", err)
	}
	if e.acked(t, held) || e.phase(t, inst) != "active" {
		t.Error("an active lease must not be acked")
	}
}

func TestCleanupAckIsIdempotentForTheSameEpoch(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 1, "verifying")
	e.lease(t, inst, "ended", 1)
	for i := range 2 {
		if _, err := e.svc.CleanupAck(context.Background(), e.ws, inst, 1); err != nil {
			t.Fatalf("ack %d: %v", i+1, err)
		}
	}
	if e.phase(t, inst) != "idle_clean" {
		t.Error("phase must stay idle_clean")
	}
}

func TestCleanupAckWithoutPendingLeaseIsRefused(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 1, "resetting")
	_, err := e.svc.CleanupAck(context.Background(), e.ws, inst, 1)
	if !errors.Is(err, ErrNoPendingLease) {
		t.Fatalf("err = %v, want ErrNoPendingLease", err)
	}
}

func TestCleanupAckUnknownOrForeignInstance(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 1, "verifying")
	e.lease(t, inst, "ended", 1)
	if _, err := e.svc.CleanupAck(context.Background(), e.ws, pgUUID(uuid.New()), 1); !errors.Is(err, ErrInstanceNotFound) {
		t.Errorf("unknown instance: err = %v, want ErrInstanceNotFound", err)
	}
	if _, err := e.svc.CleanupAck(context.Background(), pgUUID(uuid.New()), inst, 1); !errors.Is(err, ErrInstanceNotFound) {
		t.Errorf("other workspace: err = %v, want ErrInstanceNotFound", err)
	}
}

func TestQuarantineStopsGrants(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 4, "idle_clean")
	if !e.canGrant(t, inst) {
		t.Fatal("clean instance without a holder must be grantable")
	}
	res, err := e.svc.Quarantine(context.Background(), e.ws, inst, 4)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if res.Phase != "quarantined" || e.phase(t, inst) != "quarantined" {
		t.Errorf("phase = %q, want quarantined", res.Phase)
	}
	if e.canGrant(t, inst) {
		t.Error("quarantined instance must not be grantable")
	}
}

func TestQuarantineRejectsStaleEpoch(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 4, "verifying")
	if _, err := e.svc.Quarantine(context.Background(), e.ws, inst, 3); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("err = %v, want ErrStaleEpoch", err)
	}
	if e.phase(t, inst) != "verifying" {
		t.Error("a stale quarantine report must change nothing")
	}
}

func TestQuarantineKeepsStaleAckOutAndClearsOnFreshAck(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 2, "verifying")
	held := e.lease(t, inst, "ended", 2)
	if _, err := e.svc.Quarantine(context.Background(), e.ws, inst, 2); err != nil {
		t.Fatal(err)
	}
	if e.canGrant(t, inst) {
		t.Fatal("quarantined instance grantable")
	}
	// The gateway reset and probed clean for epoch 2: the ack clears the quarantine.
	if _, err := e.svc.CleanupAck(context.Background(), e.ws, inst, 2); err != nil {
		t.Fatalf("ack after quarantine: %v", err)
	}
	if !e.acked(t, held) || !e.canGrant(t, inst) {
		t.Error("ack for the current epoch must clear the quarantine")
	}
	// A late duplicate ack must not clear a later quarantine.
	if _, err := e.svc.Quarantine(context.Background(), e.ws, inst, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CleanupAck(context.Background(), e.ws, inst, 2); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("duplicate ack on quarantined instance: err = %v, want ErrQuarantined", err)
	}
	if e.canGrant(t, inst) {
		t.Error("quarantine cleared by a duplicate ack")
	}
}

// ---- HTTP ----

func (e *ackEnv) server(t *testing.T, ok bool, ws pgtype.UUID) http.Handler {
	t.Helper()
	return e.svc.Routes(func(*http.Request) (pgtype.UUID, bool) { return ws, ok })
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ackPath(instance pgtype.UUID) string {
	return strings.Replace(CleanupAckPath, "{instance_id}", uuid.UUID(instance.Bytes).String(), 1)
}

func quarantinePath(instance pgtype.UUID) string {
	return strings.Replace(QuarantinePath, "{instance_id}", uuid.UUID(instance.Bytes).String(), 1)
}

func TestAckRouteStatusCodes(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 3, "verifying")
	held := e.lease(t, inst, "ended", 3)
	h := e.server(t, true, e.ws)

	cases := []struct {
		name, path, body string
		handler          http.Handler
		want             int
		code             string
	}{
		{"stale epoch", ackPath(inst), `{"epoch":2}`, h, 409, "stale_epoch"},
		{"zero epoch", ackPath(inst), `{"epoch":0}`, h, 400, "invalid_request"},
		{"missing epoch", ackPath(inst), `{}`, h, 400, "invalid_request"},
		{"bad json", ackPath(inst), `{`, h, 400, "invalid_request"},
		{"unknown field", ackPath(inst), `{"epoch":3,"x":1}`, h, 400, "invalid_request"},
		{"bad instance id", strings.Replace(CleanupAckPath, "{instance_id}", "nope", 1), `{"epoch":3}`, h, 400, "invalid_request"},
		{"unknown instance", ackPath(pgUUID(uuid.New())), `{"epoch":3}`, h, 404, "not_found"},
		{"unauthenticated", ackPath(inst), `{"epoch":3}`, e.server(t, false, pgtype.UUID{}), 401, "unauthorized"},
	}
	for _, tc := range cases {
		rec := post(tc.handler, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			continue
		}
		var out struct{ Error string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Error != tc.code {
			t.Errorf("%s: error %q, want %q", tc.name, out.Error, tc.code)
		}
	}
	if e.acked(t, held) {
		t.Fatal("a rejected request acked the lease")
	}

	rec := post(h, ackPath(inst), `{"epoch":3}`)
	if rec.Code != 200 {
		t.Fatalf("ack: status %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		InstanceID string `json:"instance_id"`
		Epoch      int64  `json:"epoch"`
		Phase      string `json:"phase"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Epoch != 3 || out.Phase != "idle_clean" || out.InstanceID != uuid.UUID(inst.Bytes).String() {
		t.Errorf("body = %+v", out)
	}
	if !e.acked(t, held) {
		t.Error("ack route did not stamp the lease")
	}
}

func TestAckRouteQuarantineAndActiveLeaseCodes(t *testing.T) {
	e := newAckEnv(t)
	h := e.server(t, true, e.ws)

	active := e.instance(t, 1, "active")
	e.lease(t, active, "active", 1)
	if rec := post(h, ackPath(active), `{"epoch":1}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "lease_not_ended") {
		t.Errorf("active lease: %d %s", rec.Code, rec.Body.String())
	}

	q := e.instance(t, 1, "verifying")
	if rec := post(h, quarantinePath(q), `{"epoch":1}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), "quarantined") {
		t.Fatalf("quarantine: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(h, quarantinePath(q), `{"epoch":0}`); rec.Code != 400 {
		t.Errorf("quarantine epoch 0: %d", rec.Code)
	}
	if rec := post(h, ackPath(q), `{"epoch":1}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "quarantined") {
		t.Errorf("ack on quarantined, no pending lease: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRoutesNeedAnAuthenticator(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Routes(nil) must panic: never serve an unauthenticated state-changing route")
		}
	}()
	NewAckService(nil).Routes(nil)
}

// ---- queue advance (F2) ----

// handover mints a on the only instance, queues b behind it, and ends a. The instance now
// waits for cleanup-ack(1).
func handover(t *testing.T) (e *env, ack *AckService, a, b Result) {
	t.Helper()
	e = newEnv(t, "x99-1")
	ack = &AckService{pool: testPool, Queue: e.svc}
	p := e.profile(ClassS0)
	a, _ = e.mint(p)
	e.now = e.now.Add(time.Second)
	b, _ = e.mint(p)
	if b.Lease.State != "queued" {
		t.Fatalf("b = %s, want queued", b.Lease.State)
	}
	if err := e.svc.Release(context.Background(), e.ws, a.Lease.TaskID, a.Lease.ID); err != nil {
		t.Fatal(err)
	}
	return e, ack, a, b
}

func TestCleanupAckGrantsTheWaiterAfterTheCurrentEpochAck(t *testing.T) {
	e, ack, a, b := handover(t)
	if l := e.lease(b.Lease.ID); l.State != "queued" {
		t.Fatalf("b = %s before the ack, want queued", l.State)
	}
	if _, err := ack.CleanupAck(context.Background(), e.ws, a.Lease.InstanceID, 1); err != nil {
		t.Fatalf("CleanupAck: %v", err)
	}
	if l := e.lease(b.Lease.ID); l.State != "active" || l.Epoch.Int64 != 2 {
		t.Fatalf("b = %s epoch %d after the ack, want active epoch 2", l.State, l.Epoch.Int64)
	}
}

func TestStaleAckAndQuarantineGrantNothing(t *testing.T) {
	e, ack, a, b := handover(t)
	if _, err := ack.CleanupAck(context.Background(), e.ws, a.Lease.InstanceID, 2); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale ack: err = %v, want ErrStaleEpoch", err)
	}
	if _, err := ack.Quarantine(context.Background(), e.ws, a.Lease.InstanceID, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AdvanceQueue(context.Background(), e.ws, ClassS0); err != nil {
		t.Fatal(err)
	}
	if l := e.lease(b.Lease.ID); l.State != "queued" {
		t.Fatalf("b = %s, want queued: a stale ack or a quarantine granted it", l.State)
	}
}

func TestRetriedAckAdvancesAQueueThatTheFirstAckLeftWaiting(t *testing.T) {
	e, _, a, b := handover(t)
	first := &AckService{pool: testPool} // the advance did not run (or failed) after the first ack
	if _, err := first.CleanupAck(context.Background(), e.ws, a.Lease.InstanceID, 1); err != nil {
		t.Fatal(err)
	}
	if l := e.lease(b.Lease.ID); l.State != "queued" {
		t.Fatalf("b = %s, want queued", l.State)
	}
	retry := &AckService{pool: testPool, Queue: e.svc}
	if _, err := retry.CleanupAck(context.Background(), e.ws, a.Lease.InstanceID, 1); err != nil {
		t.Fatal(err)
	}
	if l := e.lease(b.Lease.ID); l.State != "active" {
		t.Fatalf("b = %s after the retried ack, want active", l.State)
	}
}

// An ended H lease and a queued S2 lease share the one C00 instance. The first ack must hand the
// instance to the S2 waiter: it advances every class the instance can serve, not only the ended
// lease's class. H mint is deferred, so the holder is an S2 lease re-labelled H. A waiting H
// lease is not tested: H is never leased to an agent (W3-12), so a queued H lease is dropped.
func TestFirstAckAdvancesAWaiterOfAnotherClassOnTheSameInstance(t *testing.T) {
	for _, tc := range []struct{ name, ended, waiting string }{
		{"ended H, waiting S2", ClassH, ClassS2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, "c00-1")
			ack := &AckService{pool: testPool, Queue: e.svc}
			p := e.profile(ClassS2)
			a, _ := e.mint(p)
			e.now = e.now.Add(time.Second)
			b, _ := e.mint(p)
			if b.Lease.State != "queued" {
				t.Fatalf("b = %s, want queued", b.Lease.State)
			}
			for id, class := range map[pgtype.UUID]string{a.Lease.ID: tc.ended, b.Lease.ID: tc.waiting} {
				if _, err := testPool.Exec(context.Background(), `UPDATE browser_lease SET class = $2 WHERE id = $1`, id, class); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.svc.Release(context.Background(), e.ws, a.Lease.TaskID, a.Lease.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := ack.CleanupAck(context.Background(), e.ws, a.Lease.InstanceID, 1); err != nil {
				t.Fatalf("CleanupAck: %v", err)
			}
			if l := e.lease(b.Lease.ID); l.State != "active" || l.Epoch.Int64 != 2 {
				t.Fatalf("b = %s epoch %d after the first ack, want active epoch 2", l.State, l.Epoch.Int64)
			}
		})
	}
}
