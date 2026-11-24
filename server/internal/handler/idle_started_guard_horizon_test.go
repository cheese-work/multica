package handler

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-9606 (CHE-9460 S2): a time wakeup is continuation only when it fires
// within the guard threshold. The grid is wakeup distance x wakeup owner x
// caller of ListIdleStartedGuardIssues.

type horizonOwner struct {
	name string
	cols func(f idleGuardFixture, other string) testutil.Cols
}

var horizonOwners = []horizonOwner{
	{"current assignee", func(f idleGuardFixture, _ string) testutil.Cols { return testutil.Cols{"agent_id": f.agent} }},
	{"other agent", func(_ idleGuardFixture, other string) testutil.Cols { return testutil.Cols{"agent_id": other} }},
	{"member", func(_ idleGuardFixture, _ string) testutil.Cols {
		return testutil.Cols{"agent_id": testutil.Raw("NULL"), "created_by": testUserID}
	}},
}

var horizonDistances = []struct {
	name     string
	interval string
	counts   bool // a wakeup this far out is continuation (threshold is 4 h)
}{
	{"1 h", "1 hour", true},
	{"48 h", "48 hours", false},
}

func (owner horizonOwner) wakeup(test *testing.T, f idleGuardFixture, other, interval string) {
	test.Helper()
	cols := testutil.Cols{"kind": "at", "mode": "once", "next_fire_at": testutil.Raw(`now() + interval '` + interval + `'`)}
	for key, value := range owner.cols(f, other) {
		cols[key] = value
	}
	f.wakeup(test, cols)
}

func TestIdleStartedGuardHorizonMainScan(test *testing.T) {
	for _, owner := range horizonOwners {
		for _, distance := range horizonDistances {
			test.Run(owner.name+"/"+distance.name, func(test *testing.T) {
				fixture := newIdleGuardFixture(test, 5)
				other := createHandlerTestAgent(test, "horizon other "+test.Name(), []byte("[]"))
				owner.wakeup(test, fixture, other, distance.interval)
				if got, want := fixture.found(test), !distance.counts; got != want {
					test.Fatalf("reported = %v, want %v", got, want)
				}
			})
		}
	}
}

func TestIdleStartedGuardHorizonActRecheck(test *testing.T) {
	for _, owner := range horizonOwners {
		for _, distance := range horizonDistances {
			test.Run(owner.name+"/"+distance.name, func(test *testing.T) {
				fixture := newIdleActFixture(test, 5)
				other := createHandlerTestAgent(test, "horizon other "+test.Name(), []byte("[]"))
				findings, err := testHandler.checkIdleStartedIssues(context.Background())
				if err != nil {
					test.Fatal(err)
				}
				var found *idleGuardFinding
				for index := range findings {
					if uuidToString(findings[index].IssueID) == fixture.issue {
						found = &findings[index]
					}
				}
				if found == nil {
					test.Fatal("fixture issue was not found idle")
				}
				// The wakeup lands after the scan; the re-check under the lock decides.
				owner.wakeup(test, fixture.idleGuardFixture, other, distance.interval)
				if err := testHandler.actOnIdleFinding(context.Background(), idleGuardSettings{}, *found); err != nil {
					test.Fatal(err)
				}
				moved := fixture.status(test) != "in_progress"
				if moved == distance.counts {
					test.Fatalf("moved = %v, want %v (status %q)", moved, !distance.counts, fixture.status(test))
				}
			})
		}
	}
}

func TestIdleStartedGuardHorizonNotice(test *testing.T) {
	for _, owner := range horizonOwners {
		for _, distance := range horizonDistances {
			test.Run(owner.name+"/"+distance.name, func(test *testing.T) {
				fixture := newIdleActFixture(test, 5)
				other := createHandlerTestAgent(test, "horizon other "+test.Name(), []byte("[]"))
				// The move stands but the leader route is down, so the notice stays owed.
				fixture.breakRoute(test)
				fixture.sweepFailing(test)
				if fixture.receipt(test, "state") != "moved" {
					test.Fatalf("setup: state = %q, want moved", fixture.receipt(test, "state"))
				}
				fixture.fixRoute(test)
				owner.wakeup(test, fixture.idleGuardFixture, other, distance.interval)
				fixture.sweep(test)
				want, tasks := "notified", 1
				if distance.counts {
					want, tasks = "resumed", 0
				}
				if got := fixture.receipt(test, "state"); got != want || fixture.leaderTasks(test) != tasks {
					test.Fatalf("state = %q, leader tasks = %d, want %q and %d", got, fixture.leaderTasks(test), want, tasks)
				}
			})
		}
	}
}

// horizon_seconds and after_seconds are independent: a short idle threshold does
// not shorten the wakeup horizon (CHE-9515 passes a short after_seconds).
func TestIdleStartedGuardHorizonIndependentOfAfterSeconds(test *testing.T) {
	fixture := newIdleGuardFixture(test, 1)
	fixture.wakeup(test, testutil.Cols{"kind": "at", "mode": "once", "next_fire_at": testutil.Raw(`now() + interval '1 hour'`)})
	list := func(horizon time.Duration) int {
		rows, err := testHandler.Queries.ListIdleStartedGuardIssues(context.Background(), db.ListIdleStartedGuardIssuesParams{
			WorkspaceID: util.MustParseUUID(testWorkspaceID), IssueID: util.MustParseUUID(fixture.issue), Statuses: []string{"in_progress"},
			AfterSeconds: 120, HorizonSeconds: horizon.Seconds(),
		})
		if err != nil {
			test.Fatal(err)
		}
		return len(rows)
	}
	if got := list(4 * time.Hour); got != 0 {
		test.Fatalf("after_seconds 120, horizon 4 h: a 1 h wakeup did not count (rows = %d)", got)
	}
	if got := list(30 * time.Minute); got != 1 {
		test.Fatalf("after_seconds 120, horizon 30 min: a 1 h wakeup counted (rows = %d)", got)
	}
}
