package ciwatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Round 2 of CHE-4278 review: sent calls are always debited, and a stored
// permission 403 always reaches the Shadow switch.

func TestRerunSentGetIsDebitedOnEveryPathThatSendsNoPost(t *testing.T) {
	cases := map[string]func(e *rerunEnv){
		"manual rerun":  func(e *rerunEnv) { e.gh.setAttempt(2) },
		"read error":    func(e *rerunEnv) { e.gh.getErr = errors.New("dial tcp: i/o timeout") },
		"run not ended": func(e *rerunEnv) { e.gh.pending = true },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newRerunEnv(t)
			e.gh.post = accepted
			setup(e)
			for run := int64(2001); run < 2004; run++ { // repeated actions share the installation
				e.schedule(t, run)
				e.runOne(t)
			}
			// S5 ghbudget keeps a header-less sent call as a settled reservation
			// (id nibble 0); a released-without-debit call would leave none.
			var settled, live int
			if err := e.pool.QueryRow(context.Background(),
				`SELECT count(*) FILTER (WHERE substr(id::text,15,1)='0'),
				        count(*) FILTER (WHERE substr(id::text,15,1)<>'0')
				   FROM gh_budget_reservation WHERE installation_id=$1`, e.inst).Scan(&settled, &live); err != nil {
				t.Fatal(err)
			}
			if settled != 3 || live != 0 {
				t.Fatalf("settled=%d live=%d after 3 sent GETs, want 3 and 0", settled, live)
			}
			e.wantPosts(t, 0)
		})
	}
}

func TestRerunPermission403SurvivesCancelledContextAfterThePost(t *testing.T) {
	e := newRerunEnv(t)
	e.enableP1(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.gh.post = func() (RerunResponse, error) { cancel(); return permission403() }
	a := e.schedule(t, 2010)
	_, _ = e.ledger.RunOne(ctx)
	if got := e.get(t, a.ID); got.PostOutcome != OutcomePermission {
		t.Fatalf("outcome lost: %+v", got)
	}
	e.followup(t)
	if mode, audits := e.p1Audits(t); mode != "shadow" || audits != 1 || e.escalations(t, 2010, ActionInboxNotify) != 1 {
		t.Fatalf("p1_mode=%s audits=%d", mode, audits)
	}
	e.wantPosts(t, 1)
}

func TestRerunPermission403SurvivesLeaseSweepBeforeTheAnswerIsStored(t *testing.T) {
	for name, steps := range map[string]int{"swept": 0, "swept and reconciled to failed": 4} {
		t.Run(name, func(t *testing.T) {
			e := newRerunEnv(t)
			e.enableP1(t)
			var a Action
			e.gh.post = func() (RerunResponse, error) {
				// the POST is slow: the lease runs out and recovery works on the row
				e.clock.Advance(3 * time.Minute)
				_, _ = e.ledger.Sweep(context.Background())
				for i := 0; i < steps; i++ {
					e.clock.Advance(3 * time.Minute)
					_, _ = e.ledger.ReconcileOne(context.Background())
				}
				return permission403()
			}
			a = e.schedule(t, 2011)
			e.runOne(t)
			if got := e.get(t, a.ID); got.PostOutcome != OutcomePermission {
				t.Fatalf("outcome lost: %+v", got)
			}
			e.followup(t)
			if mode, audits := e.p1Audits(t); mode != "shadow" || audits != 1 || e.escalations(t, 2011, ActionInboxNotify) != 1 {
				t.Fatalf("p1_mode=%s audits=%d", mode, audits)
			}
			e.wantPosts(t, 1)
		})
	}
}

func TestRerunLostOutcomeIsTreatedAsPossiblePermissionDenial(t *testing.T) {
	e := newRerunEnv(t)
	e.enableP1(t)
	e.gh.post = accepted
	a := e.schedule(t, 2012)
	// the worker died after the POST answered and before the answer was stored
	if _, err := e.pool.Exec(context.Background(), `UPDATE ci_watch_action SET state='executing', lease_epoch=1,
		expected_attempt=2, post_started_at=$2, lease_until=$2 WHERE id=$1`, a.ID, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.settleAll(t)
	e.followup(t)
	e.followup(t)
	if mode, audits := e.p1Audits(t); mode != "shadow" || audits != 1 || e.escalations(t, 2012, ActionInboxNotify) != 1 ||
		e.escalations(t, 2012, ActionIssueCreate) != 0 {
		t.Fatalf("p1_mode=%s audits=%d", mode, audits)
	}
	e.wantPosts(t, 0)
}

func TestRerunPermission403StillSwitchesShadowWhenReconcileFindsAManualRerun(t *testing.T) {
	e := newRerunEnv(t)
	e.enableP1(t)
	e.gh.post = func() (RerunResponse, error) { e.gh.setAttempt(2); return permission403() }
	a := e.schedule(t, 2013)
	e.runOne(t)
	e.settleAll(t) // reconcile reads run_attempt 2: done
	if got := e.get(t, a.ID); got.State != StateDone || got.PostOutcome != OutcomePermission {
		t.Fatalf("row: %+v", got)
	}
	e.followup(t)
	if mode, audits := e.p1Audits(t); mode != "shadow" || audits != 1 || e.escalations(t, 2013, ActionInboxNotify) != 1 {
		t.Fatalf("p1_mode=%s audits=%d", mode, audits)
	}
	e.wantPosts(t, 1)
}

func TestRerunPermission403StillSwitchesShadowWhenAnEscalationAlreadyExists(t *testing.T) {
	e := newRerunEnv(t)
	e.enableP1(t)
	e.gh.post = permission403
	e.schedule(t, 2014)
	e.runOne(t)
	n := e.action(2014, ActionIssueCreate, "")
	n.Protocol = "P2"
	if _, ins, err := e.ledger.escalate(context.Background(), nil, n); err != nil || !ins {
		t.Fatalf("seed escalation: inserted=%v err=%v", ins, err)
	}
	e.followup(t)
	e.followup(t)
	if mode, audits := e.p1Audits(t); mode != "shadow" || audits != 1 ||
		e.escalations(t, 2014, ActionIssueCreate)+e.escalations(t, 2014, ActionInboxNotify) != 1 {
		t.Fatalf("p1_mode=%s audits=%d", mode, audits)
	}
}

func TestRerunFollowupDoesNotSwitchShadowAgainAfterAnOperatorReenablesP1(t *testing.T) {
	e := newRerunEnv(t)
	e.enableP1(t)
	e.gh.post = permission403
	e.schedule(t, 2015)
	e.runOne(t)
	e.followup(t)
	if _, err := e.pool.Exec(context.Background(), `UPDATE ci_watch_config SET p1_mode='act' WHERE workspace_id=$1::uuid`, e.ws); err != nil {
		t.Fatal(err)
	}
	e.settleAll(t)
	e.followup(t)
	if mode, _ := e.p1Audits(t); mode != "act" {
		t.Fatalf("an old 403 switched the repo back: p1_mode=%s", mode)
	}
}

// Round 3 of CHE-4278 review: a P2 escalation must be executable, and a
// reconciled-done row with no stored answer must still reach the Shadow switch.

func (e *rerunEnv) scheduleSigned(t *testing.T, runID int64, sig string) Action {
	t.Helper()
	a, _, err := e.ledger.Schedule(context.Background(), e.action(runID, ActionRerun, sig))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *rerunEnv) ownerState(t *testing.T, sig string) string {
	t.Helper()
	var s string
	_ = e.pool.QueryRow(context.Background(), `SELECT state FROM ci_signature_owner WHERE workspace_id=$1::uuid AND signature_hash=$2`,
		e.ws, sig).Scan(&s)
	return s
}

func TestRerunFollowupP2IssueIsCreatedThroughExecution(t *testing.T) {
	e := newRerunEnv(t)
	e.enableP1(t)
	e.ledger.RegisterLocal(ActionIssueCreate, e.insertEffect("issue"))
	e.gh.post = func() (RerunResponse, error) { return RerunResponse{Status: 502}, nil }
	e.scheduleSigned(t, 2020, "sig-2020")
	e.runOne(t)
	e.settleAll(t) // reconcile finds no rerun: failed/reconcile_not_found
	e.followup(t)
	e.settleAll(t) // RunOne executes the issue_create
	if got := e.count(t, "issue"); got != 1 {
		t.Fatalf("issues created = %d, want 1 (the issue_create action was cancelled as owned_elsewhere?)", got)
	}
	if s := e.ownerState(t, "sig-2020"); s != "open" {
		t.Fatalf("owner state = %q", s)
	}
	e.wantPosts(t, 1)
}

func TestScheduleP1SecondFailureP2IssueIsCreatedThroughExecution(t *testing.T) {
	e := newRerunEnv(t)
	e.ledger.RegisterLocal(ActionIssueCreate, e.insertEffect("issue"))
	run := Run{WorkspaceID: e.ws, RepoOwner: "o", RepoName: e.repo, RunID: 2021, RunAttempt: 2}
	r, err := ScheduleP1(context.Background(), e.ledger, run, ClassTransient, "sig-2021", time.Time{}, e.clock.Now())
	if err != nil || r.Protocol != "P2" {
		t.Fatalf("route=%+v err=%v", r, err)
	}
	e.runOne(t)
	if got := e.count(t, "issue"); got != 1 {
		t.Fatalf("issues created = %d, want 1", got)
	}
}

func TestRerunFollowupP2WithHeldOwnerAddsNoActionAndStillMarksTheRow(t *testing.T) {
	e := newRerunEnv(t)
	e.ledger.RegisterLocal(ActionIssueCreate, e.insertEffect("issue"))
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO ci_signature_owner (workspace_id, repo_owner, repo_name, signature_hash, state, created_at)
		VALUES ($1::uuid,'o',$2,'sig-2022','open',$3)`, e.ws, e.repo, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.gh.post = func() (RerunResponse, error) { return RerunResponse{Status: 502}, nil }
	a := e.scheduleSigned(t, 2022, "sig-2022")
	e.runOne(t)
	e.settleAll(t)
	e.followup(t)
	if n := e.escalations(t, 2022, ActionIssueCreate); n != 0 {
		t.Fatalf("issue_create rows = %d with the signature already owned", n)
	}
	var marked bool
	_ = e.pool.QueryRow(context.Background(), `SELECT followup_at IS NOT NULL FROM ci_watch_action WHERE id=$1::uuid`, a.ID).Scan(&marked)
	if !marked {
		t.Fatal("row not marked: followup would retry forever")
	}
}

func TestRerunUnknownOutcomeReconciledToDoneStillSwitchesShadowAndNotifies(t *testing.T) {
	e := newRerunEnv(t)
	e.enableP1(t)
	e.gh.post = accepted
	a := e.schedule(t, 2023)
	// the worker died after a permission 403 and before storing it; an operator reran the run
	if _, err := e.pool.Exec(context.Background(), `UPDATE ci_watch_action SET state='executing', lease_epoch=1,
		expected_attempt=2, post_started_at=$2, lease_until=$2 WHERE id=$1`, a.ID, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.gh.setAttempt(2)
	e.settleAll(t)
	if got := e.get(t, a.ID); got.State != StateDone || got.Reason != ReasonReconciled || got.PostOutcome != "" {
		t.Fatalf("row: %+v", got)
	}
	e.followup(t)
	e.followup(t)
	if mode, audits := e.p1Audits(t); mode != "shadow" || audits != 1 || e.escalations(t, 2023, ActionInboxNotify) != 1 {
		t.Fatalf("p1_mode=%s audits=%d", mode, audits)
	}
	e.wantPosts(t, 0)
}

func TestRerunAcceptedPostDoneRowIsNeverFollowedUp(t *testing.T) {
	e := newRerunEnv(t)
	e.enableP1(t)
	e.gh.post = accepted
	e.schedule(t, 2024)
	e.runOne(t)
	e.followup(t)
	if mode, audits := e.p1Audits(t); mode != "act" || audits != 0 || e.escalations(t, 2024, ActionInboxNotify) != 0 {
		t.Fatalf("a successful rerun was escalated: p1_mode=%s audits=%d", mode, audits)
	}
}
