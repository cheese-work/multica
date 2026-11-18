package browserlease

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakePerms is the W3-8 store: default deny, and a store error is an error, not a grant.
type fakePerms struct {
	users, agents map[pgtype.UUID]bool
	err           error
}

func (f *fakePerms) UserPermitted(_ context.Context, _, user, _ pgtype.UUID) (bool, error) {
	return f.users[user], f.err
}

func (f *fakePerms) AgentAllowed(_ context.Context, _, _, agent pgtype.UUID) (bool, error) {
	return f.agents[agent], f.err
}

func TestUserGateCheck(t *testing.T) {
	u, a := newUUID(), newUUID()
	both := &fakePerms{users: map[pgtype.UUID]bool{u: true}, agents: map[pgtype.UUID]bool{a: true}}
	cases := []struct {
		name  string
		perms Permissions
		user  pgtype.UUID
		agent pgtype.UUID
		want  error // nil = allowed
		not   error // a denial must not read as this
	}{
		{"both", both, u, a, nil, nil},
		{"user not permitted", both, newUUID(), a, ErrUserNotPermitted, ErrAgentNotAllowed},
		{"agent not allowlisted", both, u, newUUID(), ErrAgentNotAllowed, ErrUserNotPermitted},
		{"neither", both, newUUID(), newUUID(), ErrUserNotPermitted, nil},
		{"no user on the task", both, pgtype.UUID{}, a, ErrUserNotPermitted, nil},
		{"no store configured", nil, u, a, ErrGateDenied, nil},
		{"store error fails closed", &fakePerms{users: both.users, agents: both.agents, err: errors.New("db down")}, u, a, errors.New("db down"), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Subject{WorkspaceID: newUUID(), UserID: c.user, AgentID: c.agent, Profile: db.BrowserProfile{Class: ClassS0}}
			err := UserGate{Permissions: c.perms}.Check(context.Background(), s)
			switch {
			case c.want == nil && err != nil:
				t.Fatalf("err = %v, want allowed", err)
			case c.want == nil:
				return
			case err == nil:
				t.Fatalf("allowed, want denied (%v)", c.want)
			case c.want.Error() == "db down":
				if errors.Is(err, ErrGateDenied) || err.Error() != "db down" {
					t.Fatalf("err = %v, want the store error, not a denial", err)
				}
			case !errors.Is(err, c.want) || !errors.Is(err, ErrGateDenied):
				t.Fatalf("err = %v, want %v and ErrGateDenied", err, c.want)
			case c.not != nil && errors.Is(err, c.not):
				t.Fatalf("err = %v must not match %v", err, c.not)
			}
		})
	}
}

// The gate sits under every path that grants or keeps a lease.
func gatedEnv(t *testing.T, instances ...string) (*env, *fakePerms) {
	e := newEnv(t, instances...)
	p := &fakePerms{users: map[pgtype.UUID]bool{}, agents: map[pgtype.UUID]bool{}}
	e.svc.Gate = UserGate{Permissions: p}
	return e, p
}

func allow(p *fakePerms, r MintRequest) { p.users[r.UserID], p.agents[r.AgentID] = true, true }

func TestMintDeniedWithoutUserOrAllowlist(t *testing.T) {
	e, p := gatedEnv(t, "x99-1")
	prof := e.profile(ClassS0)
	r := e.req(prof)
	if _, err := e.svc.Mint(context.Background(), r); !errors.Is(err, ErrUserNotPermitted) {
		t.Fatalf("no permission: err = %v, want ErrUserNotPermitted", err)
	}
	p.users[r.UserID] = true
	if _, err := e.svc.Mint(context.Background(), r); !errors.Is(err, ErrAgentNotAllowed) {
		t.Fatalf("agent off allowlist: err = %v, want ErrAgentNotAllowed", err)
	}
	var n int
	testPool.QueryRow(context.Background(), `SELECT count(*) FROM browser_lease WHERE workspace_id = $1`, e.ws).Scan(&n)
	if n != 0 {
		t.Fatalf("%d lease rows after two denied mints, want 0", n)
	}
	allow(p, r)
	if res, err := e.svc.Mint(context.Background(), r); err != nil || res.Token == "" {
		t.Fatalf("permitted mint: %v token=%q", err, res.Token)
	}
}

func TestRenewRevalidatesTheGate(t *testing.T) {
	e, p := gatedEnv(t, "x99-1")
	r := e.req(e.profile(ClassS0))
	allow(p, r)
	res, err := e.svc.Mint(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	delete(p.agents, r.AgentID) // Cheese removes the agent from the allowlist
	if _, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, res.Lease.ID); !errors.Is(err, ErrAgentNotAllowed) {
		t.Fatalf("renew: err = %v, want ErrAgentNotAllowed", err)
	}
	if l := e.lease(res.Lease.ID); l.State != "ended" || l.EndReason.String != endRevalidation {
		t.Fatalf("lease = %s/%s, want ended/%s", l.State, l.EndReason.String, endRevalidation)
	}
}

func TestQueuedGrantRevalidatesTheGate(t *testing.T) {
	e, p := gatedEnv(t, "x99-1")
	prof := e.profile(ClassS0)
	first, second := e.req(prof), e.req(prof)
	allow(p, first)
	allow(p, second)
	held, err := e.svc.Mint(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := e.svc.Mint(context.Background(), second)
	if err != nil || queued.Lease.State != "queued" {
		t.Fatalf("second mint: %v state=%s, want queued", err, queued.Lease.State)
	}
	delete(p.users, second.UserID) // permission withdrawn while it waits
	if err := e.svc.Release(context.Background(), e.ws, first.TaskID, held.Lease.ID); err != nil {
		t.Fatal(err)
	}
	e.ack(held.Lease.ID)
	if err := e.svc.AdvanceQueue(context.Background(), e.ws, ClassS0); err != nil {
		t.Fatal(err)
	}
	var n int
	testPool.QueryRow(context.Background(), `SELECT count(*) FROM browser_lease WHERE id = $1`, queued.Lease.ID).Scan(&n)
	if n != 0 {
		t.Fatal("the queued lease of a user without permission was kept or granted")
	}
	var phase string
	testPool.QueryRow(context.Background(), `SELECT phase FROM browser_instance WHERE workspace_id = $1`, e.ws).Scan(&phase)
	if phase != "idle_clean" {
		t.Fatalf("instance = %s, want idle_clean (no grant)", phase)
	}
}
