package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/daemon"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// CHE-9607 (S1c): the handoff preflights look at the slot the insert will take.
// These tests drive the real writers (status move, workflow intent, edge move,
// route) with a run of the target already pending in another thread, then claim
// every run through the daemon and read what the agent would start with.

const (
	slotClaimOverlay = "https://slot-claim.example.test/mcp"
	slotClaimPRHead  = "96079607960796079607960796079607960796ab"
)

// slotClaim is one run as the daemon's claim returns it.
type slotClaim struct {
	Task   daemon.Task
	Prompt string
	Raw    string
}

// claimRuns claims queued runs on the runtime until every wanted run has been
// claimed, and returns them by id. The daemon serializes one agent's runs on an
// issue, so each claimed run is completed before the next claim, as the side
// run's reply would be.
func claimRuns(t *testing.T, runtimeID string, want ...string) map[string]slotClaim {
	t.Helper()
	got := map[string]slotClaim{}
	for range len(want) + 6 {
		if len(got) == len(want) {
			break
		}
		dbfx.Exec(t, `UPDATE agent_runtime SET status = 'online', last_seen_at = now() WHERE id = $1`, runtimeID)
		w := httptest.NewRecorder()
		req := newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/claim", nil)
		req = withURLParam(req, "runtimeId", runtimeID)
		testHandler.ClaimTaskByRuntime(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("ClaimTaskByRuntime: %d %s", w.Code, w.Body.String())
		}
		var resp struct {
			Task *daemon.Task `json:"task"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode claim: %v", err)
		}
		if resp.Task == nil {
			break
		}
		got[resp.Task.ID] = slotClaim{Task: *resp.Task, Prompt: daemon.BuildPrompt(*resp.Task, "claude"), Raw: w.Body.String()}
		dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', started_at = now(), completed_at = now() WHERE id = $1`, resp.Task.ID)
	}
	for _, id := range want {
		if _, ok := got[id]; !ok {
			t.Fatalf("run %s was not claimable; claimed %d of %d", id, len(got), len(want))
		}
	}
	return got
}

// slotLinkPR says whether the case under test links a PR to the issue. A bound
// PR makes the pin's squad preflight look only at runs stamped with its head, so
// the cases run both ways: eachPR flips it.
var slotLinkPR bool

func eachPR(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	for _, pr := range []bool{false, true} {
		t.Run(map[bool]string{false: "no PR", true: "PR bound"}[pr], func(t *testing.T) {
			prior := slotLinkPR
			slotLinkPR = pr
			t.Cleanup(func() { slotLinkPR = prior })
			fn(t)
		})
	}
}

// slotCase is one target (agent or squad leader) on one issue, with a connected
// app overlay and a linked PR, plus the run it already has in another thread.
type slotCase struct {
	slotFixture
	runtimeID string
	side      string // the leader's or agent's queued comment run in thread T
	// noOverlay marks a caller that builds no connected-app overlay for a squad
	// leader's run (the edge move and the route pass none), so the claim has none.
	noOverlay bool
	pr        bool // a PR is linked to the issue
}

// newSlotCase builds the fixture on its own issue (see newSlotFixture) and arms it.
func newSlotCase(t *testing.T, squad bool) slotCase {
	t.Helper()
	c := slotCase{slotFixture: newSlotFixture(t, squad)}
	c.arm(t)
	return c
}

// slotCaseOn arms a case on an issue a workflow fixture already made: it adds the
// threads (T with the handoff reply, and another) the fixture's comments give.
func slotCaseOn(t *testing.T, issueID, agentID string, squad bool) slotCase {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID) })
	c := slotCase{slotFixture: slotFixture{issue: issue, agentID: agentID, squad: squad}}
	c.threadT = dbfx.Comment(t, issueID, "thread T root")
	c.handoff = dbfx.Comment(t, issueID, slotHandoffBrief, testutil.Cols{"parent_id": c.threadT})
	c.otherCmt = dbfx.Comment(t, issueID, "another thread root")
	c.arm(t)
	return c
}

// arm links a PR to the issue and turns the connected-app overlay on.
func (c *slotCase) arm(t *testing.T) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, c.agentID).Scan(&c.runtimeID)
	if c.pr = slotLinkPR; c.pr {
		linkPR(t, util.UUIDToString(c.issue.ID), int32(9_000_000+time.Now().UnixNano()%1_000_000), slotClaimPRHead)
	}

	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.ComposioMCPApps, featureflag.Rule{Default: true})
	// The enqueue builds the overlay through the task service; the claim merges it
	// only when the handler's own flag is on.
	flags := featureflag.NewService(provider)
	priorTaskFlags, priorComposio, priorFlags := testHandler.TaskService.FeatureFlags, testHandler.TaskService.Composio, testHandler.FeatureFlags
	testHandler.TaskService.FeatureFlags, testHandler.FeatureFlags = flags, flags
	testHandler.TaskService.Composio = &overlayComposio{overlay: `{"mcpServers":{"composio":{"url":"` + slotClaimOverlay + `"}}}`}
	t.Cleanup(func() {
		testHandler.TaskService.FeatureFlags, testHandler.TaskService.Composio, testHandler.FeatureFlags = priorTaskFlags, priorComposio, priorFlags
	})
}

// sideRun queues a comment run of the target in thread T.
func (c *slotCase) sideRun(t *testing.T) {
	t.Helper()
	c.side = c.holder(t, c.threadT)
}

// wantSideRunStillReplies claims the side run and checks it is the comment
// reply it was: same thread, same trigger comment, no step handoff.
func (c slotCase) wantSideRunStillReplies(t *testing.T, claimed map[string]slotClaim) {
	t.Helper()
	got, ok := claimed[c.side]
	if !ok {
		t.Fatalf("side run %s was not claimed", c.side)
	}
	if got.Task.TriggerCommentID != c.threadT || got.Task.TriggerKind == service.TriggerKindStatusHandoff {
		t.Errorf("side run trigger = (%q, %q), want the thread T comment %s as a comment reply", got.Task.TriggerCommentID, got.Task.TriggerKind, c.threadT)
	}
	if strings.Contains(got.Prompt, slotHandoffBrief) {
		t.Errorf("the side run's prompt picked up the step handoff:\n%s", got.Prompt)
	}
}

// wantClaimedStepRun checks the claimed run is the step run: prompt, trigger,
// attribution, connected-app overlay and PR binding.
func (c slotCase) wantClaimedStepRun(t *testing.T, got slotClaim, thread string, attr handoffRun) {
	t.Helper()
	if got.Task.AgentID != c.agentID {
		t.Errorf("claimed run agent = %s, want %s", got.Task.AgentID, c.agentID)
	}
	var run handoffRun
	var threadID string
	dbfx.QueryRow(t, `
		SELECT COALESCE(trigger_comment_id::text, ''), COALESCE(trigger_kind, ''),
		       COALESCE(originator_user_id::text, ''), COALESCE(accountable_user_id::text, ''),
		       COALESCE(originator_source, ''), COALESCE(delegated_from_task_id::text, ''), COALESCE(comment_thread_id::text, '')
		FROM agent_task_queue WHERE id = $1`, got.Task.ID).Scan(&run.TriggerCommentID, &run.TriggerKind, &run.Originator, &run.Accountable, &run.Source, &run.DelegatedFrom, &threadID)
	if threadID != thread {
		t.Errorf("step run slot = %q, want %q", threadID, thread)
	}
	if run.Originator != attr.Originator || run.Accountable != attr.Accountable || run.Source != attr.Source || run.DelegatedFrom != attr.DelegatedFrom {
		t.Errorf("step run attribution = %+v, want %+v", run, attr)
	}
	if thread != "" {
		if got.Task.TriggerKind != service.TriggerKindStatusHandoff || got.Task.TriggerCommentID != c.handoff {
			t.Errorf("claimed trigger = (%q, %q), want (%s, status_handoff)", got.Task.TriggerCommentID, got.Task.TriggerKind, c.handoff)
		}
		if !strings.Contains(got.Prompt, slotHandoffBrief) || !strings.Contains(got.Prompt, "Handoff context") {
			t.Errorf("the step prompt lost the handoff brief:\n%s", got.Prompt)
		}
	} else if got.Task.TriggerKind == service.TriggerKindStatusHandoff || strings.Contains(got.Prompt, slotHandoffBrief) {
		t.Errorf("a step run in the NULL slot carries a handoff comment: kind %q\n%s", got.Task.TriggerKind, got.Prompt)
	}
	if !c.noOverlay && !strings.Contains(got.Raw, slotClaimOverlay) {
		t.Errorf("the claim carries no connected-app overlay: it was built for another originator than the insert used")
	}
	if got.Task.PinnedHeadSHA != "" {
		t.Errorf("a plain step run claims a pinned head %q; only an elevated run may", got.Task.PinnedHeadSHA)
	}
	var head string
	dbfx.QueryRow(t, `SELECT COALESCE(context->>'head_sha', '') FROM agent_task_queue WHERE id = $1`, got.Task.ID).Scan(&head)
	wantHead := ""
	if c.pr {
		wantHead = slotClaimPRHead
	}
	if head != wantHead {
		t.Errorf("step run PR binding = %q, want %q", head, wantHead)
	}
}

func (c slotCase) pendingRuns(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued','dispatched')`, util.UUIDToString(c.issue.ID), c.agentID)
}

func (c slotCase) newRunBeside(t *testing.T, known ...string) string {
	t.Helper()
	var id string
	dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND NOT (id::text = ANY($3)) ORDER BY created_at DESC LIMIT 1`,
		util.UUIDToString(c.issue.ID), c.agentID, known).Scan(&id)
	return id
}

func forEachSlotCase(t *testing.T, name string, fn func(t *testing.T, c slotCase)) {
	for _, squad := range []bool{false, true} {
		label := name + "/agent"
		if squad {
			label = name + "/squad"
		}
		t.Run(label, func(t *testing.T) {
			if testHandler == nil {
				t.Skip("database not available")
			}
			fn(t, newSlotCase(t, squad))
		})
	}
}

// statusMove moves the issue as a person would, optionally with the handoff comment.
func (c slotCase) statusMove(t *testing.T, withHandoff bool) (int, string) {
	t.Helper()
	issueID := util.UUIDToString(c.issue.ID)
	dbfx.Exec(t, `UPDATE issue SET status = 'backlog' WHERE id = $1`, issueID)
	body := map[string]any{"status": "todo"}
	if withHandoff {
		body["handoff_comment_id"] = c.handoff
	}
	req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, body), "id", issueID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	rec := testutil.Call(t, testHandler.UpdateIssue, req)
	return rec.Code, rec.Body.String()
}

func memberMoveAttribution() handoffRun {
	return handoffRun{Originator: testUserID, Accountable: testUserID, Source: "direct_human"}
}

// Acceptance (1): no handoff comment, a run in thread T. The move starts the
// step run in the NULL slot; the run in T is unchanged and still replies in T.
func TestSlotClaim_StatusMove_NoHandoff_SideRunDoesNotBlock(t *testing.T) {
	eachPR(t, func(t *testing.T) {
		forEachSlotCase(t, "status move", func(t *testing.T, c slotCase) {
			c.sideRun(t)
			if code, body := c.statusMove(t, false); code != http.StatusOK {
				t.Fatalf("move = %d: %s", code, body)
			}
			stepRun := c.newRunBeside(t, c.side)
			claimed := claimRuns(t, c.runtimeID, c.side, stepRun)
			c.wantClaimedStepRun(t, claimed[stepRun], "", memberMoveAttribution())
			c.wantSideRunStillReplies(t, claimed)
		})
	})
}

// Acceptance (3) and (5): the handoff comment is in thread T, T is free and a
// non-step run holds the NULL slot. The step run takes T.
func TestSlotClaim_StatusMove_Handoff_ThreadFree_NullHeld(t *testing.T) {
	eachPR(t, func(t *testing.T) {
		forEachSlotCase(t, "status move", func(t *testing.T, c slotCase) {
			nullHolder := c.holder(t, "")
			if code, body := c.statusMove(t, true); code != http.StatusOK {
				t.Fatalf("move = %d: %s", code, body)
			}
			stepRun := c.newRunBeside(t, nullHolder)
			if n := c.pendingRuns(t); n != 2 {
				t.Errorf("%d pending runs, want the NULL holder and the step run", n)
			}
			claimed := claimRuns(t, c.runtimeID, nullHolder, stepRun)
			c.wantClaimedStepRun(t, claimed[stepRun], c.threadT, memberMoveAttribution())
		})
	})
}

// Acceptance (2), (4) and (6): the handoff comment is in thread T and a run
// holds T. The move still commits, no second run is made, and the holder is
// not touched. A media-pending deferred holder counts as a holder too.
func TestSlotClaim_StatusMove_Handoff_ThreadHeld_MoveCommitsWithoutAbort(t *testing.T) {
	eachPR(t, func(t *testing.T) {
		for _, holder := range []struct {
			name string
			cols testutil.Cols
		}{
			{"queued", nil},
			{"media pending", testutil.Cols{"status": "deferred", "context": testutil.Raw(`'{"channel_issue_media_pending":"true"}'::jsonb`)}},
		} {
			forEachSlotCase(t, "status move/"+holder.name, func(t *testing.T, c slotCase) {
				var held string
				if holder.cols == nil {
					held = c.holder(t, c.threadT)
				} else {
					held = c.holder(t, c.threadT, holder.cols)
				}
				code, body := c.statusMove(t, true)
				if code != http.StatusOK {
					t.Fatalf("move = %d: %s; a run in the handoff's thread must not abort the move", code, body)
				}
				if got := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND status = 'todo'`, util.UUIDToString(c.issue.ID)); got != 1 {
					t.Errorf("the status move did not commit")
				}
				if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, util.UUIDToString(c.issue.ID), c.agentID); n != 1 {
					t.Errorf("%d runs after the refused insert, want only the holder %s", n, held)
				}
				if holder.cols == nil {
					claimed := claimRuns(t, c.runtimeID, held)
					if got := claimed[held]; got.Task.TriggerKind == service.TriggerKindStatusHandoff {
						t.Errorf("the holder was rewritten into the step run")
					}
				}
			})
		}
	})
}

func targetName(squad bool) string { return map[bool]string{false: "agent", true: "squad"}[squad] }

// slotIntentCase is an issue in a project whose in_progress step hands off to the
// ready agent, at todo until moved. For a squad target the issue is assigned to
// a squad the agent leads: no workflow step hands off to a squad, so its intent
// is the one a squad assignee's move leaves, written by the test.
func slotIntentCase(t *testing.T, squad bool) slotCase {
	t.Helper()
	issueID, agentID := handoffDispatchFixture(t)
	if squad {
		setIssueAssigneeDirect(t, issueID, "squad", dbfx.Squad(t, "slot intent squad "+workflowTestSuffix(), agentID))
	}
	return slotCaseOn(t, issueID, agentID, squad)
}

// moveIntoStep makes the intent a person's move into the step writes, carrying
// the handoff comment when asked, dispatches it, and returns the settled intent.
// An agent target takes the real status writer; a squad target takes the intent
// row that writer leaves for a squad, through the real dispatcher.
func (c slotCase) moveIntoStep(t *testing.T, withHandoff bool) handoffIntentRow {
	t.Helper()
	issueID := util.UUIDToString(c.issue.ID)
	if !c.squad {
		extra := map[string]any(nil)
		if withHandoff {
			extra = map[string]any{"handoff_comment_id": c.handoff}
		}
		moveIntoHandoffStep(t, issueID, extra)
		return wantOneIntent(t, issueID)
	}
	var squadID string
	dbfx.QueryRow(t, `SELECT assignee_id::text FROM issue WHERE id = $1`, issueID).Scan(&squadID)
	cols := testutil.Cols{
		"id": testutil.Raw("gen_random_uuid()"), "workspace_id": testWorkspaceID, "issue_id": issueID, "target_type": "squad", "target_id": squadID,
		"actor_type": "member", "actor_id": testUserID, "handoff_write": "slot-claim-squad",
	}
	if withHandoff {
		cols["context_comment_ids"] = testutil.Raw("ARRAY['" + c.handoff + "'::uuid]")
	}
	dbfx.Insert(t, "issue_handoff_dispatch", cols)
	intent := wantOneIntent(t, issueID)
	if _, err := testHandler.dispatchHandoffIntent(context.Background(), parseUUID(intent.ID)); err != nil {
		t.Fatalf("dispatchHandoffIntent: %v", err)
	}
	return wantOneIntent(t, issueID)
}

// Acceptance (1) through the workflow intent: the target has a queued comment run
// in thread T and the move carries no handoff comment. The intent dispatches one
// step run beside it, linked to the intent and claimed with the step prompt.
func TestSlotClaim_WorkflowIntent_SideRunDoesNotBlock(t *testing.T) {
	eachPR(t, func(t *testing.T) {
		for _, squad := range []bool{false, true} {
			t.Run(targetName(squad), func(t *testing.T) {
				c := slotIntentCase(t, squad)
				c.sideRun(t)

				intent := c.moveIntoStep(t, false)

				if intent.State != handoffDispatched || intent.TaskID == "" || intent.TaskID == c.side {
					t.Fatalf("intent = %+v, want a dispatched intent linked to a new step run (side run %s)", intent, c.side)
				}
				claimed := claimRuns(t, c.runtimeID, c.side, intent.TaskID)
				c.wantClaimedStepRun(t, claimed[intent.TaskID], "", memberMoveAttribution())
				c.wantSideRunStillReplies(t, claimed)
			})
		}
	})
}

// Acceptance (3) and (5) through the intent: the handoff comment is in thread T,
// T is free and a non-step run holds the NULL slot. The step run takes T.
func TestSlotClaim_WorkflowIntent_Handoff_ThreadFree_NullHeld(t *testing.T) {
	eachPR(t, func(t *testing.T) {
		for _, squad := range []bool{false, true} {
			t.Run(targetName(squad), func(t *testing.T) {
				c := slotIntentCase(t, squad)
				nullHolder := c.holder(t, "")

				intent := c.moveIntoStep(t, true)

				if intent.State != handoffDispatched || intent.TaskID == "" || intent.TaskID == nullHolder {
					t.Fatalf("intent = %+v, want a dispatched intent linked to a new step run (not the NULL holder %s)", intent, nullHolder)
				}
				claimed := claimRuns(t, c.runtimeID, nullHolder, intent.TaskID)
				c.wantClaimedStepRun(t, claimed[intent.TaskID], c.threadT, memberMoveAttribution())
			})
		}
	})
}

// Acceptance (2), (4) and (6) through the intent: a run holds the handoff's
// thread. The intent settles in the dispatcher's own transaction as a skipped
// duplicate, nothing aborts, and no second run is made.
func TestSlotClaim_WorkflowIntent_Handoff_ThreadHeld_IntentSkipsWithoutAbort(t *testing.T) {
	eachPR(t, func(t *testing.T) {
		for _, squad := range []bool{false, true} {
			for _, media := range []bool{false, true} {
				name := targetName(squad) + map[bool]string{false: "/queued", true: "/media pending"}[media]
				t.Run(name, func(t *testing.T) {
					c := slotIntentCase(t, squad)
					issueID := util.UUIDToString(c.issue.ID)
					var held string
					if media {
						held = c.holder(t, c.threadT, testutil.Cols{"status": "deferred", "context": testutil.Raw(`'{"channel_issue_media_pending":"true"}'::jsonb`)})
					} else {
						held = c.holder(t, c.threadT)
					}

					if !squad && !slotLinkPR {
						// The agent's move is judged before the write: with no PR a
						// run holding the handoff's thread stops it there, so the
						// move commits with no intent and no second run. A squad
						// intent is written by the writer and settles below.
						moveIntoHandoffStep(t, issueID, map[string]any{"handoff_comment_id": c.handoff})
						if n := dbfx.Count(t, `SELECT count(*) FROM issue_handoff_dispatch WHERE issue_id = $1`, issueID); n != 0 {
							t.Errorf("%d handoff intents, want none: the held thread stops the move's run", n)
						}
						if got := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND status = 'in_progress'`, issueID); got != 1 {
							t.Errorf("the move did not commit")
						}
						if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, c.agentID); n != 1 {
							t.Errorf("%d runs, want only the holder %s", n, held)
						}
						return
					}
					intent := c.moveIntoStep(t, true)

					if got := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND status IN ('in_progress','todo')`, issueID); got != 1 {
						t.Fatalf("the issue is gone or in an unexpected status")
					}
					if intent.State != handoffSkipped || intent.TaskID != "" {
						t.Errorf("intent = %+v, want a skipped intent with no run", intent)
					}
					var reason string
					dbfx.QueryRow(t, `SELECT COALESCE(skip_reason, '') FROM issue_handoff_dispatch WHERE id = $1`, intent.ID).Scan(&reason)
					if reason != "duplicate_pending_run" {
						t.Errorf("skip reason = %q, want duplicate_pending_run", reason)
					}
					if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, c.agentID); n != 1 {
						t.Errorf("%d runs, want only the holder %s", n, held)
					}
				})
			}
		}
	})
}

// moveOnEdge drives the real edge move into the step and returns the run the move
// inserted. The settle step runs only for a run the move inserted: a refused run
// (a duplicate among them) leaves it unset, which fails the test.
func moveOnEdge(t *testing.T, issue db.Issue, to string) string {
	t.Helper()
	t.Cleanup(giveBackRunBudgetHooks)
	var inserted string
	settle := func(_ context.Context, _ pgx.Tx, taskID pgtype.UUID) (func(context.Context), error) {
		inserted = util.UUIDToString(taskID)
		return nil, nil
	}
	moved, ranHandler, err := testHandler.tryMoveIssueOnEdgeSettle(context.Background(), issue, to, "ci_green_advance", nil, true, nil, settle)
	if err != nil || !moved || !ranHandler {
		t.Fatalf("tryMoveIssueOnEdge = moved %v, ran %v, err %v; want a move that ran the handler", moved, ranHandler, err)
	}
	if inserted == "" {
		t.Fatalf("the edge move inserted no run: the handler's run was refused or taken for a duplicate")
	}
	return inserted
}

// Acceptance (7), edge move: the step's handler is the target, which has a queued
// comment run in thread T. The move starts one step run beside it, committed
// with the move.
func TestSlotClaim_EdgeMove_SideRunDoesNotBlock(t *testing.T) {
	eachPR(t, func(t *testing.T) {
		edge, agentID := newHandoffEdgeCase(t, 9607101)
		c := slotCaseOn(t, edge.issueID, agentID, false)
		c.sideRun(t)
		issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(edge.issueID))
		if err != nil {
			t.Fatal(err)
		}

		inserted := moveOnEdge(t, issue, "in_review")

		stepRun := c.newRunBeside(t, c.side)
		if inserted != stepRun {
			t.Errorf("the edge move inserted run %s, but the run beside the side run is %s", inserted, stepRun)
		}
		claimed := claimRuns(t, c.runtimeID, c.side, stepRun)
		c.wantClaimedStepRun(t, claimed[stepRun], "", memberMoveAttribution())
		c.wantSideRunStillReplies(t, claimed)
	})
}

// Acceptance (7), edge move, squad: the step's handler is a squad, so the
// edge's own handoff assigns it and queues its leader's run. The workflow API
// takes no squad handler, so the stored step is rewritten to one; everything
// after that is the real edge. The leader has a queued comment run in thread T.
// The move starts one leader step run beside it. The edge builds no
// connected-app overlay for a leader's run, before or after S1c.
func TestSlotClaim_EdgeMove_SquadLeaderRun_SideRunDoesNotBlock(t *testing.T) {
	eachPR(t, func(t *testing.T) {
		edge, agentID := newHandoffEdgeCase(t, 9607102)
		squadID := dbfx.Squad(t, "slot edge squad "+workflowTestSuffix(), agentID)
		dbfx.Exec(t, `UPDATE issue_workflow SET steps = (
			SELECT jsonb_agg(CASE WHEN s->>'status_key' = 'in_review'
				THEN jsonb_set(s, '{handler}', jsonb_build_object('type', 'squad', 'id', $2::text)) ELSE s END)
			FROM jsonb_array_elements(steps) s) WHERE id = $1`, edge.workflowID, squadID)
		c := slotCaseOn(t, edge.issueID, agentID, true)
		c.noOverlay = true
		c.sideRun(t)
		issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(edge.issueID))
		if err != nil {
			t.Fatal(err)
		}

		inserted := moveOnEdge(t, issue, "in_review")
		if got := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND assignee_type = 'squad' AND status = 'in_review'`, edge.issueID); got != 1 {
			t.Fatalf("the edge did not hand the issue to the squad")
		}

		stepRun := c.newRunBeside(t, c.side)
		if inserted != stepRun {
			t.Errorf("the edge move inserted run %s, but the run beside the side run is %s", inserted, stepRun)
		}
		claimed := claimRuns(t, c.runtimeID, c.side, stepRun)
		c.wantClaimedStepRun(t, claimed[stepRun], "", memberMoveAttribution())
		c.wantSideRunStillReplies(t, claimed)
	})
}

func routeSlotCase(t *testing.T, squad bool) (routeCase, slotCase) {
	t.Helper()
	r := newRouteCase(t)
	if squad {
		dbfx.Exec(t, `UPDATE issue SET status = 'backlog', assignee_type = 'squad', assignee_id = $2 WHERE id = $1`,
			r.issueID, dbfx.Squad(t, "slot route squad "+workflowTestSuffix(), r.handlerID))
	}
	c := slotCaseOn(t, r.issueID, r.handlerID, squad)
	c.noOverlay = squad
	return r, c
}

// Acceptance (7), route: a person routes the issue into the target's step while
// the target has a queued comment run in thread T. The route starts one step run
// beside it and reports it started.
func TestSlotClaim_Route_SideRunDoesNotBlock(t *testing.T) {
	eachPR(t, func(t *testing.T) {
		for _, squad := range []bool{false, true} {
			t.Run(targetName(squad), func(t *testing.T) {
				r, c := routeSlotCase(t, squad)
				c.sideRun(t)
				to := "in_review"
				if squad {
					to = "in_progress"
				}

				var out routeResponse
				r.route(t, routeBody(to, nil), nil).Want(http.StatusOK).JSON(&out)

				if !out.Run.Started || out.Run.Refused != "" {
					t.Fatalf("run = %+v, want a started step run beside the run in thread T", out.Run)
				}
				stepRun := c.newRunBeside(t, c.side)
				claimed := claimRuns(t, c.runtimeID, c.side, stepRun)
				c.wantClaimedStepRun(t, claimed[stepRun], "", memberMoveAttribution())
				c.wantSideRunStillReplies(t, claimed)
			})
		}
	})
}

// Acceptance (8) through the budget resume: the approval restores the refused
// comment's run while the target has a pending run of another comment. The
// resume queues its own run, and the daemon claims it with the refused comment
// as its trigger, the refusal's actor as originator, the overlay (agent path)
// and the PR binding.
func TestSlotClaim_BudgetResume_RunBesideSurvivorOfAnotherSlot(t *testing.T) {
	for i, p := range bindPaths {
		if !slotAware(p) {
			continue
		}
		eachPR(t, func(t *testing.T) {
			t.Run(p.name, func(t *testing.T) {
				b := newBindCase(t, int64(9607300+i), p, true)
				issue, err := testHandler.Queries.GetIssue(context.Background(), util.MustParseUUID(b.c.issueID))
				if err != nil {
					t.Fatal(err)
				}
				c := slotCase{slotFixture: slotFixture{issue: issue, agentID: b.agent, squad: p.squad}}
				c.noOverlay = p.squad // the squad-leader resume builds no overlay
				c.arm(t)
				survivor := b.survivor(t, p, "queued", b.own(p))
				before := issueRuns(t, b.c)

				resume(t, b.id)

				b.wantOwnRunBesideSurvivor(t, survivor, before)
				resumed := uuidToString(wantTaskOf(t, b.id))
				claimed := claimRuns(t, c.runtimeID, uuidToString(survivor), resumed)
				got := claimed[resumed]
				if got.Task.AgentID != b.agent || got.Task.TriggerCommentID != uuidToString(b.c2) {
					t.Errorf("claimed resume = (agent %s, trigger %s), want (%s, %s)", got.Task.AgentID, got.Task.TriggerCommentID, b.agent, uuidToString(b.c2))
				}
				if got.Task.TriggerKind == service.TriggerKindStatusHandoff {
					t.Errorf("a resumed comment run claims as a status handoff")
				}
				if !strings.Contains(got.Prompt, "second") {
					t.Errorf("the resumed run's prompt lost the refused comment:\n%s", got.Prompt)
				}
				var originator, source, head string
				dbfx.QueryRow(t, `SELECT COALESCE(originator_user_id::text,''), COALESCE(originator_source,''), COALESCE(context->>'head_sha','') FROM agent_task_queue WHERE id = $1`, resumed).Scan(&originator, &source, &head)
				if originator != testUserID {
					t.Errorf("resumed run originator = %q, want the refusal's actor %s", originator, testUserID)
				}
				wantHead := ""
				if c.pr {
					wantHead = slotClaimPRHead
				}
				if head != wantHead {
					t.Errorf("resumed run PR binding = %q, want %q", head, wantHead)
				}
				if !c.noOverlay && !strings.Contains(got.Raw, slotClaimOverlay) {
					t.Errorf("the resumed claim carries no connected-app overlay")
				}
			})
		})
	}
}
