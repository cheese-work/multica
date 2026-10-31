package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

type recoveryClaimFixture struct {
	store       *RecoveryClaimStore
	pool        *pgxpool.Pool
	fixture     *dbfx.Fixture
	workspaceID pgtype.UUID
	agentID     string
	runtimeID   string
	issueIDs    []pgtype.UUID
}

func newRecoveryClaimFixture(t *testing.T, issues int) *recoveryClaimFixture {
	t.Helper()
	pool := newResolveOriginatorPool(t)
	workspaceID, userID, agentID, _ := seedAttributionFixture(t, pool)
	fixture := dbfx.New(pool, workspaceID, userID)
	f := &recoveryClaimFixture{
		store:       NewRecoveryClaimStore(pool),
		pool:        pool,
		fixture:     fixture,
		workspaceID: util.MustParseUUID(workspaceID),
		agentID:     agentID,
	}
	if err := pool.QueryRow(context.Background(), `SELECT runtime_id FROM agent WHERE id = $1`, agentID).Scan(&f.runtimeID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < issues; i++ {
		id := fixture.Issue(t, "recovery claim issue", dbfx.Cols{"assignee_type": "agent", "assignee_id": agentID})
		f.issueIDs = append(f.issueIDs, util.MustParseUUID(id))
	}
	return f
}

func (f *recoveryClaimFixture) newFailedTaskID() pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

// retryChain returns a generator of failed task ids that form one retry
// lineage on an issue: the original attempt first, then each retry as a child
// of the previous attempt via parent_task_id.
func (f *recoveryClaimFixture) retryChain(t *testing.T, issue int) func() pgtype.UUID {
	t.Helper()
	var previous string
	return func() pgtype.UUID {
		cols := dbfx.Cols{"issue_id": util.UUIDToString(f.issueIDs[issue]), "status": "failed", "runtime_id": f.runtimeID}
		if previous != "" {
			cols["parent_task_id"] = previous
		}
		previous = f.fixture.Task(t, f.agentID, cols)
		return util.MustParseUUID(previous)
	}
}

func (f *recoveryClaimFixture) claimFailed(t *testing.T, issue int, mode RecoveryMode, failed pgtype.UUID, now time.Time) (RecoveryClaim, RecoveryClaimOutcome) {
	t.Helper()
	claim, outcome, err := f.store.Claim(context.Background(), RecoveryClaimParams{
		WorkspaceID:   f.workspaceID,
		IssueID:       f.issueIDs[issue],
		FailedTaskID:  failed,
		Mode:          mode,
		NativeCeiling: 3,
		PacedCap:      2,
		Spacing:       120 * time.Second,
		Now:           now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return claim, outcome
}

func (f *recoveryClaimFixture) failedTaskID(t *testing.T) pgtype.UUID {
	t.Helper()
	return f.newFailedTaskID()
}

func (f *recoveryClaimFixture) claim(t *testing.T, issue int, mode RecoveryMode, now time.Time) (RecoveryClaim, RecoveryClaimOutcome) {
	t.Helper()
	claim, outcome, err := f.store.Claim(context.Background(), RecoveryClaimParams{
		WorkspaceID:   f.workspaceID,
		IssueID:       f.issueIDs[issue],
		FailedTaskID:  f.failedTaskID(t),
		Mode:          mode,
		NativeCeiling: 3,
		PacedCap:      2,
		Spacing:       120 * time.Second,
		Now:           now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return claim, outcome
}

func TestRecoveryClaimSameFailedRunIsClaimedOnce(t *testing.T) {
	f := newRecoveryClaimFixture(t, 1)
	now := time.Now()
	p := RecoveryClaimParams{
		WorkspaceID: f.workspaceID, IssueID: f.issueIDs[0], FailedTaskID: f.failedTaskID(t),
		Mode: RecoveryModeFreshReplay, NativeCeiling: 3, PacedCap: 2, Spacing: 120 * time.Second, Now: now,
	}
	first, outcome, err := f.store.Claim(context.Background(), p)
	if err != nil || outcome != RecoveryAdmitted {
		t.Fatalf("first claim: outcome=%q err=%v, want admitted", outcome, err)
	}
	second, outcome, err := f.store.Claim(context.Background(), p)
	if err != nil || outcome != RecoveryDuplicate {
		t.Fatalf("second claim: outcome=%q err=%v, want duplicate", outcome, err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate returned claim %v, want the first claim %v", second.ID, first.ID)
	}
}

func TestRecoveryClaimFreshReplayAllowsOnePerIssue(t *testing.T) {
	f := newRecoveryClaimFixture(t, 1)
	now := time.Now()
	if _, outcome := f.claim(t, 0, RecoveryModeFreshReplay, now); outcome != RecoveryAdmitted {
		t.Fatalf("first fresh replay: outcome=%q, want admitted", outcome)
	}
	// A different failed run on the same issue, well after the spacing window.
	if _, outcome := f.claim(t, 0, RecoveryModeFreshReplay, now.Add(10*time.Minute)); outcome != RecoveryIssueAllowanceSpent {
		t.Fatalf("second fresh replay on one issue: outcome=%q, want issue_allowance_spent", outcome)
	}
}

func TestRecoveryClaimPacedCapAndSpacing(t *testing.T) {
	f := newRecoveryClaimFixture(t, 4)
	now := time.Now()
	first, outcome := f.claim(t, 0, RecoveryModeFreshReplay, now)
	if outcome != RecoveryAdmitted {
		t.Fatalf("first: outcome=%q, want admitted", outcome)
	}
	if _, outcome := f.claim(t, 1, RecoveryModeFreshReplay, now.Add(119*time.Second)); outcome != RecoveryWaitSpacing {
		t.Fatalf("119s after the first: outcome=%q, want wait_spacing", outcome)
	}
	if _, outcome := f.claim(t, 1, RecoveryModeFreshReplay, now.Add(120*time.Second)); outcome != RecoveryAdmitted {
		t.Fatalf("120s after the first: outcome=%q, want admitted", outcome)
	}
	if _, outcome := f.claim(t, 2, RecoveryModeFreshReplay, now.Add(10*time.Minute)); outcome != RecoveryWaitCap {
		t.Fatalf("third paced claim while two are in flight: outcome=%q, want wait_cap", outcome)
	}
	if err := f.store.Settle(context.Background(), first.ID, RecoveryStateCompleted); err != nil {
		t.Fatal(err)
	}
	if _, outcome := f.claim(t, 2, RecoveryModeFreshReplay, now.Add(10*time.Minute)); outcome != RecoveryAdmitted {
		t.Fatalf("after one settled: outcome=%q, want admitted", outcome)
	}
}

func TestRecoveryClaimAmbiguousSubmissionKeepsItsSlot(t *testing.T) {
	f := newRecoveryClaimFixture(t, 3)
	now := time.Now()
	a, _ := f.claim(t, 0, RecoveryModeFreshReplay, now)
	f.claim(t, 1, RecoveryModeFreshReplay, now.Add(2*time.Minute))
	if err := f.store.MarkAmbiguous(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, outcome := f.claim(t, 2, RecoveryModeFreshReplay, now.Add(10*time.Minute)); outcome != RecoveryWaitCap {
		t.Fatalf("ambiguous claim must keep its slot: outcome=%q, want wait_cap", outcome)
	}
	if err := f.store.Settle(context.Background(), a.ID, RecoveryStateReconciled); err != nil {
		t.Fatal(err)
	}
	if _, outcome := f.claim(t, 2, RecoveryModeFreshReplay, now.Add(10*time.Minute)); outcome != RecoveryAdmitted {
		t.Fatalf("reconciled claim frees its slot: outcome=%q, want admitted", outcome)
	}
}

func TestRecoveryClaimNativeResumeIsRecordedButExemptFromPacing(t *testing.T) {
	f := newRecoveryClaimFixture(t, 4)
	now := time.Now()
	f.claim(t, 0, RecoveryModeFreshReplay, now)
	f.claim(t, 1, RecoveryModeFreshReplay, now.Add(2*time.Minute))
	// Two paced claims are in flight and the spacing window is open, yet an
	// ordinary native resume retry is admitted and recorded in the ledger.
	claim, outcome := f.claim(t, 2, RecoveryModeNativeResume, now.Add(2*time.Minute))
	if outcome != RecoveryAdmitted {
		t.Fatalf("native resume with the paced cap full: outcome=%q, want admitted", outcome)
	}
	if got := f.fixture.Count(t, `SELECT count(*) FROM recovery_claim WHERE id = $1 AND mode = 'native_resume'`, util.UUIDToString(claim.ID)); got != 1 {
		t.Fatalf("native resume claim rows = %d, want 1 recorded", got)
	}
	// A native resume does not occupy a paced slot.
	var paced int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM recovery_claim WHERE workspace_id = $1 AND mode <> 'native_resume' AND state IN ('claimed','submitted','ambiguous')`,
		f.workspaceID).Scan(&paced); err != nil || paced != 2 {
		t.Fatalf("paced slots in use = %d err=%v, want 2", paced, err)
	}
}

// The native ceiling counts the original attempt, so a ceiling of 3 admits the
// original plus two recovery retries, whatever native mode each retry used.
func TestRecoveryClaimNativeCeilingCountsOriginalAttempt(t *testing.T) {
	for _, mode := range []RecoveryMode{RecoveryModeNativeResume, RecoveryModeNativeRelease} {
		t.Run(string(mode), func(t *testing.T) {
			f := newRecoveryClaimFixture(t, 1)
			next := f.retryChain(t, 0)
			now := time.Now()
			for i := 0; i < 2; i++ {
				c, outcome := f.claimFailed(t, 0, mode, next(), now.Add(time.Duration(i)*3*time.Minute))
				if outcome != RecoveryAdmitted {
					t.Fatalf("retry %d: outcome=%q, want admitted", i+1, outcome)
				}
				if err := f.store.Settle(context.Background(), c.ID, RecoveryStateFailed); err != nil {
					t.Fatal(err)
				}
			}
			if _, outcome := f.claimFailed(t, 0, mode, next(), now.Add(10*time.Minute)); outcome != RecoveryNativeCeiling {
				t.Fatalf("third retry at ceiling 3: outcome=%q, want native_ceiling", outcome)
			}
		})
	}
}

func TestRecoveryClaimNativeModesShareOneCeiling(t *testing.T) {
	f := newRecoveryClaimFixture(t, 1)
	next := f.retryChain(t, 0)
	now := time.Now()
	for i, mode := range []RecoveryMode{RecoveryModeNativeResume, RecoveryModeNativeRelease} {
		if _, outcome := f.claimFailed(t, 0, mode, next(), now.Add(time.Duration(i)*3*time.Minute)); outcome != RecoveryAdmitted {
			t.Fatalf("%s: outcome=%q, want admitted", mode, outcome)
		}
	}
	if _, outcome := f.claimFailed(t, 0, RecoveryModeNativeResume, next(), now.Add(10*time.Minute)); outcome != RecoveryNativeCeiling {
		t.Fatalf("resume after one resume and one release: outcome=%q, want native_ceiling", outcome)
	}
}

// An exhausted native lineage is not a fresh-replay budget.
func TestRecoveryClaimExhaustedNativeLineageGetsNoFreshReplay(t *testing.T) {
	f := newRecoveryClaimFixture(t, 1)
	next := f.retryChain(t, 0)
	now := time.Now()
	for i := 0; i < 2; i++ {
		c, outcome := f.claimFailed(t, 0, RecoveryModeNativeResume, next(), now)
		if outcome != RecoveryAdmitted {
			t.Fatalf("setup retry %d: outcome=%q, want admitted", i+1, outcome)
		}
		if err := f.store.Settle(context.Background(), c.ID, RecoveryStateFailed); err != nil {
			t.Fatal(err)
		}
	}
	if _, outcome := f.claimFailed(t, 0, RecoveryModeFreshReplay, next(), now.Add(10*time.Minute)); outcome != RecoveryNativeCeiling {
		t.Fatalf("fresh replay after native exhaustion: outcome=%q, want native_ceiling", outcome)
	}
}

// Native accounting is per retry lineage: an exhausted earlier chain on the
// issue neither blocks nor shortens a later independent chain.
func TestRecoveryClaimIndependentLineagesCountSeparately(t *testing.T) {
	f := newRecoveryClaimFixture(t, 1)
	chainA, chainB := f.retryChain(t, 0), f.retryChain(t, 0)
	now := time.Now()
	for i := 0; i < 2; i++ {
		c, outcome := f.claimFailed(t, 0, RecoveryModeNativeResume, chainA(), now.Add(time.Duration(i)*3*time.Minute))
		if outcome != RecoveryAdmitted {
			t.Fatalf("chain A retry %d: outcome=%q, want admitted", i+1, outcome)
		}
		if err := f.store.Settle(context.Background(), c.ID, RecoveryStateFailed); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		c, outcome := f.claimFailed(t, 0, RecoveryModeNativeResume, chainB(), now.Add(time.Duration(10+i)*time.Minute))
		if outcome != RecoveryAdmitted {
			t.Fatalf("chain B retry %d after chain A exhausted: outcome=%q, want admitted", i+1, outcome)
		}
		if err := f.store.Settle(context.Background(), c.ID, RecoveryStateFailed); err != nil {
			t.Fatal(err)
		}
	}
	if _, outcome := f.claimFailed(t, 0, RecoveryModeNativeResume, chainB(), now.Add(20*time.Minute)); outcome != RecoveryNativeCeiling {
		t.Fatalf("chain B third retry: outcome=%q, want native_ceiling", outcome)
	}
}

// The fresh allowance stays per issue, but an exhausted lineage cannot spend it
// while another lineage on the issue still has native budget.
func TestRecoveryClaimFreshReplayFollowsItsOwnLineage(t *testing.T) {
	f := newRecoveryClaimFixture(t, 1)
	chainA, chainB := f.retryChain(t, 0), f.retryChain(t, 0)
	now := time.Now()
	for i := 0; i < 2; i++ {
		c, outcome := f.claimFailed(t, 0, RecoveryModeNativeResume, chainA(), now)
		if outcome != RecoveryAdmitted {
			t.Fatalf("chain A retry %d: outcome=%q, want admitted", i+1, outcome)
		}
		if err := f.store.Settle(context.Background(), c.ID, RecoveryStateFailed); err != nil {
			t.Fatal(err)
		}
	}
	if _, outcome := f.claimFailed(t, 0, RecoveryModeFreshReplay, chainA(), now.Add(10*time.Minute)); outcome != RecoveryNativeCeiling {
		t.Fatalf("fresh replay of exhausted chain A: outcome=%q, want native_ceiling", outcome)
	}
	if _, outcome := f.claimFailed(t, 0, RecoveryModeFreshReplay, chainB(), now.Add(10*time.Minute)); outcome != RecoveryAdmitted {
		t.Fatalf("fresh replay of unexhausted chain B: outcome=%q, want admitted", outcome)
	}
}

func TestRecoveryClaimConcurrentCallersNeverExceedCap(t *testing.T) {
	const callers = 8
	f := newRecoveryClaimFixture(t, callers)
	now := time.Now()
	var wg sync.WaitGroup
	outcomes := make([]RecoveryClaimOutcome, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, outcome, err := f.store.Claim(context.Background(), RecoveryClaimParams{
				WorkspaceID: f.workspaceID, IssueID: f.issueIDs[i], FailedTaskID: f.newFailedTaskID(),
				Mode: RecoveryModeFreshReplay, NativeCeiling: 3, PacedCap: 2, Spacing: 0, Now: now,
			})
			if err != nil {
				t.Error(err)
				return
			}
			outcomes[i] = outcome
		}(i)
	}
	close(start)
	wg.Wait()
	admitted := 0
	for _, o := range outcomes {
		if o == RecoveryAdmitted {
			admitted++
		}
	}
	if admitted != 2 {
		t.Fatalf("admitted %d of %d concurrent claims, want exactly 2 (outcomes %v)", admitted, callers, outcomes)
	}
}
