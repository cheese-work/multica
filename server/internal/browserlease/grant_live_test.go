package browserlease

import (
	"context"
	"errors"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// liveEnv runs the production composition (NewDecisions) over real decision rows: the answer
// is a decision_request plus decision_answer, the issue is a real issue row.
type liveEnv struct {
	*env
	f    *decisionFx
	req  MintRequest
	prof db.BrowserProfile
}

func newLiveEnv(t *testing.T, mut func(*decisionFx)) *liveEnv {
	t.Helper()
	e := newEnv(t, "c00-1", "c00-2", "c00-3")
	e.svc.Decisions = NewDecisions(e.svc.Queries)
	e.svc.Decisions = withClock(e.svc.Decisions, func() time.Time { return e.now })
	e.svc.Gate = allowAll{}
	prof := e.profile(ClassS2)
	f := newDecisionFx(t, e.ws)
	f.now, f.effAt, f.subj.Profile, f.dur = e.now, e.now.Add(-time.Minute), prof, GrantMaxAge
	if mut != nil {
		mut(f)
	}
	e.insertIssueWithID(f.subj.IssueID, "in_progress")
	f.insert(OnceScopeDecision(f.scope()), ApproveLabel, nil)
	return &liveEnv{env: e, f: f, prof: prof, req: MintRequest{
		WorkspaceID: e.ws, TaskID: f.subj.TaskID, AgentID: f.subj.AgentID, IssueID: f.subj.IssueID,
		UserID: f.member, ProfileID: prof.ID, DecisionID: f.request,
	}}
}

func (l *liveEnv) next() MintRequest {
	r := l.req
	r.TaskID = newUUID()
	return r
}

func (l *liveEnv) mint(r MintRequest) (Result, error) { return l.svc.Mint(context.Background(), r) }

func (l *liveEnv) status(s string) {
	l.t.Helper()
	l.f.exec(`UPDATE issue SET status = $2 WHERE id = $1`, l.req.IssueID, s)
}

func TestLiveOnceGrantCoversEveryLeaseOfTheProfileOnTheIssue(t *testing.T) {
	l := newLiveEnv(t, nil)
	a, err := l.mint(l.req)
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	b, err := l.mint(l.next())
	if err != nil {
		t.Fatalf("second task, same issue, agent and profile: %v", err)
	}
	if a.Lease.ID == b.Lease.ID || a.Lease.State != "active" || b.Lease.State != "active" {
		t.Fatalf("want two active leases from one decision, got %v/%s and %v/%s", a.Lease.ID, a.Lease.State, b.Lease.ID, b.Lease.State)
	}
	if want := l.now.Add(MaxLeaseDuration); !b.Lease.ExpiresAt.Time.Equal(want) {
		t.Fatalf("lease ends %v, want the 2 h limit %v", b.Lease.ExpiresAt.Time, want)
	}
}

func TestLiveOnceGrantDoesNotCoverAnotherAgentIssueOrProfile(t *testing.T) {
	l := newLiveEnv(t, nil)
	cases := map[string]func(*MintRequest){
		"agent":   func(r *MintRequest) { r.AgentID = newUUID() },
		"issue":   func(r *MintRequest) { r.IssueID = newUUID() },
		"profile": func(r *MintRequest) { r.ProfileID = l.profile(ClassS2).ID },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := l.next()
			mutate(&r)
			if _, err := l.mint(r); !errors.Is(err, ErrDecisionInvalid) {
				t.Fatalf("err = %v, want ErrDecisionInvalid", err)
			}
		})
	}
}

func TestLiveOnceGrantEndsWhenTheIssueIsDoneOrClosed(t *testing.T) {
	for _, status := range []string{"done", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			l := newLiveEnv(t, nil)
			res, err := l.mint(l.req)
			if err != nil {
				t.Fatal(err)
			}
			l.status(status)
			if _, err := l.mint(l.next()); !errors.Is(err, ErrDecisionInvalid) {
				t.Fatalf("mint after %s: %v, want ErrDecisionInvalid", status, err)
			}
			if _, err := l.svc.Renew(context.Background(), l.ws, l.req.TaskID, res.Lease.ID); !errors.Is(err, ErrDecisionInvalid) {
				t.Fatalf("renew after %s: %v, want ErrDecisionInvalid", status, err)
			}
		})
	}
}

func TestLiveOnceGrantEndsAfterSevenDays(t *testing.T) {
	l := newLiveEnv(t, func(f *decisionFx) { f.dur = 30 * 24 * time.Hour })
	l.now = l.f.effAt.Add(GrantMaxAge - 30*time.Minute)
	res, err := l.mint(l.next())
	if err != nil {
		t.Fatal(err)
	}
	if want := l.f.effAt.Add(GrantMaxAge); !res.Lease.ExpiresAt.Time.Equal(want) {
		t.Fatalf("lease ends %v, want the 7-day edge %v", res.Lease.ExpiresAt.Time, want)
	}
	l.now = l.f.effAt.Add(GrantMaxAge)
	if _, err := l.mint(l.next()); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("mint at day 7: %v, want ErrDecisionInvalid", err)
	}
}

func TestLiveOnceGrantEndsWithTheDecisionDuration(t *testing.T) {
	l := newLiveEnv(t, func(f *decisionFx) { f.dur = 40 * time.Minute })
	res, err := l.mint(l.req)
	if err != nil {
		t.Fatal(err)
	}
	if want := l.f.effAt.Add(40 * time.Minute); !res.Lease.ExpiresAt.Time.Equal(want) {
		t.Fatalf("lease ends %v, want the decision's end %v", res.Lease.ExpiresAt.Time, want)
	}
	l.now = l.f.effAt.Add(40 * time.Minute)
	if _, err := l.mint(l.next()); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("mint after the decision duration: %v, want ErrDecisionInvalid", err)
	}
}

func TestLiveOnceGrantIsRevalidatedAtMintAndRenewWhenWithdrawn(t *testing.T) {
	l := newLiveEnv(t, nil)
	res, err := l.mint(l.req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.svc.Renew(context.Background(), l.ws, l.req.TaskID, res.Lease.ID); err != nil {
		t.Fatalf("renew before withdrawal: %v", err)
	}
	l.f.exec(`UPDATE decision_request SET status = 'cancelled', cancel_reason = 'agent_withdrawn', withdrawn_at = now() WHERE id = $1`, l.f.request)
	if _, err := l.svc.Renew(context.Background(), l.ws, l.req.TaskID, res.Lease.ID); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("renew after withdrawal: %v, want ErrDecisionInvalid", err)
	}
	if _, err := l.mint(l.next()); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("mint after withdrawal: %v, want ErrDecisionInvalid", err)
	}
}

func TestLiveOnceGrantNeedsAnApprovingAnswerFromAnAllowedAnswerer(t *testing.T) {
	cases := map[string]struct {
		want error
		fix  func(l *liveEnv)
	}{
		"unanswered": {ErrDecisionRequired, func(l *liveEnv) {
			l.f.exec(`DELETE FROM decision_answer WHERE decision_request_id = $1`, l.f.request)
			l.f.exec(`UPDATE decision_request SET status = 'open' WHERE id = $1`, l.f.request)
		}},
		"denied": {ErrDecisionInvalid, func(l *liveEnv) {
			l.f.exec(`UPDATE decision_answer SET chosen_option_index = 1, content_snapshot = 'Deny' WHERE decision_request_id = $1`, l.f.request)
		}},
		"answered by another member": {ErrDecisionInvalid, func(l *liveEnv) {
			l.f.exec(`UPDATE decision_answer SET answered_by_user_id = $2 WHERE decision_request_id = $1`, l.f.request, l.f.stranger)
		}},
		"agent authority": {ErrDecisionInvalid, func(l *liveEnv) {
			l.f.exec(`UPDATE decision_request SET authority = 'agents' WHERE id = $1`, l.f.request)
		}},
		"effective later": {ErrDecisionInvalid, func(l *liveEnv) {
			l.f.exec(`UPDATE decision_answer SET effective_at = $2 WHERE decision_request_id = $1`, l.f.request, l.now.Add(time.Hour))
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLiveEnv(t, nil)
			c.fix(l)
			if _, err := l.mint(l.req); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// A decision that is not a once-per-issue grant keeps the per-lease rule (CHE-4740).
func TestLivePerLeaseDecisionStillValidatesPerLease(t *testing.T) {
	l := newLiveEnv(t, func(f *decisionFx) { f.dur = time.Hour })
	l.f.exec(`UPDATE decision_request SET ask_json = jsonb_build_object('decision', $2::text) WHERE id = $1`, l.f.request, ScopeDecision(l.f.scope()))
	if _, err := l.mint(l.req); err != nil {
		t.Fatalf("per-lease decision: %v", err)
	}
}

func withClock(d Decisions, now func() time.Time) Decisions {
	if o, ok := d.(OncePerIssue); ok {
		o.Now = now
		if v, ok := o.Next.(*DecisionValidator); ok {
			v.Now = now
		}
		if g, ok := o.Grants.(DBGrants); ok {
			g.Now = now
			o.Grants = g
		}
		return o
	}
	return d
}
