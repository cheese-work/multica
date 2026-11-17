package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-6968 (plan CHE-6940, sink B): in block, an unchanged declared time fire
// is deferred, never consumed; an agent time wakeup must declare what it
// watches. Both sit behind workspace settings.time_wakeup_skip.mode.

func (e skipEnv) acts(t *testing.T, action string) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action=$2`, e.issue, action)
}

func (e skipEnv) pendingDue(t *testing.T, id pgtype.UUID) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND event_type='time.due' AND processed_at IS NULL`, id)
}

func (e skipEnv) enabled(t *testing.T, id pgtype.UUID) bool {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM issue_wakeup WHERE id=$1 AND enabled`, id) == 1
}

// deferred arms a declared wakeup, runs it once, completes the run and fires
// again with nothing changed in block: the second fire is deferred.
func (e skipEnv) deferred(t *testing.T, inputs ...string) pgtype.UUID {
	t.Helper()
	e.blockMode(t)
	id := e.declared(t, inputs...)
	e.twoFires(t, id, nil)
	return id
}

func (e skipEnv) tick(t *testing.T) {
	t.Helper()
	if err := e.s.TickWorkspaces(context.Background(), parseTestUUID(t, e.f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
}

func TestBlockDefersUnchangedDeclaredFire(t *testing.T) {
	e := newSkipEnv(t, false)
	id := e.deferred(t, "issue")
	if got := wakeRuns(t, e.f, id); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
	if !e.enabled(t, id) || e.pendingDue(t, id) != 1 {
		t.Fatalf("deferred wakeup must stay enabled with its fire pending (enabled=%v pending=%d)", e.enabled(t, id), e.pendingDue(t, id))
	}
	if got := e.acts(t, "wake_deferred"); got != 1 {
		t.Fatalf("wake_deferred rows = %d, want 1", got)
	}
	if got := e.declaredSkips(t); got != 0 {
		t.Fatalf("a deferral is not a skip: %d skip rows", got)
	}
	// Another scheduler pass with nothing changed neither runs nor logs again.
	e.tick(t)
	if got, rows := wakeRuns(t, e.f, id), e.acts(t, "wake_deferred"); got != 1 || rows != 1 {
		t.Fatalf("second pass: runs=%d rows=%d, want 1 and 1", got, rows)
	}
}

func TestDeferredWakeupFiresOnNextChange(t *testing.T) {
	e := newSkipEnv(t, false)
	id := e.deferred(t, "issue")
	e.finishRuns(t, "completed")
	e.f.Exec(t, `UPDATE issue SET status='in_review', revision=revision+1 WHERE id=$1`, e.issue)
	e.tick(t)
	if got := wakeRuns(t, e.f, id); got != 2 {
		t.Fatalf("runs after a change = %d, want 2", got)
	}
	if e.pendingDue(t, id) != 0 {
		t.Fatal("the fire must be consumed once it runs")
	}
}

func TestDeferredWakeupFiresAtOriginalExpiry(t *testing.T) {
	e := newSkipEnv(t, false)
	id := e.deferred(t, "issue")
	e.f.Exec(t, `UPDATE issue_wakeup SET expires_at=now()-interval '1 second' WHERE id=$1`, id)
	e.tick(t)
	if got := wakeRuns(t, e.f, id); got != 2 {
		t.Fatalf("runs at expiry = %d, want 2", got)
	}
	if e.enabled(t, id) {
		t.Fatal("an expired wakeup must end")
	}
}

func TestOffReplaysEveryDeferredWakeup(t *testing.T) {
	e := newSkipEnv(t, false)
	a := e.deferred(t, "issue")
	b := e.declared(t, "children")
	e.twoFires(t, b, nil)
	if e.pendingDue(t, a) != 1 || e.pendingDue(t, b) != 1 {
		t.Fatal("both fires must be deferred")
	}
	e.mode(t, `{"time_wakeup_skip":{"mode":"off"}}`)
	e.tick(t)
	// One agent holds one waiting run per issue: the second fire joins the pass
	// after the first run has started, and is not lost.
	e.finishRuns(t, "completed")
	e.tick(t)
	for _, id := range []pgtype.UUID{a, b} {
		if got := wakeRuns(t, e.f, id); got != 2 {
			t.Fatalf("wakeup %s runs after off = %d, want 2", util.UUIDToString(id), got)
		}
	}
}

func TestWarnLogsWouldDeferWithTheRunThatFollowed(t *testing.T) {
	e := newSkipEnv(t, false)
	e.mode(t, `{"time_wakeup_skip":{"mode":"warn"}}`)
	id := e.declared(t, "issue")
	e.twoFires(t, id, nil)
	if got := wakeRuns(t, e.f, id); got != 2 {
		t.Fatalf("warn must run: runs = %d, want 2", got)
	}
	var taskID string
	e.f.QueryRow(t, `SELECT details->>'task_id' FROM activity_log WHERE issue_id=$1 AND action='wake_would_defer'`, e.issue).Scan(&taskID)
	var newest string
	e.f.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE issue_id=$1 ORDER BY created_at DESC LIMIT 1`, e.issue).Scan(&newest)
	if taskID == "" || taskID != newest {
		t.Fatalf("would-defer row names run %q, want the run that followed %q", taskID, newest)
	}
}

func TestDeferralNeverAppliesToDeadlineFires(t *testing.T) {
	e := newSkipEnv(t, false)
	e.blockMode(t)
	w := wakeCreate(t, e.f, e.s, e.issue, WakeupInput{AgentID: e.agent, Kind: "event", EventTypes: []string{"comment.created"}, ExpiresInSeconds: 3600, OnTimeout: "wake", Instruction: "Escalate"})
	e.f.Exec(t, `UPDATE issue_wakeup SET source_task_id=$2, watch_inputs='{issue}', expires_at=now()-interval '1 second' WHERE id=$1`, w.ID, dbid.NewV7())
	e.tick(t)
	if got := wakeRuns(t, e.f, w.ID); got != 1 {
		t.Fatalf("a deadline fire must run: runs = %d, want 1", got)
	}
	if got := e.acts(t, "wake_deferred"); got != 0 {
		t.Fatalf("deadline fire deferred: %d rows", got)
	}
}

func TestWatchPRSeesNewHeadAndFinishedCheckAsChange(t *testing.T) {
	for name, change := range map[string]string{
		"new head":       `UPDATE github_pull_request SET head_sha='h2' WHERE id=$1`,
		"finished check": `UPDATE github_pull_request SET checks_rollup_state='SUCCESS' WHERE id=$1`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newSkipEnv(t, true)
			id := e.deferred(t, "pr")
			e.finishRuns(t, "completed")
			e.f.Exec(t, change, e.prID)
			e.tick(t)
			if got := wakeRuns(t, e.f, id); got != 2 {
				t.Fatalf("runs after %s = %d, want 2", name, got)
			}
		})
	}
}

// agentCreate registers a time wakeup the way an agent's run does (a source run).
func (e skipEnv) agentCreate(t *testing.T, watch []string) (time.Time, error) {
	t.Helper()
	source := e.f.Task(t, e.agent, testutil.Cols{"issue_id": e.issue, "status": "running", "runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + e.agent + "')")})
	_, err := e.s.Create(context.Background(), e.issue, parseTestUUID(t, e.f.UserID), parseTestUUID(t, source), WakeupInput{
		AgentID: e.agent, Kind: "at", AfterSeconds: 600, Instruction: "Check back", WatchInputs: watch,
	})
	return time.Now(), err
}

func TestBlockRefusesAgentTimeWakeupWithoutWatch(t *testing.T) {
	e := newSkipEnv(t, false)
	e.blockMode(t)
	if _, err := e.agentCreate(t, nil); !errors.Is(err, ErrWakeupWatchInput) {
		t.Fatalf("err = %v, want ErrWakeupWatchInput", err)
	} else if want := "--watch"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q must name the %s flag", err, want)
	}
	for _, ok := range [][]string{{"always"}, {"issue", "pr"}} {
		if _, err := e.agentCreate(t, ok); err != nil {
			t.Fatalf("declared %v refused: %v", ok, err)
		}
	}
	if _, err := e.agentCreate(t, []string{"always", "issue"}); !errors.Is(err, ErrWakeupWatchInput) {
		t.Fatalf("always mixed with inputs: err = %v, want ErrWakeupWatchInput", err)
	}
}

func TestWarnAndMemberCreatesWithoutWatchSucceed(t *testing.T) {
	e := newSkipEnv(t, false)
	e.mode(t, `{"time_wakeup_skip":{"mode":"warn"}}`)
	if _, err := e.agentCreate(t, nil); err != nil {
		t.Fatalf("warn must allow the create: %v", err)
	}
	if got := e.acts(t, "wake_watch_missing"); got != 1 {
		t.Fatalf("wake_watch_missing rows = %d, want 1", got)
	}
	e.blockMode(t)
	wakeCreate(t, e.f, e.s, e.issue, WakeupInput{AgentID: e.agent, Kind: "at", AfterSeconds: 600, Instruction: "A person asked"})
	if got := e.acts(t, "wake_watch_missing"); got != 1 {
		t.Fatalf("a member create is exempt: rows = %d, want 1", got)
	}
}
