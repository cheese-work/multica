package browserlease

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// decisionFx is one answered lease decision on a fresh workspace: an owner (Cheese) who
// answered, an accountable member, and a request whose scope names the subject.
type decisionFx struct {
	t        *testing.T
	now      time.Time
	ws       pgtype.UUID
	owner    pgtype.UUID
	member   pgtype.UUID
	stranger pgtype.UUID
	subj     Subject
	request  pgtype.UUID
	answer   pgtype.UUID
	effAt    time.Time
	dur      time.Duration
}

func uid() pgtype.UUID { return newUUID() }

func newDecisionFx(t *testing.T, ws ...pgtype.UUID) *decisionFx {
	t.Helper()
	f := &decisionFx{
		t: t, now: time.Now().UTC().Truncate(time.Microsecond), ws: uid(), owner: uid(), member: uid(), stranger: uid(),
		request: uid(), answer: uid(), dur: time.Hour,
	}
	if len(ws) > 0 {
		f.ws = ws[0]
	}
	f.effAt = f.now.Add(-time.Minute)
	f.subj = Subject{
		WorkspaceID: f.ws, UserID: f.member, AgentID: uid(), IssueID: uid(), TaskID: uid(), DecisionID: f.request,
		Profile: db.BrowserProfile{ID: uid(), WorkspaceID: f.ws, Name: "figma", Class: ClassS2},
	}
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM decision_answer WHERE decision_request_id = $1`, f.request)
		testPool.Exec(ctx, `DELETE FROM decision_request WHERE workspace_id = $1`, f.ws)
		testPool.Exec(ctx, `DELETE FROM member WHERE workspace_id = $1`, f.ws)
		testPool.Exec(ctx, `DELETE FROM workspace WHERE id = $1`, f.ws)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = ANY($1)`, []pgtype.UUID{f.owner, f.member, f.stranger})
	})
	f.exec(`INSERT INTO workspace (id, name, slug) VALUES ($1, 'lease decision', $2) ON CONFLICT (id) DO NOTHING`, f.ws, "ld-"+uuid.NewString())
	for u, role := range map[pgtype.UUID]string{f.owner: "owner", f.member: "member", f.stranger: "member"} {
		f.exec(`INSERT INTO "user" (id, name, email) VALUES ($1, 'u', $2)`, u, uuid.NewString()+"@example.test")
		f.exec(`INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, $3)`, f.ws, u, role)
	}
	return f
}

func (f *decisionFx) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := testPool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *decisionFx) scope() Scope {
	return Scope{AgentID: f.subj.AgentID, IssueID: f.subj.IssueID, ProfileID: f.subj.Profile.ID, Class: ClassS2, Duration: f.dur}
}

// insert writes the request and, when answer is non-empty, its live answer.
func (f *decisionFx) insert(decision, answer string, mut func(*reqRow, *ansRow)) {
	f.t.Helper()
	opts, _ := json.Marshal([]map[string]any{{"label": ApproveLabel}, {"label": "Deny"}})
	askJSON, _ := json.Marshal(map[string]string{"decision": decision})
	r := &reqRow{status: "open", authority: "human", human: f.owner, agent: f.subj.AgentID, issue: f.subj.IssueID}
	a := &ansRow{text: answer, effectiveAt: f.effAt, by: f.owner, chosen: -1}
	if answer == ApproveLabel {
		a.chosen = 0
	}
	if answer != "" {
		r.status = "answered"
	}
	if mut != nil {
		mut(r, a)
	}
	f.exec(`INSERT INTO decision_request (id, workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, ask, options, ask_json,
		escalation_class, origin, status, authority, cancel_reason, expires_at, withdrawn_at)
		VALUES ($1,$2,$3,$4,$5,$6,'lease?',$7,$8,'reversible','asked',$9,$10,$11,$12,$13)`,
		f.request, f.ws, r.issue, r.agent, r.human, uid(), opts, askJSON, r.status, r.authority, r.cancel, r.expiresAt, r.withdrawnAt)
	if answer == "" {
		return
	}
	f.exec(`INSERT INTO decision_answer (id, decision_request_id, content_snapshot, chat_message_ids, effective_at, answered_by_user_id,
		chosen_option_index, reply_text, disclaimed_at, answer_source)
		VALUES ($1,$2,$3,'{}',$4,$5,$6,$7,$8,'card')`,
		f.answer, f.request, a.text, a.effectiveAt, a.by, a.chosenArg(), a.reply, a.disclaimedAt)
}

type reqRow struct {
	status, authority   string
	human, agent, issue pgtype.UUID
	cancel              *string
	expiresAt           *time.Time
	withdrawnAt         *time.Time
}

type ansRow struct {
	text         string
	effectiveAt  time.Time
	by           pgtype.UUID
	chosen       int
	reply        *string
	disclaimedAt *time.Time
}

func (a *ansRow) chosenArg() any {
	if a.chosen < 0 {
		return nil
	}
	return a.chosen
}

func (f *decisionFx) validator() *DecisionValidator {
	return &DecisionValidator{Queries: db.New(testPool), Now: func() time.Time { return f.now }}
}

func (f *decisionFx) validate() (*time.Time, error) {
	return f.validator().Validate(context.Background(), f.subj)
}

func ptr[T any](v T) *T { return &v }

func TestValidateAcceptsOwnerApproval(t *testing.T) {
	f := newDecisionFx(t)
	f.insert(ScopeDecision(f.scope()), ApproveLabel, nil)
	exp, err := f.validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if want := f.effAt.Add(f.dur); exp == nil || !exp.Equal(want) {
		t.Fatalf("expiry = %v, want effective time + duration %v", exp, want)
	}
}

func TestValidateAcceptsAccountableUserForS2(t *testing.T) {
	f := newDecisionFx(t)
	f.insert(ScopeDecision(f.scope()), ApproveLabel, func(_ *reqRow, a *ansRow) { a.by = f.member })
	if _, err := f.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidateRejections(t *testing.T) {
	other := uid()
	cases := []struct {
		name     string
		want     error
		noID     bool
		decision func(f *decisionFx) string // nil: the matching scope
		answer   string
		mut      func(f *decisionFx, r *reqRow, a *ansRow)
		subject  func(s *Subject)
	}{
		{name: "no decision named", want: ErrDecisionRequired, noID: true},
		{name: "request unanswered", want: ErrDecisionRequired, answer: ""},
		{name: "request not found", want: ErrDecisionInvalid, answer: ApproveLabel, subject: func(s *Subject) { s.DecisionID = uid() }},
		{name: "request on another issue", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, _ *ansRow) { r.issue = other }},
		{name: "request asked by another agent", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, _ *ansRow) { r.agent = other }},
		{name: "agent authority, not a human", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, _ *ansRow) { r.authority = "agents" }},
		{name: "denied: another option", want: ErrDecisionInvalid, answer: "Deny", mut: func(f *decisionFx, r *reqRow, a *ansRow) { a.chosen = 1 }},
		{name: "denied: chosen index is the deny option", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, a *ansRow) { a.chosen = 1 }},
		{name: "approval with a typed note", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, a *ansRow) { a.reply = ptr("only until noon") }},
		{name: "withdrawn by the agent", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, _ *ansRow) {
				r.status, r.cancel, r.withdrawnAt = "cancelled", ptr("agent_withdrawn"), ptr(f.now.Add(-time.Second))
			}},
		{name: "expired request", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, _ *ansRow) { r.status, r.cancel = "cancelled", ptr("expired") }},
		{name: "request past its expires_at", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, _ *ansRow) { r.expiresAt = ptr(f.now.Add(-time.Second)) }},
		{name: "answer disclaimed", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, a *ansRow) {
				r.status, a.disclaimedAt = "disclaimed", ptr(f.now.Add(-time.Second))
			}},
		{name: "held: effective time not reached", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, a *ansRow) { a.effectiveAt = f.now.Add(10 * time.Minute) }},
		{name: "answer older than its duration", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, a *ansRow) { a.effectiveAt = f.now.Add(-2 * time.Hour) }},
		{name: "answerer is another member", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, a *ansRow) { a.by = f.stranger }},
		{name: "answerer is not a workspace member", want: ErrDecisionInvalid, answer: ApproveLabel,
			mut: func(f *decisionFx, r *reqRow, a *ansRow) { a.by = other }},
		{name: "scope names another agent", want: ErrDecisionInvalid, answer: ApproveLabel,
			decision: func(f *decisionFx) string { s := f.scope(); s.AgentID = other; return ScopeDecision(s) }},
		{name: "scope names another issue", want: ErrDecisionInvalid, answer: ApproveLabel,
			decision: func(f *decisionFx) string { s := f.scope(); s.IssueID = other; return ScopeDecision(s) }},
		{name: "scope names another profile", want: ErrDecisionInvalid, answer: ApproveLabel,
			decision: func(f *decisionFx) string { s := f.scope(); s.ProfileID = other; return ScopeDecision(s) }},
		{name: "scope names another class", want: ErrDecisionInvalid, answer: ApproveLabel,
			decision: func(f *decisionFx) string { s := f.scope(); s.Class = ClassS0; return ScopeDecision(s) }},
		{name: "scope duration over the hard limit", want: ErrDecisionInvalid, answer: ApproveLabel,
			decision: func(f *decisionFx) string {
				s := f.scope()
				s.Duration = MaxLeaseDuration + time.Second
				return ScopeDecision(s)
			}},
		{name: "scope duration zero", want: ErrDecisionInvalid, answer: ApproveLabel,
			decision: func(f *decisionFx) string { s := f.scope(); s.Duration = 0; return ScopeDecision(s) }},
		{name: "ask carries no scope", want: ErrDecisionInvalid, answer: ApproveLabel,
			decision: func(f *decisionFx) string { return "Lease a browser?" }},
		{name: "ask repeats a scope line", want: ErrDecisionInvalid, answer: ApproveLabel,
			decision: func(f *decisionFx) string { return ScopeDecision(f.scope()) + "\nagent: " + uuid.NewString() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newDecisionFx(t)
			if c.subject != nil {
				c.subject(&f.subj)
			}
			if c.noID {
				f.subj.DecisionID = pgtype.UUID{}
			} else {
				decision := ScopeDecision(f.scope())
				if c.decision != nil {
					decision = c.decision(f)
				}
				var mut func(*reqRow, *ansRow)
				if c.mut != nil {
					mut = func(r *reqRow, a *ansRow) { c.mut(f, r, a) }
				}
				f.insert(decision, c.answer, mut)
			}
			exp, err := f.validate()
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, exp = %v; want %v", err, exp, c.want)
			}
		})
	}
}

// Mint, renew and the queued grant all judge through Service.check: a decision withdrawn after
// the grant ends the lease at renew, and one withdrawn while queued drops the lease at its turn.
func TestServiceRevalidatesWithdrawnDecision(t *testing.T) {
	e := newEnv(t, "c00-1")
	e.svc.Decisions = &DecisionValidator{Queries: e.svc.Queries, Now: func() time.Time { return e.now }}
	e.svc.Gate = allowAll{}
	prof := e.profile(ClassS2)
	ctx := context.Background()

	lease := func() (*decisionFx, MintRequest) {
		f := newDecisionFx(t, e.ws)
		f.now, f.effAt, f.subj.Profile = e.now, e.now.Add(-time.Minute), prof
		f.insert(ScopeDecision(f.scope()), ApproveLabel, nil)
		return f, MintRequest{WorkspaceID: e.ws, TaskID: f.subj.TaskID, AgentID: f.subj.AgentID, IssueID: f.subj.IssueID,
			UserID: f.member, ProfileID: prof.ID, DecisionID: f.request}
	}
	withdraw := func(f *decisionFx) {
		f.exec(`UPDATE decision_request SET status = 'cancelled', cancel_reason = 'agent_withdrawn', withdrawn_at = now() WHERE id = $1`, f.request)
	}

	fa, ra := lease()
	a, err := e.svc.Mint(ctx, ra)
	if err != nil || a.Lease.State != "active" {
		t.Fatalf("mint A: %v (state %q)", err, a.Lease.State)
	}
	fb, rb := lease()
	b, err := e.svc.Mint(ctx, rb)
	if err != nil || b.Lease.State != "queued" {
		t.Fatalf("mint B: %v (state %q)", err, b.Lease.State)
	}

	withdraw(fa)
	if _, err := e.svc.Renew(ctx, e.ws, ra.TaskID, a.Lease.ID); !errors.Is(err, ErrDecisionInvalid) {
		t.Fatalf("renew err = %v, want ErrDecisionInvalid", err)
	}
	if got := e.lease(a.Lease.ID); got.State != "ended" || got.EndReason.String != endRevalidation {
		t.Fatalf("lease A = %s/%s, want ended/%s", got.State, got.EndReason.String, endRevalidation)
	}

	withdraw(fb)
	e.ack(a.Lease.ID)
	if err := e.svc.AdvanceQueue(ctx, e.ws, ClassS2); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := e.svc.Queries.GetBrowserLease(ctx, db.GetBrowserLeaseParams{ID: b.Lease.ID, WorkspaceID: e.ws}); err == nil {
		t.Fatal("queued lease B kept its place after its decision was withdrawn")
	}
}

type allowAll struct{}

func (allowAll) Check(context.Context, Subject) error { return nil }
