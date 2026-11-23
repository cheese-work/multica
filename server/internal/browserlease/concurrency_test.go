package browserlease

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A mutation changes what E reads, taking its locks in the global order (instance row, then
// credential row) as the host and instance edit paths and credential revocation will. Host rows
// arrive with T4c; until then "host narrowing" moves the instance row to the other host, which no
// longer agrees with its name.
type mutation struct {
	name  string
	apply func(ctx context.Context, e *env) error
}

func inTestTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := testPool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lockInstanceTx(ctx context.Context, tx pgx.Tx, e *env, name string) error {
	var id pgtype.UUID
	return tx.QueryRow(ctx, `SELECT id FROM browser_instance WHERE workspace_id = $1 AND name = $2 FOR UPDATE`, e.ws, name).Scan(&id)
}

var mutations = []mutation{
	{"host narrowing", func(ctx context.Context, e *env) error {
		return inTestTx(ctx, func(tx pgx.Tx) error {
			if err := lockInstanceTx(ctx, tx, e, "c00-1"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE browser_instance SET host = 'x99', gateway = 'x99', allowed_classes = ARRAY['S0'] WHERE workspace_id = $1 AND name = 'c00-1'`, e.ws)
			return err
		})
	}},
	{"instance narrowing", func(ctx context.Context, e *env) error {
		return inTestTx(ctx, func(tx pgx.Tx) error {
			if err := lockInstanceTx(ctx, tx, e, "c00-1"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE browser_instance SET allowed_classes = ARRAY['H'] WHERE workspace_id = $1 AND name = 'c00-1'`, e.ws)
			return err
		})
	}},
	{"credential revocation", revokeCredential},
}

func revokeCredential(ctx context.Context, e *env) error {
	return inTestTx(ctx, func(tx pgx.Tx) error {
		if err := lockInstanceTx(ctx, tx, e, "c00-1"); err != nil {
			return err
		}
		var id pgtype.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM browser_gateway_credential WHERE workspace_id = $1 AND gateway = 'c00' FOR UPDATE`, e.ws).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE browser_gateway_credential SET revoked_at = clock_timestamp() WHERE id = $1`, id)
		return err
	})
}

func leaseRow(t *testing.T, e *env) db.BrowserLease {
	t.Helper()
	l, err := e.svc.Queries.GetBrowserLease(context.Background(), leaseParams(t, e))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func setPhase(t *testing.T, e *env, phase string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE browser_instance SET phase = $2 WHERE workspace_id = $1`, e.ws, phase); err != nil {
		t.Fatal(err)
	}
}

// queuedS2 leaves one S2 lease queued on an instance that is idle again, as after a cleanup-ack.
func queuedS2(t *testing.T, e *env) MintRequest {
	t.Helper()
	setPhase(t, e, "draining")
	r := e.req(e.profile(ClassS2))
	res, err := e.svc.Mint(context.Background(), r)
	if err != nil || res.Lease.State != "queued" {
		t.Fatalf("mint: state = %q, err = %v, want queued", res.Lease.State, err)
	}
	setPhase(t, e, "idle_clean")
	return r
}

// holdsLocks runs path while it sits at point with its locks held, starts m, and checks that m
// waits for the path to commit.
func holdsLocks(t *testing.T, e *env, point string, m mutation, path func() error) error {
	t.Helper()
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.svc.hook = func(p string) {
		if p == point {
			once.Do(func() { close(reached); <-release })
		}
	}
	pathDone := make(chan error, 1)
	go func() { pathDone <- path() }()
	select {
	case <-reached:
	case <-time.After(3 * time.Second):
		t.Fatalf("path never reached %q", point)
	}
	mDone := make(chan error, 1)
	go func() { mDone <- m.apply(context.Background(), e) }()
	select {
	case err := <-mDone:
		t.Fatalf("%s committed while the path held its locks at %q (err %v)", m.name, point, err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	err := <-pathDone
	if mErr := <-mDone; mErr != nil {
		t.Fatalf("%s: %v", m.name, mErr)
	}
	return err
}

func requireNotEligibleNow(t *testing.T, e *env, r MintRequest, wantTokens int) {
	t.Helper()
	res, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, leaseRow(t, e).ID)
	if !errors.Is(err, ErrNotEligible) || res.Token != "" {
		t.Fatalf("renew after the change: token = %q, err = %v, want ErrNotEligible and no token", res.Token, err)
	}
	if l := leaseRow(t, e); l.State != "ended" || l.EndReason.String != endNotEligible {
		t.Fatalf("lease = %s/%q, want ended/%s", l.State, l.EndReason.String, endNotEligible)
	}
	if n := len(e.signer.claims); n != wantTokens {
		t.Fatalf("%d tokens signed, want %d", n, wantTokens)
	}
}

// TestConcurrencyPairs: each of grant, initial signing and renewal against each change to what E
// reads. In one order the path holds its locks and the change waits (the path commits with E true);
// in the other the change commits first and the path refuses.
func TestConcurrencyPairs(t *testing.T) {
	ctx := context.Background()
	for _, m := range mutations {
		t.Run("grant/"+m.name, func(t *testing.T) {
			t.Run("path holds locks", func(t *testing.T) {
				e := newEnv(t, "c00-1")
				r := queuedS2(t, e)
				err := holdsLocks(t, e, "grant:locked", m, func() error { return e.svc.AdvanceQueue(ctx, e.ws, ClassS2) })
				if err != nil {
					t.Fatal(err)
				}
				if l := leaseRow(t, e); l.State != "active" || len(e.signer.claims) != 0 {
					t.Fatalf("lease = %s, %d tokens, want active and none", l.State, len(e.signer.claims))
				}
				requireNotEligibleNow(t, e, r, 0)
			})
			t.Run("change first", func(t *testing.T) {
				e := newEnv(t, "c00-1")
				queuedS2(t, e)
				e.svc.hook = func(p string) {
					if p == "grant:candidates" {
						if err := m.apply(ctx, e); err != nil {
							t.Error(err)
						}
					}
				}
				if err := e.svc.AdvanceQueue(ctx, e.ws, ClassS2); err != nil {
					t.Fatal(err)
				}
				if l := leaseRow(t, e); l.State != "queued" || instanceEpoch(t, e, "c00-1") != 0 {
					t.Fatalf("lease = %s, epoch = %d: granted on an instance that fails E", l.State, instanceEpoch(t, e, "c00-1"))
				}
			})
		})
		for _, path := range []string{"initial signing", "renewal"} {
			path := path
			t.Run(path+"/"+m.name, func(t *testing.T) {
				run := func(e *env, r MintRequest) (Result, error) {
					if path == "initial signing" {
						return e.svc.Mint(ctx, r)
					}
					return e.svc.Renew(ctx, e.ws, r.TaskID, leaseRow(t, e).ID)
				}
				prior := 0
				prepare := func(e *env) MintRequest {
					r := e.req(e.profile(ClassS2))
					if path == "renewal" {
						if _, err := e.svc.Mint(ctx, r); err != nil {
							t.Fatal(err)
						}
						prior = 1
					}
					return r
				}
				t.Run("path holds locks", func(t *testing.T) {
					e := newEnv(t, "c00-1")
					r := prepare(e)
					var res Result
					err := holdsLocks(t, e, "sign:locked", m, func() (err error) { res, err = run(e, r); return })
					if err != nil || res.Token == "" {
						t.Fatalf("token = %q, err = %v, want a token: E held when the path committed", res.Token, err)
					}
					requireNotEligibleNow(t, e, r, prior+1)
				})
				t.Run("change first", func(t *testing.T) {
					e := newEnv(t, "c00-1")
					r := prepare(e)
					e.svc.hook = func(p string) {
						if p == "sign:begin" {
							if err := m.apply(ctx, e); err != nil {
								t.Error(err)
							}
						}
					}
					res, err := run(e, r)
					if !errors.Is(err, ErrNotEligible) || res.Token != "" || len(e.signer.claims) != prior {
						t.Fatalf("token = %q, err = %v, %d signed, want ErrNotEligible, no token, %d signed", res.Token, err, len(e.signer.claims), prior)
					}
					if l := leaseRow(t, e); l.State != "ended" || l.EndReason.String != endNotEligible {
						t.Fatalf("lease = %s/%q, want ended/%s", l.State, l.EndReason.String, endNotEligible)
					}
				})
			})
		}
	}
}

func isDeadlock(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "40P01"
}

// activeAndQueued leaves an active S2 lease with a token and a second S2 lease waiting behind it.
func activeAndQueued(t *testing.T, e *env) (first, second MintRequest) {
	t.Helper()
	first = e.req(e.profile(ClassS2))
	if res, err := e.svc.Mint(context.Background(), first); err != nil || res.Token == "" {
		t.Fatalf("first mint: %v", err)
	}
	second = e.req(e.profile(ClassS2))
	if res, err := e.svc.Mint(context.Background(), second); err != nil || res.Lease.State != "queued" {
		t.Fatalf("second mint: state = %q, err = %v", res.Lease.State, err)
	}
	return first, second
}

func leaseOf(t *testing.T, e *env, r MintRequest) db.BrowserLease {
	t.Helper()
	var id pgtype.UUID
	if err := testPool.QueryRow(context.Background(), `SELECT id FROM browser_lease WHERE workspace_id = $1 AND task_id = $2`, e.ws, r.TaskID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	l, err := e.svc.Queries.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: id, WorkspaceID: e.ws})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestEndAndRevokeAgainstAckAndGrant: {end, revoke} × {cleanup-ack, grant}, concurrent, repeated.
// No deadlock; the waiting lease ends up on the instance at the new epoch, the first lease acked.
func TestEndAndRevokeAgainstAckAndGrant(t *testing.T) {
	ctx := context.Background()
	ends := map[string]func(e *env, r MintRequest, l db.BrowserLease) error{
		"end": func(e *env, r MintRequest, l db.BrowserLease) error { return e.svc.Release(ctx, e.ws, r.TaskID, l.ID) },
		"revoke": func(e *env, r MintRequest, l db.BrowserLease) error {
			_, err := (&Revoker{Queries: e.svc.Queries, Bus: events.New()}).Revoke(ctx, Filter{WorkspaceID: e.ws, LeaseID: l.ID}, "test_revoke")
			return err
		},
	}
	follows := map[string]func(e *env, l db.BrowserLease) error{
		"cleanup-ack": func(e *env, l db.BrowserLease) error {
			_, err := (&AckService{pool: testPool, Queue: e.svc}).CleanupAck(ctx, e.ws, l.InstanceID, l.Epoch.Int64)
			if errors.Is(err, ErrLeaseActive) {
				return nil // the ack came before the end; the settle step acks again
			}
			return err
		},
		"grant": func(e *env, l db.BrowserLease) error { return e.svc.AdvanceQueue(ctx, e.ws, ClassS2) },
	}
	for en, end := range ends {
		for fn, follow := range follows {
			t.Run(en+"×"+fn, func(t *testing.T) {
				for round := 0; round < 15; round++ {
					e := newEnv(t, "c00-1")
					first, second := activeAndQueued(t, e)
					l1 := leaseOf(t, e, first)
					errs := make(chan error, 2)
					start := make(chan struct{})
					go func() { <-start; errs <- end(e, first, l1) }()
					go func() { <-start; errs <- follow(e, l1) }()
					close(start)
					for i := 0; i < 2; i++ {
						if err := <-errs; err != nil {
							t.Fatalf("round %d: %v (deadlock = %v)", round, err, isDeadlock(err))
						}
					}
					// Settle: the ack that lands after the end releases the instance to the waiter.
					if !leaseOf(t, e, first).CleanupAckedAt.Valid {
						if _, err := (&AckService{pool: testPool, Queue: e.svc}).CleanupAck(ctx, e.ws, l1.InstanceID, l1.Epoch.Int64); err != nil {
							t.Fatalf("round %d settle: %v", round, err)
						}
					}
					got1, got2 := leaseOf(t, e, first), leaseOf(t, e, second)
					if got1.State != "ended" || !got1.CleanupAckedAt.Valid {
						t.Fatalf("round %d: first lease %s, acked = %v", round, got1.State, got1.CleanupAckedAt.Valid)
					}
					if got2.State != "active" || got2.Epoch.Int64 != instanceEpoch(t, e, "c00-1") || got2.Epoch.Int64 != l1.Epoch.Int64+1 {
						t.Fatalf("round %d: second lease %s at epoch %d, instance epoch %d", round, got2.State, got2.Epoch.Int64, instanceEpoch(t, e, "c00-1"))
					}
				}
			})
		}
	}
}

// timedSigner records when each token was signed.
type timedSigner struct {
	fakeSigner
	at []time.Time
}

func (s *timedSigner) Sign(c Claims) (string, error) {
	tok, err := s.fakeSigner.Sign(c)
	s.mu.Lock()
	s.at = append(s.at, time.Now())
	s.mu.Unlock()
	return tok, err
}

// TestLockOrderStress200: grant, initial signing, renewal, end, revoke, cleanup-ack and a
// credential revocation race for 200 rounds. No deadlock (SQLSTATE 40P01), and no token is signed
// once the credential revocation has committed: it cannot commit while a signing transaction holds
// the credential row, so a later token would mean E was not checked under the lock.
func TestLockOrderStress200(t *testing.T) {
	ctx := context.Background()
	for round := 0; round < 200; round++ {
		e := newEnv(t, "c00-1")
		sg := &timedSigner{}
		e.svc.Signer = sg
		first, second := activeAndQueued(t, e)
		l1 := leaseOf(t, e, first)
		l2 := leaseOf(t, e, second)
		rng := rand.New(rand.NewSource(int64(round)))

		var mu sync.Mutex
		var errs []error
		var revokedAt time.Time
		record := func(err error) {
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}
		delays := make([]time.Duration, 8)
		for i := range delays {
			delays[i] = time.Duration(rng.Intn(3000)) * time.Microsecond
		}
		ack := &AckService{pool: testPool, Queue: e.svc}
		revoker := &Revoker{Queries: e.svc.Queries, Bus: events.New()}
		steps := []func(){
			func() { _, err := e.svc.Renew(ctx, e.ws, first.TaskID, l1.ID); record(err) },
			func() { _, err := e.svc.Renew(ctx, e.ws, second.TaskID, l2.ID); record(err) },
			func() { _, err := e.svc.Renew(ctx, e.ws, second.TaskID, l2.ID); record(err) },
			func() {
				if rng.Intn(2) == 0 {
					record(e.svc.Release(ctx, e.ws, first.TaskID, l1.ID))
				} else {
					_, err := revoker.Revoke(ctx, Filter{WorkspaceID: e.ws, LeaseID: l1.ID}, "stress")
					record(err)
				}
			},
			func() { _, err := ack.CleanupAck(ctx, e.ws, l1.InstanceID, l1.Epoch.Int64); record(err) },
			func() { record(e.svc.AdvanceQueue(ctx, e.ws, ClassS2)) },
			func() {
				err := revokeCredential(ctx, e)
				mu.Lock()
				revokedAt = time.Now()
				mu.Unlock()
				record(err)
			},
		}
		var wg sync.WaitGroup
		for i, step := range steps {
			wg.Add(1)
			go func(i int, step func()) {
				defer wg.Done()
				time.Sleep(delays[i])
				step()
			}(i, step)
		}
		wg.Wait()

		for _, err := range errs {
			if isDeadlock(err) {
				t.Fatalf("round %d: deadlock: %v", round, err)
			}
		}
		sg.mu.Lock()
		for i, at := range sg.at {
			if !revokedAt.IsZero() && at.After(revokedAt) {
				t.Fatalf("round %d: token %d (lease %s) signed %v after the credential revocation committed", round, i, sg.claims[i].LeaseID, at.Sub(revokedAt))
			}
		}
		sg.mu.Unlock()
	}
}
