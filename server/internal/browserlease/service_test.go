package browserlease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica_test?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err == nil {
		err = pool.Ping(context.Background())
	}
	if err != nil {
		testutil.ExitWithoutDatabase(err)
	}
	testPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

type fakeSigner struct {
	mu     sync.Mutex
	claims []Claims
}

func (f *fakeSigner) Sign(c Claims) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, c)
	return fmt.Sprintf("tok.%s.%d", c.JTI, len(f.claims)), nil
}

type fakeHooks struct {
	mu          sync.Mutex
	gateErr     map[string]error // by task id
	decisionErr error
	decisionExp *time.Time
	blockNext   chan struct{} // the next Validate returns its expiry only after this closes
}

func (f *fakeHooks) Check(_ context.Context, s Subject) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gateErr[uuid.UUID(s.TaskID.Bytes).String()]
}

func (f *fakeHooks) Validate(_ context.Context, s Subject) (*time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.Profile.Class == ClassS2 && !s.DecisionID.Valid {
		return nil, ErrDecisionRequired
	}
	exp, err := f.decisionExp, f.decisionErr
	if ch := f.blockNext; ch != nil {
		f.blockNext = nil
		f.mu.Unlock()
		<-ch
		f.mu.Lock()
	}
	return exp, err
}

type env struct {
	t      *testing.T
	svc    *Service
	hooks  *fakeHooks
	signer *fakeSigner
	ws     pgtype.UUID
	now    time.Time
}

func newUUID() pgtype.UUID { return pgtype.UUID{Bytes: uuid.New(), Valid: true} }

func newEnv(t *testing.T, instances ...string) *env {
	t.Helper()
	q := db.New(testPool)
	e := &env{
		t: t, ws: newUUID(), hooks: &fakeHooks{gateErr: map[string]error{}}, signer: &fakeSigner{},
		now: time.Now().UTC().Truncate(time.Microsecond),
	}
	e.svc = &Service{
		Pool: testPool, Queries: q, Gate: e.hooks, Decisions: e.hooks,
		Registry: NameRegistry{Queries: q}, Signer: e.signer, Now: func() time.Time { return e.now },
		Class3: testClass3(t),
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"browser_lease", "browser_instance", "browser_profile"} {
			testPool.Exec(context.Background(), "DELETE FROM "+tbl+" WHERE workspace_id = $1", e.ws)
		}
	})
	for _, name := range instances {
		if _, err := q.CreateBrowserInstance(context.Background(), db.CreateBrowserInstanceParams{WorkspaceID: e.ws, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) profile(class string) db.BrowserProfile {
	e.t.Helper()
	p, err := e.svc.Queries.CreateBrowserProfile(context.Background(), db.CreateBrowserProfileParams{
		WorkspaceID: e.ws, Name: "p-" + uuid.NewString(), Class: class, Hosts: hostsFor(class),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) profileWithHosts(class string, hosts ...string) db.BrowserProfile {
	e.t.Helper()
	if hosts == nil {
		hosts = []string{}
	}
	p, err := e.svc.Queries.CreateBrowserProfile(context.Background(), db.CreateBrowserProfileParams{
		WorkspaceID: e.ws, Name: "p-" + uuid.NewString(), Class: class, Hosts: hosts,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) req(p db.BrowserProfile) MintRequest {
	r := MintRequest{
		WorkspaceID: e.ws, TaskID: newUUID(), AgentID: newUUID(), IssueID: newUUID(),
		UserID: newUUID(), ProfileID: p.ID,
	}
	if p.Class == ClassS2 {
		r.DecisionID = newUUID()
	}
	return r
}

func (e *env) mint(p db.BrowserProfile) (Result, MintRequest) {
	e.t.Helper()
	r := e.req(p)
	res, err := e.svc.Mint(context.Background(), r)
	if err != nil {
		e.t.Fatalf("mint: %v", err)
	}
	return res, r
}

// ack stands in for W3-7: the gateway confirmed the reset.
func (e *env) ack(leaseID pgtype.UUID) {
	e.t.Helper()
	var inst pgtype.UUID
	if err := testPool.QueryRow(context.Background(),
		`UPDATE browser_lease SET cleanup_acked_at = now() WHERE id = $1 RETURNING instance_id`, leaseID).Scan(&inst); err != nil {
		e.t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE browser_instance SET phase = 'idle_clean' WHERE id = $1`, inst); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) lease(id pgtype.UUID) db.BrowserLease {
	e.t.Helper()
	l, err := e.svc.Queries.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: id, WorkspaceID: e.ws})
	if err != nil {
		e.t.Fatalf("get lease: %v", err)
	}
	return l
}

func TestMintS0GrantsFirstIdleX99Instance(t *testing.T) {
	e := newEnv(t, "x99-1", "x99-2", "c00-1")
	res, r := e.mint(e.profile(ClassS0))
	if res.Lease.State != "active" || res.Lease.Epoch.Int64 != 1 {
		t.Fatalf("lease = %s epoch %d, want active epoch 1", res.Lease.State, res.Lease.Epoch.Int64)
	}
	inst, _ := e.svc.Queries.GetBrowserInstance(context.Background(), db.GetBrowserInstanceParams{ID: res.Lease.InstanceID, WorkspaceID: e.ws})
	if inst.Name != "x99-1" || inst.Phase != "starting" || inst.Epoch != 1 {
		t.Fatalf("instance = %s/%s/%d, want x99-1/starting/1", inst.Name, inst.Phase, inst.Epoch)
	}
	if res.Token == "" || len(e.signer.claims) != 1 {
		t.Fatal("no token signed")
	}
	c := e.signer.claims[0]
	if c.Aud != "steel-gateway/x99" || c.Class != ClassS0 || c.Epoch != 1 || c.DecisionID != "" ||
		c.Sub != uuid.UUID(r.AgentID.Bytes).String() || c.UserID != uuid.UUID(r.UserID.Bytes).String() ||
		c.TaskID != uuid.UUID(r.TaskID.Bytes).String() || c.IssueID != uuid.UUID(r.IssueID.Bytes).String() {
		t.Fatalf("claims = %+v", c)
	}
	if got, want := time.Unix(c.Exp, 0), e.now.Add(TokenTTL); got.Sub(want).Abs() > time.Second {
		t.Fatalf("exp = %v, want %v", got, want)
	}
	if res.Lease.JtiHash.String == "" || strings.Contains(res.Lease.JtiHash.String, c.JTI) {
		t.Fatalf("jti_hash must be a hash of the jti, got %q", res.Lease.JtiHash.String)
	}
	if d := res.Lease.ExpiresAt.Time.Sub(e.now); d != MaxLeaseDuration {
		t.Fatalf("lease limit = %v, want %v", d, MaxLeaseDuration)
	}
}

func TestMintRefusesClassOnWrongHost(t *testing.T) {
	e := newEnv(t, "x99-1")
	_, err := e.svc.Mint(context.Background(), e.req(e.profile(ClassS2)))
	if !errors.Is(err, ErrNoInstance) {
		t.Fatalf("S2 with only an X99 instance: err = %v, want ErrNoInstance", err)
	}
	e = newEnv(t, "c00-1")
	_, err = e.svc.Mint(context.Background(), e.req(e.profile(ClassS0)))
	if !errors.Is(err, ErrNoInstance) {
		t.Fatalf("S0 with only a C00 instance: err = %v, want ErrNoInstance", err)
	}
}

func TestMintS2UsesC00AndNeedsDecision(t *testing.T) {
	e := newEnv(t, "x99-1", "c00-1")
	p := e.profile(ClassS2)
	r := e.req(p)
	r.DecisionID = pgtype.UUID{}
	if _, err := e.svc.Mint(context.Background(), r); !errors.Is(err, ErrDecisionRequired) {
		t.Fatalf("err = %v, want ErrDecisionRequired", err)
	}
	res, _ := e.mint(p)
	if got := e.signer.claims[len(e.signer.claims)-1]; got.Aud != "steel-gateway/c00" || got.DecisionID == "" {
		t.Fatalf("claims = %+v", got)
	}
	if res.Host != HostC00 {
		t.Fatalf("host = %q", res.Host)
	}
}

func TestMintRefusesHAndDeniedGateAndLeavesNoRow(t *testing.T) {
	e := newEnv(t, "c00-1")
	if _, err := e.svc.Mint(context.Background(), e.req(e.profile(ClassH))); !errors.Is(err, ErrClassNotMintable) {
		t.Fatalf("H: err = %v, want ErrClassNotMintable", err)
	}
	p := e.profile(ClassS2)
	r := e.req(p)
	e.hooks.gateErr[uuid.UUID(r.TaskID.Bytes).String()] = ErrGateDenied
	if _, err := e.svc.Mint(context.Background(), r); !errors.Is(err, ErrGateDenied) {
		t.Fatalf("gate: err = %v, want ErrGateDenied", err)
	}
	var n int
	testPool.QueryRow(context.Background(), `SELECT count(*) FROM browser_lease WHERE workspace_id = $1`, e.ws).Scan(&n)
	if n != 0 {
		t.Fatalf("%d lease rows after refused mints, want 0", n)
	}
}

func TestMintWithoutSignerLeavesNoLease(t *testing.T) {
	e := newEnv(t, "x99-1")
	e.svc.Signer = nil
	if _, err := e.svc.Mint(context.Background(), e.req(e.profile(ClassS0))); !errors.Is(err, ErrSignerMissing) {
		t.Fatalf("err = %v, want ErrSignerMissing", err)
	}
	var n int
	testPool.QueryRow(context.Background(), `SELECT count(*) FROM browser_lease WHERE workspace_id = $1`, e.ws).Scan(&n)
	if n != 0 {
		t.Fatalf("%d lease rows, want 0", n)
	}
}

func TestMintTwiceForOneTaskIsRefused(t *testing.T) {
	e := newEnv(t, "x99-1")
	p := e.profile(ClassS0)
	_, r := e.mint(p)
	if _, err := e.svc.Mint(context.Background(), r); !errors.Is(err, ErrLeaseExists) {
		t.Fatalf("err = %v, want ErrLeaseExists", err)
	}
}

func TestLeaseLimitIsTheShorterDecisionExpiry(t *testing.T) {
	e := newEnv(t, "c00-1")
	exp := e.now.Add(30 * time.Minute)
	e.hooks.decisionExp = &exp
	res, _ := e.mint(e.profile(ClassS2))
	if !res.Lease.ExpiresAt.Time.Equal(exp) {
		t.Fatalf("lease limit = %v, want %v", res.Lease.ExpiresAt.Time, exp)
	}
	e.now = exp.Add(-5 * time.Minute)
	got, err := e.svc.Renew(context.Background(), e.ws, res.Lease.TaskID, res.Lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TokenExpires.Equal(exp) {
		t.Fatalf("token exp = %v, want capped at lease limit %v", got.TokenExpires, exp)
	}
}

func TestRenewPersistsAShortenedDecisionAndNeverExtendsTheHardLimit(t *testing.T) {
	e := newEnv(t, "c00-1")
	long := e.now.Add(90 * time.Minute)
	e.hooks.decisionExp = &long
	res, _ := e.mint(e.profile(ClassS2))
	hard := res.Lease.ExpiresAt.Time
	if !hard.Equal(long) {
		t.Fatalf("lease limit = %v, want %v", hard, long)
	}
	// The decision is shortened to 10 minutes from now; renew must cap and persist it.
	short := e.now.Add(10 * time.Minute)
	e.hooks.decisionExp = &short
	e.now = e.now.Add(5 * time.Minute)
	got, err := e.svc.Renew(context.Background(), e.ws, res.Lease.TaskID, res.Lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TokenExpires.Equal(short) {
		t.Fatalf("token exp = %v, want capped at shortened decision %v", got.TokenExpires, short)
	}
	if l := e.lease(res.Lease.ID); !l.ExpiresAt.Time.Equal(short) {
		t.Fatalf("stored limit = %v, want persisted %v", l.ExpiresAt.Time, short)
	}
	// A later, longer decision must not extend the stored limit.
	longer := e.now.Add(80 * time.Minute)
	e.hooks.decisionExp = &longer
	got, err = e.svc.Renew(context.Background(), e.ws, res.Lease.TaskID, res.Lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TokenExpires.Equal(short) {
		t.Fatalf("token exp = %v, want still %v", got.TokenExpires, short)
	}
	if l := e.lease(res.Lease.ID); !l.ExpiresAt.Time.Equal(short) {
		t.Fatalf("stored limit = %v, want unextended %v", l.ExpiresAt.Time, short)
	}
	// Past the shortened deadline the lease ends as expired.
	e.now = short.Add(time.Second)
	if _, err := e.svc.Renew(context.Background(), e.ws, res.Lease.TaskID, res.Lease.ID); !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("err = %v, want ErrLeaseEnded", err)
	}
}

// Two renewals overlap: the longer one validated before the shortener and finishes after it.
// The last token the platform signs and records must still honour the shortened deadline.
func TestOverlappingRenewalNeverSignsPastAShortenedDeadline(t *testing.T) {
	e := newEnv(t, "c00-1")
	long := e.now.Add(90 * time.Minute)
	e.hooks.decisionExp = &long
	res, _ := e.mint(e.profile(ClassS2))
	taskID, leaseID := res.Lease.TaskID, res.Lease.ID

	// R2 reads the long decision, then stalls before it signs.
	gate := make(chan struct{})
	e.hooks.mu.Lock()
	e.hooks.blockNext = gate
	e.hooks.mu.Unlock()
	r2 := make(chan error, 1)
	go func() {
		_, err := e.svc.Renew(context.Background(), e.ws, taskID, leaseID)
		r2 <- err
	}()
	// Wait until R2 sits inside Validate holding the long expiry.
	for i := 0; ; i++ {
		e.hooks.mu.Lock()
		parked := e.hooks.blockNext == nil
		e.hooks.mu.Unlock()
		if parked {
			break
		}
		if i > 500 {
			t.Fatal("renewal never reached the decision check")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// R1 sees the shortened decision and completes first.
	short := e.now.Add(10 * time.Minute)
	e.hooks.mu.Lock()
	e.hooks.decisionExp = &short
	e.hooks.mu.Unlock()
	if _, err := e.svc.Renew(context.Background(), e.ws, taskID, leaseID); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if err := <-r2; err != nil {
		t.Fatal(err)
	}

	e.signer.mu.Lock()
	last := e.signer.claims[len(e.signer.claims)-1]
	e.signer.mu.Unlock()
	if last.Exp > short.Unix() {
		t.Fatalf("last signed token exp = %v, want <= shortened deadline %v", time.Unix(last.Exp, 0).UTC(), short)
	}
	sum := sha256.Sum256([]byte(last.JTI))
	if got := e.lease(leaseID); got.JtiHash.String != hex.EncodeToString(sum[:]) {
		t.Fatalf("stored jti does not belong to the last signed token")
	}
	if l := e.lease(leaseID); !l.ExpiresAt.Time.Equal(short) {
		t.Fatalf("stored limit = %v, want %v", l.ExpiresAt.Time, short)
	}
}

func TestQueueIsFirstInFirstOutAndAdvancesOnlyAfterAck(t *testing.T) {
	e := newEnv(t, "x99-1")
	p := e.profile(ClassS0)
	a, _ := e.mint(p)
	e.now = e.now.Add(time.Second)
	b, _ := e.mint(p)
	e.now = e.now.Add(time.Second)
	c, _ := e.mint(p)
	if b.Lease.State != "queued" || b.QueuePosition != 1 || b.Token != "" || c.QueuePosition != 2 {
		t.Fatalf("b = %s pos %d token %q, c pos %d", b.Lease.State, b.QueuePosition, b.Token, c.QueuePosition)
	}

	if err := e.svc.Release(context.Background(), e.ws, a.Lease.TaskID, a.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if l := e.lease(a.Lease.ID); l.State != "ended" || l.EndReason.String != "released" {
		t.Fatalf("a = %s/%s", l.State, l.EndReason.String)
	}
	if err := e.svc.AdvanceQueue(context.Background(), e.ws, ClassS0); err != nil {
		t.Fatal(err)
	}
	if l := e.lease(b.Lease.ID); l.State != "queued" {
		t.Fatalf("b advanced before cleanup-ack: %s", l.State)
	}

	e.ack(a.Lease.ID)
	if err := e.svc.AdvanceQueue(context.Background(), e.ws, ClassS0); err != nil {
		t.Fatal(err)
	}
	if l := e.lease(b.Lease.ID); l.State != "active" || l.Epoch.Int64 != 2 {
		t.Fatalf("b = %s epoch %d, want active epoch 2", l.State, l.Epoch.Int64)
	}
	if l := e.lease(c.Lease.ID); l.State != "queued" {
		t.Fatalf("c = %s, want queued", l.State)
	}
	got, err := e.svc.Renew(context.Background(), e.ws, c.Lease.TaskID, c.Lease.ID)
	if err != nil || got.QueuePosition != 1 || got.Token != "" {
		t.Fatalf("c renew: pos %d token %q err %v", got.QueuePosition, got.Token, err)
	}
	// The granted lease collects its first token with renew.
	got, err = e.svc.Renew(context.Background(), e.ws, b.Lease.TaskID, b.Lease.ID)
	if err != nil || got.Token == "" {
		t.Fatalf("b renew: token %q err %v", got.Token, err)
	}
}

func TestNewMintQueuesBehindWaitersEvenWhenAnInstanceIsIdle(t *testing.T) {
	e := newEnv(t, "x99-1", "x99-2")
	p := e.profile(ClassS0)
	a, _ := e.mint(p)
	b, _ := e.mint(p)
	if a.Lease.State != "active" || b.Lease.State != "active" || a.Lease.InstanceID == b.Lease.InstanceID {
		t.Fatal("two instances must hold two leases")
	}
	c, _ := e.mint(p)
	if c.Lease.State != "queued" || c.QueuePosition != 1 {
		t.Fatalf("c = %s pos %d", c.Lease.State, c.QueuePosition)
	}
}

func TestQueuedGrantRevalidatesAndDropsADeniedLease(t *testing.T) {
	e := newEnv(t, "x99-1")
	p := e.profile(ClassS0)
	a, _ := e.mint(p)
	e.now = e.now.Add(time.Second)
	b, br := e.mint(p)
	e.now = e.now.Add(time.Second)
	c, _ := e.mint(p)

	e.hooks.gateErr[uuid.UUID(br.TaskID.Bytes).String()] = ErrGateDenied
	e.svc.Release(context.Background(), e.ws, a.Lease.TaskID, a.Lease.ID)
	e.ack(a.Lease.ID)
	if err := e.svc.AdvanceQueue(context.Background(), e.ws, ClassS0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Queries.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: b.Lease.ID, WorkspaceID: e.ws}); err == nil {
		t.Fatal("denied queued lease must leave the queue")
	}
	if l := e.lease(c.Lease.ID); l.State != "active" || l.Epoch.Int64 != 2 {
		t.Fatalf("c = %s epoch %d, want active epoch 2", l.State, l.Epoch.Int64)
	}
}

func TestRenewRevalidatesAndEndsTheLeaseWhenDenied(t *testing.T) {
	e := newEnv(t, "x99-1")
	a, r := e.mint(e.profile(ClassS0))
	first := e.signer.claims[0].JTI
	got, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, a.Lease.ID)
	if err != nil || got.Token == "" || e.signer.claims[1].JTI == first {
		t.Fatalf("renew must sign a fresh jti: err %v", err)
	}
	e.hooks.gateErr[uuid.UUID(r.TaskID.Bytes).String()] = ErrGateDenied
	if _, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, a.Lease.ID); !errors.Is(err, ErrGateDenied) {
		t.Fatalf("err = %v, want ErrGateDenied", err)
	}
	if l := e.lease(a.Lease.ID); l.State != "ended" || l.EndReason.String != "revalidation_failed" {
		t.Fatalf("lease = %s/%s", l.State, l.EndReason.String)
	}
}

func TestRenewPastTheHardLimitEndsTheLease(t *testing.T) {
	e := newEnv(t, "x99-1")
	a, r := e.mint(e.profile(ClassS0))
	e.now = e.now.Add(MaxLeaseDuration)
	if _, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, a.Lease.ID); !errors.Is(err, ErrLeaseEnded) {
		t.Fatalf("err = %v, want ErrLeaseEnded", err)
	}
	if l := e.lease(a.Lease.ID); l.EndReason.String != "expired" {
		t.Fatalf("end reason = %q, want expired", l.EndReason.String)
	}
}

func TestRenewAndReleaseBelongToTheLeasesTask(t *testing.T) {
	e := newEnv(t, "x99-1")
	a, _ := e.mint(e.profile(ClassS0))
	other := newUUID()
	if _, err := e.svc.Renew(context.Background(), e.ws, other, a.Lease.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renew: err = %v, want ErrNotFound", err)
	}
	if err := e.svc.Release(context.Background(), e.ws, other, a.Lease.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("release: err = %v, want ErrNotFound", err)
	}
	if l := e.lease(a.Lease.ID); l.State != "active" {
		t.Fatalf("lease = %s, want active", l.State)
	}
}

func TestReleaseQueuedDeletesAndReleaseEndedIsIdempotent(t *testing.T) {
	e := newEnv(t, "x99-1")
	p := e.profile(ClassS0)
	a, _ := e.mint(p)
	b, _ := e.mint(p)
	if err := e.svc.Release(context.Background(), e.ws, b.Lease.TaskID, b.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Queries.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: b.Lease.ID, WorkspaceID: e.ws}); err == nil {
		t.Fatal("released queued lease must be gone")
	}
	for i := 0; i < 2; i++ {
		if err := e.svc.Release(context.Background(), e.ws, a.Lease.TaskID, a.Lease.ID); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}
	inst, _ := e.svc.Queries.GetBrowserInstance(context.Background(), db.GetBrowserInstanceParams{ID: a.Lease.InstanceID, WorkspaceID: e.ws})
	if inst.Phase != "draining" {
		t.Fatalf("phase = %s, want draining", inst.Phase)
	}
}

// hostsFor gives a login (S2) profile a harmless site host; S0 profiles log in nowhere.
func hostsFor(class string) []string {
	if class == ClassS2 {
		return []string{"example.com"}
	}
	return []string{}
}

func testClass3(t *testing.T) *Class3Deny {
	t.Helper()
	d, err := ParseClass3Deny(strings.NewReader(class3Fixture))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const class3Fixture = `# fixture, same shape as shared-browser steel-gateway/class3-deny.txt
[hosts]
paypal.com
dashboard.stripe.com
Mail.Google.com

[patterns]
github.com/settings/tokens
`

// reclassify moves a profile to another class after leases exist (W3-12: the class rules hold at
// every mint, renew and queued grant, not only at the first mint).
func (e *env) reclassify(p db.BrowserProfile, class string) {
	e.t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE browser_profile SET class = $2 WHERE id = $1`, p.ID, class); err != nil {
		e.t.Fatal(err)
	}
}

func TestRenewEndsTheLeaseWhenItsProfileBecomesClassH(t *testing.T) {
	e := newEnv(t, "c00-1")
	p := e.profile(ClassS2)
	a, r := e.mint(p)
	if _, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, a.Lease.ID); err != nil {
		t.Fatalf("renew while S2: %v", err)
	}
	e.reclassify(p, ClassH)
	if _, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, a.Lease.ID); !errors.Is(err, ErrClassNotMintable) {
		t.Fatalf("err = %v, want ErrClassNotMintable", err)
	}
	if l := e.lease(a.Lease.ID); l.State != "ended" || l.EndReason.String != "revalidation_failed" {
		t.Fatalf("lease = %s/%s, want ended/revalidation_failed", l.State, l.EndReason.String)
	}
}

func TestQueuedGrantDropsALeaseWhoseProfileBecomesClassH(t *testing.T) {
	e := newEnv(t, "c00-1")
	p := e.profile(ClassS2)
	a, _ := e.mint(p)
	e.now = e.now.Add(time.Second)
	b, _ := e.mint(p)
	if b.Lease.State != "queued" {
		t.Fatalf("b = %s, want queued", b.Lease.State)
	}
	e.reclassify(p, ClassH)
	e.svc.Release(context.Background(), e.ws, a.Lease.TaskID, a.Lease.ID)
	e.ack(a.Lease.ID)
	if err := e.svc.AdvanceQueue(context.Background(), e.ws, ClassS2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Queries.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: b.Lease.ID, WorkspaceID: e.ws}); err == nil {
		t.Fatal("a queued lease of a class-H profile must leave the queue, not take the instance")
	}
}

// A profile that changes class keeps no lease granted under the old class: the host, token and
// decision rule of that lease no longer match it.
func TestRenewEndsTheLeaseWhenItsProfileChangesBetweenS0AndS2(t *testing.T) {
	for _, tc := range []struct{ from, to, instance string }{
		{ClassS2, ClassS0, "c00-1"},
		{ClassS0, ClassS2, "x99-1"},
	} {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			e := newEnv(t, tc.instance)
			p := e.profile(tc.from)
			a, r := e.mint(p)
			e.reclassify(p, tc.to)
			if _, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, a.Lease.ID); !errors.Is(err, ErrClassNotMintable) {
				t.Fatalf("err = %v, want ErrClassNotMintable", err)
			}
			if l := e.lease(a.Lease.ID); l.State != "ended" || l.EndReason.String != "revalidation_failed" {
				t.Fatalf("lease = %s/%s, want ended/revalidation_failed", l.State, l.EndReason.String)
			}
		})
	}
}

func TestQueuedGrantDropsALeaseWhoseProfileChangesBetweenS0AndS2(t *testing.T) {
	for _, tc := range []struct{ from, to, instance string }{
		{ClassS2, ClassS0, "c00-1"},
		{ClassS0, ClassS2, "x99-1"},
	} {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			e := newEnv(t, tc.instance)
			p := e.profile(tc.from)
			a, _ := e.mint(p)
			e.now = e.now.Add(time.Second)
			b, _ := e.mint(p)
			e.reclassify(p, tc.to)
			e.svc.Release(context.Background(), e.ws, a.Lease.TaskID, a.Lease.ID)
			e.ack(a.Lease.ID)
			if err := e.svc.AdvanceQueue(context.Background(), e.ws, tc.from); err != nil {
				t.Fatal(err)
			}
			if _, err := e.svc.Queries.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: b.Lease.ID, WorkspaceID: e.ws}); err == nil {
				t.Fatal("a queued lease whose profile changed class must leave the queue, not take the instance")
			}
		})
	}
}
