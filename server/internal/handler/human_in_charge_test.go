package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type hicFixture struct {
	creator, requester, lead, owner pgtype.UUID
	members                         map[pgtype.UUID]bool
	invokable                       map[pgtype.UUID]bool
	memberErr                       error
	invokeErr                       error
	ownerErr                        error
	calls                           []string
}

func newHICFixture() *hicFixture {
	f := &hicFixture{
		creator:   parseUUID(uuid.NewString()),
		requester: parseUUID(uuid.NewString()),
		lead:      parseUUID(uuid.NewString()),
		owner:     parseUUID(uuid.NewString()),
		members:   map[pgtype.UUID]bool{},
		invokable: map[pgtype.UUID]bool{},
	}
	for _, id := range []pgtype.UUID{f.creator, f.requester, f.lead, f.owner} {
		f.members[id] = true
		f.invokable[id] = true
	}
	return f
}

func (f *hicFixture) deps() humanInChargeDeps {
	return humanInChargeDeps{
		isMember: func(_ context.Context, id pgtype.UUID) (bool, error) {
			f.calls = append(f.calls, "member:"+uuidToString(id))
			return f.members[id], f.memberErr
		},
		canInvoke: func(_ context.Context, id pgtype.UUID) (bool, error) {
			f.calls = append(f.calls, "invoke:"+uuidToString(id))
			return f.invokable[id], f.invokeErr
		},
		workspaceOwner: func(context.Context) (pgtype.UUID, error) {
			return f.owner, f.ownerErr
		},
	}
}

func (f *hicFixture) issue() db.Issue {
	return db.Issue{CreatorType: "member", CreatorID: f.creator, RequestedByID: f.requester}
}

func (f *hicFixture) project() db.Project {
	return db.Project{LeadType: pgtype.Text{String: "member", Valid: true}, LeadID: f.lead}
}

func TestHumanInChargeOrder(t *testing.T) {
	f := newHICFixture()
	got, err := resolveHumanInCharge(context.Background(), f.issue(), f.project(), f.deps())
	if err != nil || got != f.creator {
		t.Fatalf("got %v, %v; want creator", got, err)
	}
	delete(f.members, f.creator)
	if got, err = resolveHumanInCharge(context.Background(), f.issue(), f.project(), f.deps()); err != nil || got != f.requester {
		t.Fatalf("got %v, %v; want requester", got, err)
	}
	delete(f.members, f.requester)
	if got, err = resolveHumanInCharge(context.Background(), f.issue(), f.project(), f.deps()); err != nil || got != f.lead {
		t.Fatalf("got %v, %v; want project lead", got, err)
	}
	delete(f.members, f.lead)
	if got, err = resolveHumanInCharge(context.Background(), f.issue(), f.project(), f.deps()); err != nil || got != f.owner {
		t.Fatalf("got %v, %v; want workspace owner", got, err)
	}
}

func TestHumanInChargeSkipsIneligibleCandidate(t *testing.T) {
	f := newHICFixture()
	f.invokable[f.creator] = false
	got, err := resolveHumanInCharge(context.Background(), f.issue(), f.project(), f.deps())
	if err != nil || got != f.requester {
		t.Fatalf("got %v, %v; want requester after creator fails canInvoke", got, err)
	}
}

func TestHumanInChargeNoneWhenNobodyEligible(t *testing.T) {
	f := newHICFixture()
	for id := range f.invokable {
		f.invokable[id] = false
	}
	got, err := resolveHumanInCharge(context.Background(), f.issue(), f.project(), f.deps())
	if err != nil || got.Valid {
		t.Fatalf("got %v, %v; want none", got, err)
	}
}

func TestHumanInChargeNeverAgentOrSquad(t *testing.T) {
	f := newHICFixture()
	issue := f.issue()
	issue.CreatorType = "agent"
	project := f.project()
	project.LeadType = pgtype.Text{String: "squad", Valid: true}
	delete(f.members, f.requester)
	got, err := resolveHumanInCharge(context.Background(), issue, project, f.deps())
	if err != nil || got != f.owner {
		t.Fatalf("got %v, %v; want owner (agent creator and squad lead skipped)", got, err)
	}
	for _, c := range f.calls {
		if c == "member:"+uuidToString(f.creator) || c == "member:"+uuidToString(f.lead) {
			t.Fatalf("looked up a non-member candidate: %s", c)
		}
	}
}

func TestHumanInChargeNullRequestedBySkipsStep(t *testing.T) {
	f := newHICFixture()
	f.invokable[f.creator] = false
	issue := f.issue()
	issue.RequestedByID = pgtype.UUID{}
	got, err := resolveHumanInCharge(context.Background(), issue, f.project(), f.deps())
	if err != nil || got != f.lead {
		t.Fatalf("got %v, %v; want project lead", got, err)
	}
	for _, c := range f.calls {
		if c == "member:00000000-0000-0000-0000-000000000000" {
			t.Fatalf("looked up the null requester")
		}
	}
}

func TestHumanInChargeLookupErrorFailsClosed(t *testing.T) {
	boom := errors.New("db down")
	cases := map[string]func(*hicFixture){
		"membership": func(f *hicFixture) { f.memberErr = boom },
		"invoke":     func(f *hicFixture) { f.invokeErr = boom },
		"owner": func(f *hicFixture) {
			for _, id := range []pgtype.UUID{f.creator, f.requester, f.lead} {
				delete(f.members, id)
			}
			f.ownerErr = boom
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newHICFixture()
			mutate(f)
			got, err := resolveHumanInCharge(context.Background(), f.issue(), f.project(), f.deps())
			if !errors.Is(err, boom) || got.Valid {
				t.Fatalf("got %v, %v; want error and no candidate", got, err)
			}
		})
	}
}

func TestHumanInChargeNoOwnerIsNone(t *testing.T) {
	f := newHICFixture()
	for _, id := range []pgtype.UUID{f.creator, f.requester, f.lead} {
		delete(f.members, id)
	}
	f.ownerErr = pgx.ErrNoRows
	got, err := resolveHumanInCharge(context.Background(), f.issue(), f.project(), f.deps())
	if err != nil || got.Valid {
		t.Fatalf("got %v, %v; want none", got, err)
	}
}
