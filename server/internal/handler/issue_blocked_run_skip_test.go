package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6973 (plan CHE-6940 step 6): an issue entering blocked starts no run only
// when the owner's own condition wakeup waits on the same ref as
// metadata.blocked_on. The mode sits behind workspace settings.blocked_run_skip.

type blockedSkip struct {
	issue, agent, other string
}

func newBlockedSkip(t *testing.T, mode string) blockedSkip {
	t.Helper()
	seedTestCatalog(t)
	if mode != "" {
		setWorkspaceSettings(t, `{"blocked_run_skip":{"mode":"`+mode+`"}}`)
	}
	agent := seededReadyAgentID(t)
	other := dbfx.Issue(t, "blocked-skip-target", testutil.Cols{"status": "in_progress"})
	issue := dbfx.Issue(t, "blocked-skip", testutil.Cols{
		"status": "blocked", "assignee_type": "agent", "assignee_id": agent,
		"metadata": testutil.Raw(`'{"blocked_on":"issue:` + other + `"}'::jsonb`),
	})
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id=$1`, issue) })
	return blockedSkip{issue: issue, agent: agent, other: other}
}

// wakeup inserts an enabled condition wakeup for agent on the issue. Each
// override replaces one column, so a case names only what makes it differ.
func (b blockedSkip) wakeup(t *testing.T, agent string, condition map[string]any, set string) string {
	t.Helper()
	raw, _ := json.Marshal(condition)
	var id string
	dbfx.QueryRow(t, `INSERT INTO issue_wakeup (id, workspace_id, issue_id, agent_id, created_by, instruction, kind, mode, condition, expires_at, on_timeout)
		VALUES (gen_random_uuid(), $1, $2, $3, $3, 'continue', 'event', 'once', $4::jsonb, now() + interval '2 days', 'wake') RETURNING id::text`,
		testWorkspaceID, b.issue, agent, string(raw)).Scan(&id)
	if set != "" {
		dbfx.Exec(t, `UPDATE issue_wakeup SET `+set+` WHERE id=$1`, id)
	}
	return id
}

func (b blockedSkip) linked(t *testing.T, set string) string {
	t.Helper()
	return b.wakeup(t, b.agent, map[string]any{"type": "other_issue", "issue_id": b.other, "state": "done"}, set)
}

// enter runs the dispatch the status move to blocked makes.
func (b blockedSkip) enter(t *testing.T) bool {
	t.Helper()
	issueID := parseUUID(b.issue)
	issue, err := testHandler.Queries.GetIssue(context.Background(), issueID)
	if err != nil {
		t.Fatal(err)
	}
	return testHandler.dispatchIssueRun(context.Background(), issue, service.IssueRunTrigger{
		IssueID: issueID, AgentID: parseUUID(b.agent), AssigneeType: "agent", BlockedEntry: true,
	}, "agent", b.agent, "")
}

func (b blockedSkip) runs(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status IN ('queued','dispatched')`, b.issue)
}

func (b blockedSkip) acts(t *testing.T, action string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action=$2`, b.issue, action)
}

func TestBlockedRunSkip_MatchingOwnerWakeupStartsNoRun(t *testing.T) {
	b := newBlockedSkip(t, "block")
	id := b.linked(t, "")
	if b.enter(t) {
		t.Fatal("entry started a run although the owner's wakeup waits on blocked_on")
	}
	if got := b.runs(t); got != 0 {
		t.Fatalf("pending runs = %d, want 0", got)
	}
	if got := b.acts(t, "blocked_entry_linked"); got != 1 {
		t.Fatalf("blocked_entry_linked rows = %d, want 1", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup WHERE id=$1 AND enabled`, id); got != 1 {
		t.Fatal("the linked wakeup must stay enabled")
	}
}

func (b blockedSkip) sweep(t *testing.T) {
	t.Helper()
	if err := testHandler.SweepBlockedEntryLinks(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBlockedRunSkip_UnmatchedLinksKeepTheEntryRun(t *testing.T) {
	for name, tc := range map[string]func(b blockedSkip, t *testing.T){
		"another target": func(b blockedSkip, t *testing.T) {
			b.wakeup(t, b.agent, map[string]any{"type": "other_issue", "issue_id": b.issue, "state": "done"}, "")
		},
		"another state": func(b blockedSkip, t *testing.T) {
			b.wakeup(t, b.agent, map[string]any{"type": "other_issue", "issue_id": b.other, "state": "in_review"}, "")
		},
		"a pr condition": func(b blockedSkip, t *testing.T) {
			b.wakeup(t, b.agent, map[string]any{"type": "pull_request", "event": "merged"}, "")
		},
		"a time wakeup": func(b blockedSkip, t *testing.T) {
			dbfx.Exec(t, `INSERT INTO issue_wakeup (id, workspace_id, issue_id, agent_id, created_by, instruction, kind, mode, next_fire_at, expires_at, on_timeout)
				VALUES (gen_random_uuid(), $1, $2, $3, $3, 'later', 'at', 'once', now() + interval '1 hour', now() + interval '2 days', 'wake')`, testWorkspaceID, b.issue, b.agent)
		},
		"another agent's matching wakeup": func(b blockedSkip, t *testing.T) {
			var other string
			dbfx.QueryRow(t, `SELECT gen_random_uuid()::text`).Scan(&other)
			b.wakeup(t, other, map[string]any{"type": "other_issue", "issue_id": b.other, "state": "done"}, "")
		},
		"no deadline":            func(b blockedSkip, t *testing.T) { b.linked(t, "expires_at = NULL") },
		"deadline beyond 7 days": func(b blockedSkip, t *testing.T) { b.linked(t, "expires_at = now() + interval '8 days'") },
		"end on timeout":         func(b blockedSkip, t *testing.T) { b.linked(t, "on_timeout = 'end'") },
		"a last_error":           func(b blockedSkip, t *testing.T) { b.linked(t, "last_error = 'Condition pending: x'") },
		"disabled":               func(b blockedSkip, t *testing.T) { b.linked(t, "enabled = false, disabled_at = now()") },
	} {
		t.Run(name, func(t *testing.T) {
			b := newBlockedSkip(t, "block")
			tc(b, t)
			if !b.enter(t) || b.runs(t) != 1 {
				t.Fatalf("entry must start its run (pending runs = %d)", b.runs(t))
			}
			if got := b.acts(t, "blocked_entry_linked"); got != 0 {
				t.Fatalf("blocked_entry_linked rows = %d, want 0", got)
			}
		})
	}
}

func TestBlockedRunSkip_NoBlockedOnKeepsTheEntryRun(t *testing.T) {
	b := newBlockedSkip(t, "block")
	b.linked(t, "")
	dbfx.Exec(t, `UPDATE issue SET metadata = '{}' WHERE id=$1`, b.issue)
	if !b.enter(t) {
		t.Fatal("entry without blocked_on must start its run")
	}
}

func TestBlockedRunSkip_EveryRefFormMatchesItsOwnFlag(t *testing.T) {
	const id = "11111111-2222-4333-8444-555555555555"
	for ref, cond := range map[string]map[string]any{
		"issue:" + id:                    {"type": "other_issue", "issue_id": id, "state": "done"},
		"issue:" + id + "@in_review":     {"type": "other_issue", "issue_id": id, "state": "in_review"},
		"pr:checks":                      {"type": "pull_request", "event": "checks_finished"},
		"pr:merged":                      {"type": "pull_request", "event": "merged"},
		"pr:conflicts":                   {"type": "pull_request", "event": "conflicts"},
		"pr:dequeued":                    {"type": "pull_request", "event": "dequeued"},
		"children":                       {"type": "children_done"},
		"children@2":                     {"type": "children_done", "stage": 2},
		"gh-run:Cheese-Work/multica:123": {"type": "github_run", "repo": "cheese-work/multica", "run_id": 123, "state": "completed"},
	} {
		raw, _ := json.Marshal(cond)
		if !blockedOnMatches(ref, db.IssueWakeup{Condition: raw}) {
			t.Errorf("%s must match %s", ref, raw)
		}
	}
	raw, _ := json.Marshal(map[string]any{"type": "children_done", "stage": 2})
	if blockedOnMatches("children", db.IssueWakeup{Condition: raw}) || blockedOnMatches("children@3", db.IssueWakeup{Condition: raw}) {
		t.Error("a different stage must not match")
	}
}

func TestBlockedRunSkip_ModesOffAndWarn(t *testing.T) {
	for _, tc := range []struct{ mode, settings string }{{"off", `{"blocked_run_skip":{"mode":"off"}}`}, {"unreadable", `{"blocked_run_skip":{"mode":"bogus"}}`}} {
		t.Run(tc.mode, func(t *testing.T) {
			b := newBlockedSkip(t, "")
			setWorkspaceSettings(t, tc.settings)
			b.linked(t, "")
			if !b.enter(t) || b.acts(t, "blocked_entry_linked")+b.acts(t, "blocked_entry_would_skip") != 0 {
				t.Fatal("off must start the run and record nothing")
			}
		})
	}
	// Default (no key) is warn: the run starts and one row names it.
	b := newBlockedSkip(t, "")
	b.linked(t, "")
	if !b.enter(t) || b.runs(t) != 1 {
		t.Fatal("warn must start the run")
	}
	var runID string
	dbfx.QueryRow(t, `SELECT details->>'run_id' FROM activity_log WHERE issue_id=$1 AND action='blocked_entry_would_skip'`, b.issue).Scan(&runID)
	var task string
	dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE issue_id=$1`, b.issue).Scan(&task)
	if runID == "" || runID != task {
		t.Fatalf("warn row run_id = %q, want the started run %q", runID, task)
	}
}

func TestBlockedRunSkip_RefusedAdmissionAtEntryKeepsTheRun(t *testing.T) {
	b := newBlockedSkip(t, "block")
	b.linked(t, "")
	dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = now() WHERE id=$1`, b.agent)
	t.Cleanup(func() { dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = NULL WHERE id=$1`, b.agent) })
	b.enter(t)
	if got := b.acts(t, "blocked_entry_linked"); got != 0 {
		t.Fatal("a paused agent's entry must not be linked")
	}
}

func TestBlockedRunSkip_RearmStartsTheBlockedRunWhenTheLinkBreaks(t *testing.T) {
	for name, break_ := range map[string]string{
		"last_error":            "last_error = 'boom'",
		"disabled":              "enabled = false, disabled_at = now()",
		"expired without a run": "enabled = false, disabled_at = now(), timed_out_at = now()",
		"refused by the guard":  "enabled = false, disabled_at = now(), last_error = 'the issue used its workflow''s run budget'",
	} {
		t.Run(name, func(t *testing.T) {
			b := newBlockedSkip(t, "block")
			id := b.linked(t, "")
			if b.enter(t) {
				t.Fatal("entry must be linked")
			}
			b.sweep(t)
			if b.runs(t) != 0 {
				t.Fatal("a live link must not start a run")
			}
			dbfx.Exec(t, `UPDATE issue_wakeup SET `+break_+` WHERE id=$1`, id)
			b.sweep(t)
			if b.runs(t) != 1 || b.acts(t, "blocked_entry_rearmed") != 1 {
				t.Fatalf("re-arm: runs=%d rows=%d, want 1 and 1", b.runs(t), b.acts(t, "blocked_entry_rearmed"))
			}
			b.sweep(t)
			if b.acts(t, "blocked_entry_rearmed") != 1 {
				t.Fatal("re-arm must happen once")
			}
		})
	}
}

func (b blockedSkip) linkKey(t *testing.T) *string {
	t.Helper()
	var link *string
	dbfx.QueryRow(t, `SELECT metadata->>'blocked_entry_link' FROM issue WHERE id=$1`, b.issue).Scan(&link)
	return link
}

func TestBlockedRunSkip_FiredWakeupRetiresTheLinkWithoutRearm(t *testing.T) {
	b := newBlockedSkip(t, "block")
	id := b.linked(t, "")
	b.enter(t)
	// The wakeup fired: it ended with its own run.
	dbfx.Exec(t, `UPDATE issue_wakeup SET enabled=false, disabled_at=now() WHERE id=$1`, id)
	dbfx.Exec(t, `INSERT INTO agent_task_queue (id, agent_id, runtime_id, issue_id, status, context)
		SELECT gen_random_uuid(), $1, runtime_id, $2, 'completed', jsonb_build_object('wakeup_id', $3::text) FROM agent WHERE id=$1`, b.agent, b.issue, id)
	b.sweep(t)
	if b.acts(t, "blocked_entry_rearmed") != 0 || b.runs(t) != 0 {
		t.Fatal("a wakeup that ran must not re-arm")
	}
	if b.linkKey(t) != nil {
		t.Fatal("a settled link must be retired so the sweep stops selecting it")
	}
}

func TestBlockedRunSkip_LeavingBlockedDropsTheLink(t *testing.T) {
	b := newBlockedSkip(t, "block")
	b.linked(t, "")
	b.enter(t)
	dbfx.Exec(t, `UPDATE issue SET status='in_progress' WHERE id=$1`, b.issue)
	moveBlockedOn(t, b.issue, map[string]any{"status": "todo"}).Want(200)
	if b.linkKey(t) != nil {
		t.Fatal("leaving blocked must drop blocked_entry_link")
	}
}

// Review finding 1 (PR 1917): a refused re-arm must stay owed. The link stays
// until a replacement run is admitted, and a pass after the refusal clears
// starts it.
func TestBlockedRunSkip_RefusedRearmStaysOwedUntilAdmitted(t *testing.T) {
	b := newBlockedSkip(t, "block")
	id := b.linked(t, "")
	b.enter(t)
	dbfx.Exec(t, `UPDATE issue_wakeup SET enabled=false, disabled_at=now(), last_error='boom' WHERE id=$1`, id)
	dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = now() WHERE id=$1`, b.agent)
	t.Cleanup(func() { dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = NULL WHERE id=$1`, b.agent) })
	b.sweep(t)
	b.sweep(t)
	if b.runs(t) != 0 {
		t.Fatal("a paused agent must not get a run")
	}
	if b.linkKey(t) == nil {
		t.Fatal("a refused re-arm must keep the link so a later pass retries")
	}
	if got := b.acts(t, "blocked_entry_rearm_refused"); got != 1 {
		t.Fatalf("refusal rows = %d, want 1 (the same refusal is recorded once)", got)
	}
	dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = NULL WHERE id=$1`, b.agent)
	b.sweep(t)
	if b.runs(t) != 1 || b.acts(t, "blocked_entry_rearmed") != 1 || b.linkKey(t) != nil {
		t.Fatalf("recovery: runs=%d rearmed=%d, want 1 and 1 with the link dropped", b.runs(t), b.acts(t, "blocked_entry_rearmed"))
	}
}

// Review finding 2 (PR 1917): more than one page of live links must not starve
// a broken link that sorts after them.
func TestBlockedRunSkip_BrokenLinkBehindManyLiveLinksIsStillRearmed(t *testing.T) {
	b := newBlockedSkip(t, "block")
	for i := 0; i < 205; i++ {
		other := dbfx.Issue(t, "blocked-skip-live", testutil.Cols{"status": "in_progress"})
		live := dbfx.Issue(t, "blocked-skip-live-link", testutil.Cols{
			"status": "blocked", "assignee_type": "agent", "assignee_id": b.agent,
			"metadata": testutil.Raw(`'{"blocked_on":"issue:` + other + `"}'::jsonb`),
		})
		var w string
		dbfx.QueryRow(t, `INSERT INTO issue_wakeup (id, workspace_id, issue_id, agent_id, created_by, instruction, kind, mode, condition, expires_at, on_timeout)
			VALUES (gen_random_uuid(), $1, $2, $3, $3, 'continue', 'event', 'once', jsonb_build_object('type','other_issue','issue_id',$4::text,'state','done'), now() + interval '2 days', 'wake') RETURNING id::text`,
			testWorkspaceID, live, b.agent, other).Scan(&w)
		dbfx.Exec(t, `UPDATE issue SET metadata = metadata || jsonb_build_object('blocked_entry_link', $2::text), updated_at = now() - interval '1 day' WHERE id=$1`, live, w)
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id=$1`, live) })
	}
	id := b.linked(t, "")
	b.enter(t)
	dbfx.Exec(t, `UPDATE issue_wakeup SET last_error='boom' WHERE id=$1`, id)
	b.sweep(t)
	if b.runs(t) != 1 {
		t.Fatalf("broken link behind 205 live links: runs=%d, want 1", b.runs(t))
	}
}

// Step 1's deferral leaves a continuation live: a deferred time wakeup on the
// same issue neither breaks nor replaces the linked condition wakeup.
func TestBlockedRunSkip_TimeDeferralLeavesTheLinkedContinuationLive(t *testing.T) {
	b := newBlockedSkip(t, "block")
	id := b.linked(t, "")
	dbfx.Exec(t, `INSERT INTO issue_wakeup (id, workspace_id, issue_id, agent_id, created_by, instruction, kind, mode, event_types, next_fire_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $3, 'poll', 'every', 'continuous', '{}', now() + interval '1 hour')`, testWorkspaceID, b.issue, b.agent)
	if b.enter(t) {
		t.Fatal("the linked entry must start no run")
	}
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, action, details) VALUES ($1, $2, 'system', 'wake_deferred', '{}')`, testWorkspaceID, b.issue)
	b.sweep(t)
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup WHERE id=$1 AND enabled`, id); got != 1 || b.runs(t) != 0 {
		t.Fatalf("linked wakeup enabled=%d runs=%d, want 1 and 0", got, b.runs(t))
	}
}
