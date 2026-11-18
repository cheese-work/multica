package browserlease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type fakeGrants struct {
	mu sync.Mutex
	m  map[pgtype.UUID]Grant
}

func (f *fakeGrants) Grant(_ context.Context, _, id pgtype.UUID) (Grant, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.m[id]
	return g, ok, nil
}

type fakeIssues struct {
	mu     sync.Mutex
	closed map[pgtype.UUID]bool
}

func (f *fakeIssues) Closed(_ context.Context, _, issue pgtype.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed[issue], nil
}

// grantEnv is an S2 world where decisions are once-per-issue grants. e.hooks stays the inner
// (per-lease) decision source.
type grantEnv struct {
	*env
	grants *fakeGrants
	issues *fakeIssues
	req    MintRequest
	grant  Grant
}

func newGrantEnv(t *testing.T) *grantEnv {
	t.Helper()
	e := newEnv(t, "c00-1", "c00-2", "c00-3")
	g := &grantEnv{env: e, grants: &fakeGrants{m: map[pgtype.UUID]Grant{}}, issues: &fakeIssues{closed: map[pgtype.UUID]bool{}}}
	e.svc.Decisions = OncePerIssue{Grants: g.grants, Issues: g.issues, Next: e.hooks, Now: func() time.Time { return e.now }}
	p := e.profile(ClassS2)
	g.req = e.req(p)
	g.grant = Grant{
		DecisionID: g.req.DecisionID, WorkspaceID: e.ws, IssueID: g.req.IssueID, AgentID: g.req.AgentID,
		ProfileID: p.ID, AnsweredAt: e.now,
	}
	g.grants.m[g.grant.DecisionID] = g.grant
	return g
}

// next is another task of the same agent on the same issue, under the same decision.
func (g *grantEnv) next() MintRequest {
	r := g.req
	r.TaskID = newUUID()
	return r
}

func (g *grantEnv) set(f func(*Grant)) {
	gr := g.grant
	f(&gr)
	g.grants.mu.Lock()
	g.grants.m[gr.DecisionID] = gr
	g.grants.mu.Unlock()
}

func (g *grantEnv) mint(r MintRequest) (Result, error) { return g.svc.Mint(context.Background(), r) }

func TestOnceGrantCoversEveryLeaseOfTheProfileOnTheIssue(t *testing.T) {
	g := newGrantEnv(t)
	first, err := g.mint(g.req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := g.mint(g.next())
	if err != nil {
		t.Fatalf("second task, same issue, agent and profile: %v", err)
	}
	if first.Lease.ID == second.Lease.ID || second.Token == "" {
		t.Fatalf("want two leases from one decision, got %v and %v", first.Lease.ID, second.Lease.ID)
	}
}

func TestOnceGrantDoesNotCoverAnotherAgentIssueOrProfile(t *testing.T) {
	g := newGrantEnv(t)
	cases := map[string]func(*MintRequest){
		"agent":   func(r *MintRequest) { r.AgentID = newUUID() },
		"issue":   func(r *MintRequest) { r.IssueID = newUUID() },
		"profile": func(r *MintRequest) { r.ProfileID = g.profile(ClassS2).ID },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := g.next()
			mutate(&r)
			if _, err := g.mint(r); !errors.Is(err, ErrDecisionInvalid) {
				t.Fatalf("err = %v, want ErrDecisionInvalid", err)
			}
		})
	}
}

func TestOnceGrantEndsWhenTheIssueIsClosed(t *testing.T) {
	g := newGrantEnv(t)
	res, err := g.mint(g.req)
	if err != nil {
		t.Fatal(err)
	}
	g.issues.closed[g.req.IssueID] = true
	if _, err := g.mint(g.next()); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("mint after close: %v, want ErrDecisionInvalid", err)
	}
	if _, err := g.svc.Renew(context.Background(), g.ws, g.req.TaskID, res.Lease.ID); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("renew after close: %v, want ErrDecisionInvalid", err)
	}
	l, _ := g.svc.Queries.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: res.Lease.ID, WorkspaceID: g.ws})
	if l.State != "ended" || l.EndReason.String != endRevalidation {
		t.Fatalf("lease = %s/%q, want ended/%s", l.State, l.EndReason.String, endRevalidation)
	}
}

func TestOnceGrantEndsAfterSevenDays(t *testing.T) {
	g := newGrantEnv(t)
	g.now = g.grant.AnsweredAt.Add(GrantMaxAge - 30*time.Minute)
	res, err := g.mint(g.next())
	if err != nil {
		t.Fatal(err)
	}
	if want := g.grant.AnsweredAt.Add(GrantMaxAge); !res.Lease.ExpiresAt.Time.Equal(want) {
		t.Fatalf("lease ends %v, want the 7-day edge %v, not the 2 h limit", res.Lease.ExpiresAt.Time, want)
	}
	g.now = g.grant.AnsweredAt.Add(GrantMaxAge)
	if _, err := g.mint(g.next()); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("mint at day 7: %v, want ErrDecisionInvalid", err)
	}
}

func TestOnceGrantEndsWithTheDecisionDuration(t *testing.T) {
	g := newGrantEnv(t)
	end := g.now.Add(40 * time.Minute)
	g.set(func(gr *Grant) { gr.EndsAt = &end })
	res, err := g.mint(g.req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Lease.ExpiresAt.Time.Equal(end) {
		t.Fatalf("lease ends %v, want the decision's end %v", res.Lease.ExpiresAt.Time, end)
	}
	g.now = end
	if _, err := g.mint(g.next()); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("mint after the decision duration: %v, want ErrDecisionInvalid", err)
	}
}

func TestOnceGrantLeaseStillEndsAtTwoHours(t *testing.T) {
	g := newGrantEnv(t)
	res, err := g.mint(g.req)
	if err != nil {
		t.Fatal(err)
	}
	if want := g.now.Add(MaxLeaseDuration); !res.Lease.ExpiresAt.Time.Equal(want) {
		t.Fatalf("lease ends %v, want %v", res.Lease.ExpiresAt.Time, want)
	}
}

func TestOnceGrantIsRevalidatedAtEveryRenew(t *testing.T) {
	g := newGrantEnv(t)
	res, err := g.mint(g.req)
	if err != nil {
		t.Fatal(err)
	}
	g.set(func(gr *Grant) { gr.Withdrawn = true })
	if _, err := g.svc.Renew(context.Background(), g.ws, g.req.TaskID, res.Lease.ID); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("renew of a withdrawn grant: %v, want ErrDecisionInvalid", err)
	}
}

func TestNonGrantDecisionsFallThroughToTheInnerSource(t *testing.T) {
	g := newGrantEnv(t)
	r := g.next()
	r.DecisionID = newUUID() // not a once-per-issue grant
	g.hooks.decisionErr = ErrDecisionInvalid
	if _, err := g.mint(r); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("err = %v, want the inner source's ErrDecisionInvalid", err)
	}
	g.hooks.decisionErr = nil
	if _, err := g.mint(r); err != nil {
		t.Fatalf("inner source approves: %v", err)
	}
}

func TestWithdrawingTheDecisionRevokesEveryLeaseItCovers(t *testing.T) {
	g := newGrantEnv(t)
	bus := events.New()
	var mu sync.Mutex
	var revoked []string
	bus.Subscribe(protocol.EventBrowserLeaseRevoked, func(ev events.Event) {
		mu.Lock()
		defer mu.Unlock()
		revoked = append(revoked, ev.Payload.(map[string]any)["reason"].(string))
	})
	a, err := g.mint(g.req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := g.mint(g.next())
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated S2 lease under another decision must survive.
	other := g.env.req(g.profile(ClassS2))
	o, err := g.svc.Mint(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}

	rv := &Revoker{Queries: g.svc.Queries, Bus: bus}
	got, err := rv.RevokeDecision(context.Background(), g.ws, g.grant.DecisionID)
	if err != nil || len(got) != 2 {
		t.Fatalf("revoked %d leases, err=%v; want 2", len(got), err)
	}
	for _, id := range []pgtype.UUID{a.Lease.ID, b.Lease.ID} {
		l, _ := g.svc.Queries.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: id, WorkspaceID: g.ws})
		if l.State != "ended" || l.EndReason.String != ReasonDecisionWithdrawn {
			t.Fatalf("lease %v = %s/%q, want ended/%s", id, l.State, l.EndReason.String, ReasonDecisionWithdrawn)
		}
	}
	l, _ := g.svc.Queries.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: o.Lease.ID, WorkspaceID: g.ws})
	if l.State != "active" {
		t.Fatalf("unrelated lease state = %s, want active", l.State)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(revoked) != 2 {
		t.Fatalf("revoke events = %v, want 2 with the withdrawn reason", revoked)
	}
}

func TestRevokeDecisionNeedsAWorkspaceAndADecision(t *testing.T) {
	rv := &Revoker{}
	if _, err := rv.RevokeDecision(context.Background(), pgtype.UUID{}, newUUID()); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("no workspace: %v", err)
	}
	if _, err := rv.RevokeDecision(context.Background(), newUUID(), pgtype.UUID{}); err == nil {
		t.Fatal("an empty decision id would revoke the whole workspace")
	}
}

func TestQueryIssueStateReadsStatusAndCategory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := QueryIssueState{Queries: e.svc.Queries}
	missing, err := s.Closed(ctx, e.ws, newUUID())
	if err != nil || !missing {
		t.Fatalf("missing issue: closed=%v err=%v, want closed (fail closed)", missing, err)
	}
	for status, want := range map[string]bool{"in_progress": false, "done": true, "cancelled": true} {
		issue := e.insertIssue(status)
		got, err := s.Closed(ctx, e.ws, issue)
		if err != nil || got != want {
			t.Fatalf("status %s: closed=%v err=%v, want %v", status, got, err, want)
		}
	}
}

// insertIssue writes the least an issue row needs. Soft references: no workspace row.
func (e *env) insertIssue(status string) pgtype.UUID {
	e.t.Helper()
	id := newUUID()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO workspace (id, name, slug) VALUES ($1, 'w3-19', $2) ON CONFLICT DO NOTHING`, e.ws, "w3-19-"+uuid.NewString()); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM workspace WHERE id = $1`, e.ws) })
	_, err := testPool.Exec(ctx,
		`INSERT INTO issue (id, workspace_id, title, status, creator_type, creator_id, number) VALUES ($1, $2, 'w3-19', $3, 'agent', $4, $5)`,
		id, e.ws, status, newUUID(), time.Now().UnixNano()%1_000_000_000)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id) })
	return id
}

func TestQueryIssueStateHonoursACustomDoneCategory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, tc := range []struct{ key, category string }{{"custom_accepted", "done"}, {"custom_parked", "started"}} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO issue_status (workspace_id, key, name, category, color) VALUES ($1, $2, $2, $3, '#888888')`, e.ws, tc.key, tc.category); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue_status WHERE workspace_id = $1`, e.ws) })
	s := QueryIssueState{Queries: e.svc.Queries}
	for key, want := range map[string]bool{"custom_accepted": true, "custom_parked": false} {
		got, err := s.Closed(ctx, e.ws, e.insertIssue(key))
		if err != nil || got != want {
			t.Fatalf("status %s: closed=%v err=%v, want %v", key, got, err, want)
		}
	}
}
