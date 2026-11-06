package ciwatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Round 2 of CHE-4278 review: sent calls are always debited, and a stored
// permission 403 always reaches the Shadow switch.

func (e *rerunEnv) budgetRemaining(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT remaining FROM gh_installation_budget WHERE installation_id=$1`, e.inst).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

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
			if got := e.budgetRemaining(t); got >= 4000-3 {
				t.Fatalf("remaining = %d after 3 sent GETs; the reserve was released without a debit", got)
			}
			var live int
			_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM gh_budget_reservation WHERE installation_id=$1`, e.inst).Scan(&live)
			if live != 0 {
				t.Fatalf("%d reservations left", live)
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
