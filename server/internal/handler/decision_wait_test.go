package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// episodeFixture is an issue in a project whose need_decision step waits on the
// person in charge (handler human_in_charge), in_progress and assigned to the
// asking agent, with one open asked request to the issue's creator.
type episodeFixture struct {
	requestCoreFixture
	requestID string
}

func newEpisodeFixture(t *testing.T, group bool) episodeFixture {
	t.Helper()
	ensureParkCatalog(t)
	wf := createTestWorkflowSteps(t, "HIC episode "+workflowTestSuffix(), statusSteps("backlog", "todo", "in_progress", "need_decision", "blocked", "done"))
	setWorkflowHandoff(t, wf.ID, "need_decision", map[string]any{"handler": map[string]any{"type": "human_in_charge"}}).Want(http.StatusOK)
	projectID := createWorkflowTestProject(t, "HIC episode project "+workflowTestSuffix())
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	fixture := newRequestCoreFixture(t, group)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM decision_wait WHERE issue_id = $1`, fixture.issueID) })
	first, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		t.Fatal(err)
	}
	// The ask parked the issue; put it back where an agent run leaves it.
	dbfx.Exec(t, `UPDATE issue SET project_id = $1, status = 'in_progress', creator_type = 'member', creator_id = $2,
		assignee_type = 'agent', assignee_id = $3, metadata = metadata - 'decision_park' WHERE id = $4`,
		projectID, fixture.humanID, fixture.agentID, fixture.issueID)
	f := episodeFixture{fixture, uuidToString(first.Request.ID)}
	f.unseal(t)
	return f
}

// unseal puts the intent back as an agent that has not finished asking leaves
// it: creating the last request of an intent seals it, and sealing is what the
// entry has to do.
func (f episodeFixture) unseal(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE decision_intent SET sealed_at = NULL WHERE id = $1`, f.intentID)
}

// secondRequest is the group's request to the workspace owner.
func (f episodeFixture) secondRequest(t *testing.T) string {
	t.Helper()
	input := f.input()
	input.SessionID = parseUUID(laneNamingIssue(t, f.issueID, f.agentID, testUserID, "Owner lane"))
	second, err := testHandler.createDecisionRequest(f.request(), f.issue, input)
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress', assignee_type = 'agent', assignee_id = $2, metadata = metadata - 'decision_park' WHERE id = $1`, f.issueID, f.agentID)
	f.unseal(t)
	return uuidToString(second.Request.ID)
}

var agentRunHeaders = func(f episodeFixture) map[string]string {
	return map[string]string{"X-Actor-Source": "task_token", "X-Agent-ID": f.agentID, "X-Task-ID": f.taskID}
}

func (f episodeFixture) agentMove(t *testing.T, writer issueWriter, status string) *testutil.Response {
	t.Helper()
	return writer.write(t, f.issueID, map[string]any{"status": status}, agentRunHeaders(f))
}

func (f episodeFixture) memberMove(t *testing.T, status string) *testutil.Response {
	t.Helper()
	return issueWriters[0].write(t, f.issueID, map[string]any{"status": status}, map[string]string{"X-Actor-Source": "interactive_jwt"})
}

type episodeRow struct {
	id, phase, humanID, restoreStatus, restoreAssigneeType, restoreAssigneeID, suspendReason string
	restoreOnResume                                                                          bool
	requestIDs, intentIDs, groupAckIntentIDs                                                 []string
}

func (f episodeFixture) episodes(t *testing.T) []episodeRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT id::text, phase, human_user_id::text, restore_status, COALESCE(restore_assignee_type, ''), COALESCE(restore_assignee_id::text, ''),
		COALESCE(suspend_reason, ''), restore_on_resume, request_ids::text[], intent_ids::text[], group_ack_intent_ids::text[]
		FROM decision_wait WHERE issue_id = $1 AND closed_at IS NULL`, f.issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []episodeRow
	for rows.Next() {
		var row episodeRow
		if err := rows.Scan(&row.id, &row.phase, &row.humanID, &row.restoreStatus, &row.restoreAssigneeType, &row.restoreAssigneeID, &row.suspendReason, &row.restoreOnResume, &row.requestIDs, &row.intentIDs, &row.groupAckIntentIDs); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

func (f episodeFixture) onlyEpisode(t *testing.T) episodeRow {
	t.Helper()
	rows := f.episodes(t)
	if len(rows) != 1 {
		t.Fatalf("non-closed episodes = %d, want 1", len(rows))
	}
	return rows[0]
}

// untouched asserts a refused move changed nothing: no episode, the same
// status, assignee and seal state.
func (f episodeFixture) untouched(t *testing.T, wantEpisodes int) {
	t.Helper()
	if got := len(f.episodes(t)); got != wantEpisodes {
		t.Fatalf("non-closed episodes = %d, want %d", got, wantEpisodes)
	}
	status, assigneeType, assigneeID := issueStatusAndAssignee(t, f.issueID)
	if status != "in_progress" || assigneeType != "agent" || assigneeID != f.agentID {
		t.Fatalf("refused move changed the issue: %s %s/%s", status, assigneeType, assigneeID)
	}
	if dbfx.Count(t, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NOT NULL`, f.intentID) != 0 && wantEpisodes == 0 {
		t.Fatal("refused move sealed the intent")
	}
}

func (f episodeFixture) wantRefusal(t *testing.T, resp *testutil.Response, fragment string) {
	t.Helper()
	resp.Want(http.StatusConflict)
	if body := resp.Text(); !strings.Contains(body, fragment) {
		t.Fatalf("refusal = %s, want it to mention %q", body, fragment)
	}
}

func TestDecisionEpisodeEntryStoresTheEpisodeAndSealsTheIntent(t *testing.T) {
	for _, writer := range issueWriters {
		t.Run(writer.name, func(t *testing.T) {
			f := newEpisodeFixture(t, false)
			f.agentMove(t, writer, "need_decision").Want(http.StatusOK)
			status, assigneeType, assigneeID := issueStatusAndAssignee(t, f.issueID)
			if status != "need_decision" || assigneeType != "member" || assigneeID != f.humanID {
				t.Fatalf("issue = %s %s/%s, want need_decision held by the human in charge %s", status, assigneeType, assigneeID, f.humanID)
			}
			row := f.onlyEpisode(t)
			if row.phase != "waiting" || row.humanID != f.humanID || !row.restoreOnResume ||
				row.restoreStatus != "in_progress" || row.restoreAssigneeType != "agent" || row.restoreAssigneeID != f.agentID {
				t.Fatalf("episode = %+v, want waiting on the human, restoring in_progress/agent", row)
			}
			if len(row.requestIDs) != 1 || row.requestIDs[0] != f.requestID || len(row.intentIDs) != 1 || row.intentIDs[0] != f.intentID ||
				len(row.groupAckIntentIDs) != 1 || row.groupAckIntentIDs[0] != f.intentID {
				t.Fatalf("bound sets = %v %v %v, want the request, its intent, and the intent in group_ack", row.requestIDs, row.intentIDs, row.groupAckIntentIDs)
			}
			if dbfx.Count(t, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NOT NULL`, f.intentID) != 1 {
				t.Fatal("entry did not seal the intent")
			}
			if dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'dispatched') AND agent_id = $2`, f.issueID, f.agentID) != 0 {
				t.Fatal("entry started an agent run")
			}
		})
	}
}

func TestDecisionEpisodeEntryRefusals(t *testing.T) {
	otherHuman := func(t *testing.T) string { return hicMember(t, "member") }
	cases := []struct {
		name     string
		group    bool
		fragment string
		arrange  func(t *testing.T, f episodeFixture)
		move     func(t *testing.T, f episodeFixture) *testutil.Response
	}{
		{"no request", false, "", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'expired' WHERE id = $1`, f.requestID)
		}, nil},
		{"request to a member other than R", false, "one human", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE decision_request SET human_user_id = $2 WHERE id = $1`, f.requestID, otherHuman(t))
		}, nil},
		{"second open request to another human", true, "one human", func(t *testing.T, f episodeFixture) { f.secondRequest(t) }, nil},
		{"intent member addressed to another human", true, "one human", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE decision_request SET status = 'answered' WHERE id = $1`, f.secondRequest(t))
		}, nil},
		{"bound request already answered", true, "answer is already in", func(t *testing.T, f episodeFixture) {
			second := f.secondRequest(t)
			dbfx.Exec(t, `UPDATE decision_request SET human_user_id = $2, status = 'answered' WHERE id = $1`, second, f.humanID)
		}, nil},
		{"pending retraction in B", false, "retract", func(t *testing.T, f episodeFixture) { f.pendingRetraction(t, false) }, nil},
		{"rejected lane delivery", false, "lane", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE chat_message SET failure_reason = 'rejected' WHERE id = (SELECT ping_message_id FROM decision_request WHERE id = $1)`, f.requestID)
		}, nil},
		{"archived lane", false, "lane", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE chat_session SET status = 'archived' WHERE id = (SELECT chat_session_id FROM decision_request WHERE id = $1)`, f.requestID)
		}, nil},
		{"asking agent archived", false, "agent", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, f.agentID)
		}, nil},
		{"asking agent unbound", false, "agent", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE agent SET runtime_id = NULL WHERE id = $1`, f.agentID)
		}, nil},
		{"canInvokeAgent false", false, "no eligible human", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE agent SET permission_mode = 'private', owner_id = $2 WHERE id = $1`, f.agentID, otherHuman(t))
		}, nil},
		{"creator left, R falls to another person", false, "one human", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, f.humanID)
		}, nil},
		{"member hand move with no request", false, "", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'expired' WHERE id = $1`, f.requestID)
		}, func(t *testing.T, f episodeFixture) *testutil.Response { return f.memberMove(t, "need_decision") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEpisodeFixture(t, tc.group)
			tc.arrange(t, f)
			move := tc.move
			if move == nil {
				move = func(t *testing.T, f episodeFixture) *testutil.Response {
					return f.agentMove(t, issueWriters[0], "need_decision")
				}
			}
			f.wantRefusal(t, move(t, f), tc.fragment)
			f.untouched(t, 0)
		})
	}
}

// pendingRetraction adds an answer whose retraction is (delivered=false) or is
// not (delivered=true) still owed, on the request whose human withdrew it.
func (f episodeFixture) pendingRetraction(t *testing.T, delivered bool) {
	t.Helper()
	var pingID string
	dbfx.QueryRow(t, `SELECT ping_message_id::text FROM decision_request WHERE id = $1`, f.requestID).Scan(&pingID)
	cols := testutil.Cols{"decision_request_id": f.requestID, "answered_by_task_id": f.taskID, "source_chat_session_id": f.sessionID,
		"chat_message_ids": "{" + pingID + "}",
		"content_snapshot": "Yes", "effective_at": time.Now().Add(-time.Hour), "woken_at": time.Now().Add(-30 * time.Minute), "wake_outcome": "run_enqueued",
		"disclaimed_at": time.Now().Add(-10 * time.Minute), "withdrawal_comment_id": f.tagID}
	if delivered {
		cols["retraction_delivered_at"] = time.Now().Add(-5 * time.Minute)
		cols["retraction_outcome"] = "run_enqueued"
	}
	dbfx.Insert(t, "decision_answer", cols)
}

func TestDecisionEpisodeEntryAfterDeliveredRetractionKeepsGroupWake(t *testing.T) {
	f := newEpisodeFixture(t, false)
	woken := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	dbfx.Exec(t, `UPDATE decision_intent SET group_woken_at = $2 WHERE id = $1`, f.intentID, woken)
	f.pendingRetraction(t, true)
	f.agentMove(t, issueWriters[0], "need_decision").Want(http.StatusOK)
	row := f.onlyEpisode(t)
	if len(row.groupAckIntentIDs) != 0 || len(row.intentIDs) != 1 {
		t.Fatalf("group_ack = %v intents = %v, want the group-woken intent bound but not acknowledged", row.groupAckIntentIDs, row.intentIDs)
	}
	if dbfx.Count(t, `SELECT count(*) FROM decision_intent WHERE id = $1 AND group_woken_at = $2`, f.intentID, woken) != 1 {
		t.Fatal("entry changed group_woken_at")
	}
}

// suspendedEpisode stores an episode of the fixture's request as an earlier
// entry left it, then suspended it: the issue is blocked on Tofu-like
// placement, and flag says whether the latest status write was the episode's.
func createWaitParamsFor(f episodeFixture) db.CreateDecisionWaitParams {
	return db.CreateDecisionWaitParams{
		WorkspaceID: parseUUID(testWorkspaceID), IssueID: parseUUID(f.issueID), AskingAgentID: parseUUID(f.agentID), AskingTaskID: parseUUID(f.taskID),
		HumanUserID: parseUUID(f.humanID), RequestIds: []pgtype.UUID{parseUUID(f.requestID)}, IntentIds: []pgtype.UUID{parseUUID(f.intentID)},
		GroupAckIntentIds: []pgtype.UUID{parseUUID(f.intentID)}, RestoreStatus: "in_progress",
		RestoreAssigneeType: pgtype.Text{String: "agent", Valid: true}, RestoreAssigneeID: parseUUID(f.agentID),
	}
}

func (f episodeFixture) suspendedEpisode(t *testing.T, flag bool) {
	t.Helper()
	_, err := testHandler.Queries.CreateDecisionWait(context.Background(), createWaitParamsFor(f))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE decision_wait SET phase = 'suspended', suspend_reason = 'run_budget', suspended_at = now(), restore_on_resume = $2 WHERE issue_id = $1`, f.issueID, flag)
	dbfx.Exec(t, `UPDATE decision_intent SET sealed_at = now() WHERE id = $1`, f.intentID)
}

func TestDecisionEpisodeHandMoveWithOnlyTerminalRequestsIsRefused(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.suspendedEpisode(t, true)
	dbfx.Exec(t, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'expired' WHERE id = $1`, f.requestID)
	f.wantRefusal(t, f.memberMove(t, "need_decision"), "already holds this issue's decision")
	if rows := f.episodes(t); len(rows) != 1 || rows[0].phase != "suspended" {
		t.Fatalf("episodes = %+v, want the suspended one untouched", rows)
	}
}

func TestDecisionEpisodeReattach(t *testing.T) {
	callers := map[string]func(t *testing.T, f episodeFixture) *testutil.Response{
		"member": func(t *testing.T, f episodeFixture) *testutil.Response { return f.memberMove(t, "need_decision") },
		"asking task": func(t *testing.T, f episodeFixture) *testutil.Response {
			return f.agentMove(t, issueWriters[0], "need_decision")
		},
	}
	for name, move := range callers {
		t.Run(name+"/flag true keeps the target", func(t *testing.T) {
			f := newEpisodeFixture(t, false)
			f.suspendedEpisode(t, true)
			dbfx.Exec(t, `UPDATE issue SET status = 'blocked' WHERE id = $1`, f.issueID)
			before := f.onlyEpisode(t)
			move(t, f).Want(http.StatusOK)
			row := f.onlyEpisode(t)
			if row.id != before.id || row.phase != "waiting" || row.suspendReason != "" || !row.restoreOnResume || row.restoreStatus != "in_progress" || row.humanID != f.humanID {
				t.Fatalf("episode = %+v, want the same episode waiting again with the saved target", row)
			}
			if status, assigneeType, assigneeID := issueStatusAndAssignee(t, f.issueID); status != "need_decision" || assigneeType != "member" || assigneeID != f.humanID {
				t.Fatalf("issue = %s %s/%s, want need_decision held by %s", status, assigneeType, assigneeID, f.humanID)
			}
		})
		t.Run(name+"/flag false takes the current placement", func(t *testing.T) {
			f := newEpisodeFixture(t, false)
			f.suspendedEpisode(t, false)
			other := hicMember(t, "member")
			dbfx.Exec(t, `UPDATE issue SET status = 'todo', assignee_type = 'member', assignee_id = $2 WHERE id = $1`, f.issueID, other)
			move(t, f).Want(http.StatusOK)
			row := f.onlyEpisode(t)
			if row.restoreStatus != "todo" || row.restoreAssigneeType != "member" || row.restoreAssigneeID != other || !row.restoreOnResume || row.restoreStatus == "need_decision" {
				t.Fatalf("restore target = %s %s/%s flag=%v, want the person's todo placement", row.restoreStatus, row.restoreAssigneeType, row.restoreAssigneeID, row.restoreOnResume)
			}
		})
	}
}

// A re-attach runs the same §4.3 step 3 checks as an entry, on the stored B.
func TestDecisionEpisodeReattachRefusals(t *testing.T) {
	bindSecond := func(t *testing.T, f episodeFixture) string {
		second := f.secondRequest(t)
		dbfx.Exec(t, `UPDATE decision_wait SET request_ids = array_append(request_ids, $2::uuid) WHERE issue_id = $1`, f.issueID, second)
		return second
	}
	cases := []struct {
		name     string
		group    bool
		fragment string
		arrange  func(t *testing.T, f episodeFixture)
	}{
		{"open request with an answered sibling", true, "answer is already in", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE decision_request SET human_user_id = $2, status = 'answered' WHERE id = $1`, bindSecond(t, f), f.humanID)
		}},
		{"open request with a sibling to another human", true, "one human", func(t *testing.T, f episodeFixture) { bindSecond(t, f) }},
		{"pending retraction in B", false, "retract", func(t *testing.T, f episodeFixture) { f.pendingRetraction(t, false) }},
		{"rejected lane delivery", false, "lane", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE chat_message SET failure_reason = 'rejected' WHERE id = (SELECT ping_message_id FROM decision_request WHERE id = $1)`, f.requestID)
		}},
		{"archived lane", false, "lane", func(t *testing.T, f episodeFixture) {
			dbfx.Exec(t, `UPDATE chat_session SET status = 'archived' WHERE id = (SELECT chat_session_id FROM decision_request WHERE id = $1)`, f.requestID)
		}},
	}
	for _, tc := range cases {
		for name, move := range map[string]func(t *testing.T, f episodeFixture) *testutil.Response{
			"member": func(t *testing.T, f episodeFixture) *testutil.Response { return f.memberMove(t, "need_decision") },
			"asking task": func(t *testing.T, f episodeFixture) *testutil.Response {
				return f.agentMove(t, issueWriters[0], "need_decision")
			},
		} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				f := newEpisodeFixture(t, tc.group)
				f.suspendedEpisode(t, true)
				f.unseal(t) // a sibling can only join an unsealed intent
				tc.arrange(t, f)
				dbfx.Exec(t, `UPDATE decision_intent SET sealed_at = now() WHERE id = $1`, f.intentID)
				dbfx.Exec(t, `UPDATE issue SET status = 'blocked' WHERE id = $1`, f.issueID)
				f.wantRefusal(t, move(t, f), tc.fragment)
				if rows := f.episodes(t); len(rows) != 1 || rows[0].phase != "suspended" {
					t.Fatalf("episodes = %+v, want the suspended one untouched", rows)
				}
				if status, _, _ := issueStatusAndAssignee(t, f.issueID); status != "blocked" {
					t.Fatalf("issue status = %s, want blocked (unchanged)", status)
				}
			})
		}
	}
}

func TestDecisionEpisodeStatusMovedSuspendsAndReattaches(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.agentMove(t, issueWriters[0], "need_decision").Want(http.StatusOK)
	first := f.onlyEpisode(t)

	// An assignee change alone changes nothing.
	issueWriters[0].write(t, f.issueID, map[string]any{"assignee_type": "member", "assignee_id": f.humanID}, nil).Want(http.StatusOK)
	if row := f.onlyEpisode(t); row.phase != "waiting" || !row.restoreOnResume {
		t.Fatalf("assignee-only write changed the episode: %+v", row)
	}

	f.memberMove(t, "todo").Want(http.StatusOK)
	row := f.onlyEpisode(t)
	if row.id != first.id || row.phase != "suspended" || row.suspendReason != "status_moved" || row.restoreOnResume {
		t.Fatalf("episode = %+v, want suspended/status_moved with the restore flag false", row)
	}

	f.agentMove(t, issueWriters[0], "need_decision").Want(http.StatusOK)
	row = f.onlyEpisode(t)
	if row.id != first.id || row.phase != "waiting" || !row.restoreOnResume || row.restoreStatus != "todo" {
		t.Fatalf("episode = %+v, want the same episode re-attached with the person's todo as target", row)
	}
}

func TestDecisionEpisodeClosedStatusMoveClearsFlagWithoutSuspending(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.agentMove(t, issueWriters[0], "need_decision").Want(http.StatusOK)
	f.memberMove(t, "done").Want(http.StatusOK)
	if row := f.onlyEpisode(t); row.phase != "waiting" || row.restoreOnResume {
		t.Fatalf("episode = %+v, want waiting with the restore flag false", row)
	}
}
