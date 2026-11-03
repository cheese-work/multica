package handler

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// enter moves the fixture's issue into need_decision as the asking task does,
// then ends that task so the continuation is the only agent run left to count.
func (f episodeFixture) enter(t *testing.T) {
	t.Helper()
	f.agentMove(t, issueWriters[0], "need_decision").Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, f.taskID)
}

// answerRequest records a live answer on requestID. A card answer (nonReversible)
// is what a non-reversible class needs; wakeIn is the hold left before delivery.
func (f episodeFixture) answerRequest(t *testing.T, requestID string, wakeIn time.Duration, nonReversible bool) (answerID, commentID string) {
	t.Helper()
	commentID = dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": f.issueID, "author_type": "agent", "author_id": f.agentID, "content": "Answer " + requestID[:8]})
	var sessionID, pingID string
	dbfx.QueryRow(t, `SELECT chat_session_id::text, ping_message_id::text FROM decision_request WHERE id = $1`, requestID).Scan(&sessionID, &pingID)
	cols := testutil.Cols{"decision_request_id": requestID, "content_snapshot": "Yes", "effective_at": time.Now().Add(-time.Minute),
		"wake_at": time.Now().Add(wakeIn), "issue_comment_id": commentID}
	if nonReversible {
		dbfx.Exec(t, `UPDATE decision_request SET escalation_class = 'irreversible' WHERE id = $1`, requestID)
		cols["answer_source"], cols["answered_by_user_id"], cols["chat_message_ids"] = "card", f.humanID, "{}"
	} else {
		cols["answered_by_task_id"], cols["source_chat_session_id"], cols["chat_message_ids"] = f.taskID, sessionID, "{"+pingID+"}"
	}
	answerID = dbfx.Insert(t, "decision_answer", cols)
	dbfx.Exec(t, `UPDATE decision_request SET status = 'answered' WHERE id = $1`, requestID)
	return answerID, commentID
}

// cancelRequest ends requestID the way the lifecycle or a withdrawal would.
func (f episodeFixture) cancelRequest(t *testing.T, requestID, reason string) (commentID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE decision_request SET status = 'cancelled', cancel_reason = $2, cancel_notice_posted_at = now() WHERE id = $1`, requestID, reason)
	return dbfx.Insert(t, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": f.issueID, "author_type": "agent", "author_id": f.agentID,
		"content": "Decision request cancelled: " + reason, "decision_request_id": requestID, "decision_comment_kind": "cancel"})
}

// secondIntentRequest adds a request in its own intent, before entry.
func (f episodeFixture) secondIntentRequest(t *testing.T) string {
	t.Helper()
	intentID, _ := openLaneIntent(t, f.intentFixture, f.mention())
	input := f.input()
	input.SessionID = parseUUID(laneNamingIssue(t, f.issueID, f.agentID, f.humanID, "Second lane"))
	input.Ask = "Second, separate question?"
	second, err := testHandler.createDecisionRequest(f.request(), f.issue, input)
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE issue SET status = 'in_progress', assignee_type = 'agent', assignee_id = $2, metadata = metadata - 'decision_park' WHERE id = $1`, f.issueID, f.agentID)
	dbfx.Exec(t, `UPDATE decision_intent SET sealed_at = NULL WHERE id = $1`, intentID)
	return uuidToString(second.Request.ID)
}

type episodeClose struct {
	reason, outcome, resumeTaskID string
}

func (f episodeFixture) closedEpisodes(t *testing.T) []episodeClose {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT COALESCE(close_reason, ''), COALESCE(resume_outcome, ''), COALESCE(resume_task_id::text, '')
		FROM decision_wait WHERE issue_id = $1 AND closed_at IS NOT NULL ORDER BY closed_at`, f.issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []episodeClose
	for rows.Next() {
		var row episodeClose
		if err := rows.Scan(&row.reason, &row.outcome, &row.resumeTaskID); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

func (f episodeFixture) continuations(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND id <> $3`, f.issueID, f.agentID, f.taskID)
}

// wantOneContinuation asserts the episode closed once with reason and that the
// asking agent has exactly one run on the issue, which resume_task_id names.
func (f episodeFixture) wantOneContinuation(t *testing.T, reason string) db.AgentTaskQueue {
	t.Helper()
	closed := f.closedEpisodes(t)
	if len(closed) != 1 || closed[0].reason != reason || closed[0].outcome != "run_enqueued" {
		t.Fatalf("closed episodes = %+v, want one closed %q with run_enqueued", closed, reason)
	}
	if got := f.continuations(t); got != 1 {
		t.Fatalf("continuation runs = %d, want 1", got)
	}
	var task db.AgentTaskQueue
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(closed[0].resumeTaskID))
	if err != nil {
		t.Fatalf("resume_task_id %q: %v", closed[0].resumeTaskID, err)
	}
	if uuidToString(task.AgentID) != f.agentID || uuidToString(task.IssueID) != f.issueID {
		t.Fatalf("resume task = agent %s issue %s, want the asking agent on the issue", uuidToString(task.AgentID), uuidToString(task.IssueID))
	}
	return task
}

func (f episodeFixture) answerState(t *testing.T, answerID string) (woken bool, outcome string) {
	t.Helper()
	var wokenAt *time.Time
	var out *string
	dbfx.QueryRow(t, `SELECT woken_at, wake_outcome FROM decision_answer WHERE id = $1`, answerID).Scan(&wokenAt, &out)
	if out != nil {
		outcome = *out
	}
	return wokenAt != nil, outcome
}

func newDeliveryRunner(t *testing.T) decisionRunner {
	t.Helper()
	resetDecisionLease(t)
	_, runner := newDecisionRunnerFixture(t)
	return runner
}

func TestDecisionEpisodeDeliveryReversibleWake(t *testing.T) {
	for _, withSquad := range []bool{false, true} {
		name := "agent context"
		if withSquad {
			name = "squad context"
		}
		t.Run(name, func(t *testing.T) {
			runner := newDeliveryRunner(t)
			f := newEpisodeFixture(t, false)
			f.enter(t)
			var squadID string
			if withSquad {
				squadID = dbfx.Insert(t, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Resume squad", "creator_id": testUserID, "leader_id": f.agentID})
				dbfx.Exec(t, `UPDATE decision_wait SET resume_squad_id = $2 WHERE issue_id = $1 AND closed_at IS NULL`, f.issueID, squadID)
			}
			answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)

			decisionPass(t, runner)

			task := f.wantOneContinuation(t, "answered")
			if status, assigneeType, assigneeID := issueStatusAndAssignee(t, f.issueID); status != "in_progress" || assigneeType != "agent" || assigneeID != f.agentID {
				t.Fatalf("issue = %s %s/%s, want the restore placement in_progress agent/%s", status, assigneeType, assigneeID, f.agentID)
			}
			if got := uuidToString(task.SquadID); (got != "") != withSquad || (withSquad && got != squadID) || task.IsLeaderTask != withSquad {
				t.Fatalf("task squad = %q leader=%v, want the asking task's context %q", got, task.IsLeaderTask, squadID)
			}
			if task.WorkflowStep.String != "in_progress" {
				t.Fatalf("workflow_step = %q, want the restored status in_progress", task.WorkflowStep.String)
			}
			if woken, outcome := f.answerState(t, answerID); !woken || outcome != "run_enqueued" {
				t.Fatalf("answer woken=%v outcome=%q, want acknowledged run_enqueued", woken, outcome)
			}
			if dbfx.Count(t, `SELECT count(*) FROM decision_intent WHERE id = $1 AND group_woken_at IS NOT NULL`, f.intentID) != 1 {
				t.Fatal("the group acknowledgement was not written")
			}
			decisionPass(t, runner)
			if got := f.continuations(t); got != 1 {
				t.Fatalf("a second pass made %d continuation runs, want 1", got)
			}
		})
	}
}

func TestDecisionEpisodeDeliveryNonReversibleWaitsForTheHold(t *testing.T) {
	runner := newDeliveryRunner(t)
	f := newEpisodeFixture(t, false)
	f.enter(t)
	answerID, _ := f.answerRequest(t, f.requestID, time.Hour, true)

	decisionPass(t, runner)
	if status, assigneeType, _ := issueStatusAndAssignee(t, f.issueID); status != "need_decision" || assigneeType != "member" {
		t.Fatalf("issue = %s %s before wake_at, want need_decision held by the human", status, assigneeType)
	}
	if f.continuations(t) != 0 || len(f.closedEpisodes(t)) != 0 {
		t.Fatal("the episode delivered inside the hold")
	}
	if woken, _ := f.answerState(t, answerID); woken {
		t.Fatal("the answer was acknowledged inside the hold")
	}

	dbfx.Exec(t, `UPDATE decision_answer SET wake_at = clock_timestamp() - interval '1 second' WHERE id = $1`, answerID)
	decisionPass(t, runner)
	f.wantOneContinuation(t, "answered")
	decisionPass(t, runner)
	if got := f.continuations(t); got != 1 {
		t.Fatalf("continuation runs = %d, want exactly one after the hold", got)
	}
}

func TestDecisionEpisodeBoundSetWaitsForEveryRequest(t *testing.T) {
	runner := newDeliveryRunner(t)
	f := newEpisodeFixture(t, false)
	second := f.secondIntentRequest(t)
	f.unseal(t)
	f.enter(t)
	if len(f.onlyEpisode(t).requestIDs) != 2 {
		t.Fatalf("bound set = %v, want both requests", f.onlyEpisode(t).requestIDs)
	}
	firstAnswer, firstComment := f.answerRequest(t, f.requestID, -time.Minute, false)

	decisionPass(t, runner)
	if f.continuations(t) != 0 || len(f.closedEpisodes(t)) != 0 {
		t.Fatal("the episode delivered with one bound request still open")
	}
	if woken, _ := f.answerState(t, firstAnswer); woken {
		t.Fatal("the first answer was acknowledged before the set was complete")
	}
	for _, candidate := range wakeCandidates(t) {
		if candidate == f.requestID {
			t.Fatal("the existing wake still lists a bound answered request")
		}
	}

	secondAnswer, secondComment := f.answerRequest(t, second, -time.Minute, false)
	decisionPass(t, runner)
	task := f.wantOneContinuation(t, "answered")
	for _, answerID := range []string{firstAnswer, secondAnswer} {
		if woken, outcome := f.answerState(t, answerID); !woken || outcome != "run_enqueued" {
			t.Fatalf("answer %s woken=%v outcome=%q, want acknowledged", answerID, woken, outcome)
		}
	}
	inputs := uuidToString(task.TriggerCommentID) + " " + strings.Join(uuidStringsOrEmpty(task.CoalescedCommentIds), " ") + " " + task.HandoffNote.String
	for _, quote := range []string{firstComment, secondComment} {
		if !strings.Contains(inputs, quote) {
			t.Fatalf("continuation inputs %q miss the quote %s", inputs, quote)
		}
	}
}

func TestDecisionEpisodeAnswerAndReaskIsOneContinuation(t *testing.T) {
	runner := newDeliveryRunner(t)
	f := newEpisodeFixture(t, false)
	second := f.secondIntentRequest(t)
	f.unseal(t)
	f.enter(t)
	answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
	f.cancelRequest(t, second, "human_removed")

	decisionPass(t, runner)
	task := f.wantOneContinuation(t, "answered")
	if !strings.Contains(task.HandoffNote.String, second) {
		t.Fatalf("handoff %q does not name the re-asked request %s", task.HandoffNote.String, second)
	}
	if woken, _ := f.answerState(t, answerID); !woken {
		t.Fatal("answer not acknowledged")
	}
	if dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE id = $1 AND reask_delivered_at IS NOT NULL`, second) != 1 {
		t.Fatal("the re-ask was not acknowledged")
	}
	decisionPass(t, runner)
	if got := f.continuations(t); got != 1 {
		t.Fatalf("continuation runs = %d, want no second re-ask run", got)
	}
}

// A request outside B keeps its own wake and never resumes the episode.
func TestDecisionEpisodeIgnoresRequestsOutsideTheBoundSet(t *testing.T) {
	runner := newDeliveryRunner(t)
	f := newEpisodeFixture(t, false)
	outside := f.secondIntentRequest(t)
	f.unseal(t)
	f.enter(t)
	dbfx.Exec(t, `UPDATE decision_wait SET request_ids = ARRAY[$2::uuid], intent_ids = ARRAY[$3::uuid], group_ack_intent_ids = ARRAY[$3::uuid] WHERE issue_id = $1 AND closed_at IS NULL`, f.issueID, f.requestID, f.intentID)
	dbfx.Exec(t, `UPDATE decision_intent SET sealed_at = now() WHERE id = (SELECT decision_intent_id FROM decision_request WHERE id = $1)`, outside)
	answerID, _ := f.answerRequest(t, outside, -time.Minute, false)

	decisionPass(t, runner)
	if len(f.closedEpisodes(t)) != 0 || f.onlyEpisode(t).phase != "waiting" {
		t.Fatal("an answer outside B resumed the episode")
	}
	if woken, _ := f.answerState(t, answerID); !woken {
		t.Fatal("the request outside B lost its own wake")
	}
}

func wakeCandidates(t *testing.T) []string {
	t.Helper()
	rows, err := testHandler.Queries.ListDecisionWakeCandidates(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, row := range rows {
		ids = append(ids, uuidToString(row.ID))
	}
	return ids
}

// The three existing queries leave a bound request to the episode in both
// phases, and return it again once the episode is closed.
func TestDecisionEpisodeExcludesBoundRequestsFromTheExistingQueries(t *testing.T) {
	for _, phase := range []string{"waiting", "suspended"} {
		t.Run(phase, func(t *testing.T) {
			f := newEpisodeFixture(t, false)
			second := f.secondIntentRequest(t)
			f.unseal(t)
			f.enter(t)
			if phase == "suspended" {
				dbfx.Exec(t, `UPDATE decision_wait SET phase = 'suspended', suspend_reason = 'run_budget', suspended_at = now() WHERE issue_id = $1 AND closed_at IS NULL`, f.issueID)
			}
			f.answerRequest(t, f.requestID, -time.Minute, false)
			f.cancelRequest(t, second, "human_removed")
			f.pendingRetraction(t, false)

			ctx := context.Background()
			reasks, err := testHandler.Queries.ListDecisionReaskCandidates(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			retractions, err := testHandler.Queries.ListDecisionRetractionCandidates(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			var reaskIDs, retractionRequests []string
			for _, row := range reasks {
				reaskIDs = append(reaskIDs, uuidToString(row.ID))
			}
			for _, row := range retractions {
				retractionRequests = append(retractionRequests, uuidToString(row.DecisionRequestID))
			}
			if contains(wakeCandidates(t), f.requestID) || contains(reaskIDs, second) || contains(retractionRequests, f.requestID) {
				t.Fatalf("an existing query still returns a bound request: wake=%v reask=%v retraction=%v", wakeCandidates(t), reaskIDs, retractionRequests)
			}

			dbfx.Exec(t, `UPDATE decision_wait SET closed_at = now(), close_reason = 'answered' WHERE issue_id = $1`, f.issueID)
			reasks, _ = testHandler.Queries.ListDecisionReaskCandidates(ctx, 100)
			found := false
			for _, row := range reasks {
				found = found || uuidToString(row.ID) == second
			}
			if !contains(wakeCandidates(t), f.requestID) || !found {
				t.Fatal("a closed episode still hides its requests from the existing queries")
			}
		})
	}
}

// A wake loaded before the entry commits restarts and writes nothing.
func TestDecisionEpisodeWakeRacingEntryRestartsAndDoesNothing(t *testing.T) {
	f := newEpisodeFixture(t, false)
	answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
	ctx := context.Background()
	stale, err := testHandler.loadDecisionItem(ctx, testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(f.requestID), "")
	if err != nil {
		t.Fatal(err)
	}
	// A pending retraction would refuse the entry; the answer here is the wake under test.
	dbfx.Exec(t, `UPDATE decision_request SET status = 'open' WHERE id = $1`, f.requestID)
	f.enter(t)
	dbfx.Exec(t, `UPDATE decision_request SET status = 'answered' WHERE id = $1`, f.requestID)

	restart, err := testHandler.deliverDecisionWake(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if !restart {
		t.Fatal("a wake that raced the entry did not restart")
	}
	if woken, _ := f.answerState(t, answerID); woken || f.continuations(t) != 0 {
		t.Fatal("the raced wake wrote")
	}
	if err := testHandler.deliverDecisionItem(ctx, parseUUID(testWorkspaceID), parseUUID(f.requestID), ""); err != nil {
		t.Fatal(err)
	}
	if woken, _ := f.answerState(t, answerID); woken || f.continuations(t) != 0 {
		t.Fatal("the restarted wake found a binding episode and still wrote")
	}
}

// §6.1: every allowed terminal request state closes the episode with its
// mapped reason and gives one continuation. A value the table below does not
// know fails the test, so a later migration has to add its row.
var terminalMap = map[string]string{
	"answered/live":                   "answered",
	"answered/disclaimed":             "other_terminal",
	"disclaimed":                      "other_terminal",
	"cancelled/human_removed":         "reask",
	"cancelled/lane_archived":         "reask",
	"cancelled/expired":               "expired",
	"cancelled/agent_withdrawn/human": "human_withdrawn",
	"cancelled/agent_withdrawn/task":  "agent_withdrawn",
	"cancelled/issue_closed":          "system_cancelled",
	"cancelled/agent_archived":        "system_cancelled",
	"cancelled/record_disclaimed":     "other_terminal",
	"cancelled/record_vetoed":         "other_terminal",
	"cancelled/class_needs_owner":     "other_terminal",
	"cancelled/delegate_removed":      "other_terminal",
}

func checkValues(t *testing.T, table, column string) []string {
	t.Helper()
	var def string
	dbfx.QueryRow(t, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%' || $2 || ' = ANY%'`, table, column).Scan(&def)
	var out []string
	for _, match := range regexp.MustCompile(`'([a-z_]+)'::text`).FindAllStringSubmatch(def, -1) {
		out = append(out, match[1])
	}
	sort.Strings(out)
	return out
}

func TestDecisionEpisodeTerminalStateMapIsTotal(t *testing.T) {
	var cases []string
	for _, status := range checkValues(t, "decision_request", "status") {
		switch status {
		case "open":
		case "answered":
			cases = append(cases, "answered/live", "answered/disclaimed")
		case "cancelled":
			for _, reason := range checkValues(t, "decision_request", "cancel_reason") {
				if reason == "agent_withdrawn" {
					cases = append(cases, "cancelled/agent_withdrawn/human", "cancelled/agent_withdrawn/task")
				} else {
					cases = append(cases, "cancelled/"+reason)
				}
			}
		default:
			cases = append(cases, status)
		}
	}
	for _, key := range cases {
		want, ok := terminalMap[key]
		if !ok {
			t.Errorf("terminal state %q has no row in the §6.1 map", key)
			continue
		}
		t.Run(key, func(t *testing.T) {
			runner := newDeliveryRunner(t)
			f := newEpisodeFixture(t, false)
			f.enter(t)
			parts := strings.Split(key, "/")
			switch parts[0] {
			case "answered":
				answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
				if parts[1] == "disclaimed" {
					dbfx.Exec(t, `UPDATE decision_answer SET disclaimed_at = now(), disclaim_reason = 'human' WHERE id = $1`, answerID)
				}
			case "disclaimed":
				dbfx.Exec(t, `UPDATE decision_request SET status = 'disclaimed' WHERE id = $1`, f.requestID)
			case "cancelled":
				f.cancelRequest(t, f.requestID, parts[1])
				if parts[1] == "agent_withdrawn" && parts[2] == "human" {
					dbfx.Exec(t, `UPDATE decision_request SET withdrawn_by_user_id = $2 WHERE id = $1`, f.requestID, f.humanID)
				} else if parts[1] == "agent_withdrawn" {
					dbfx.Exec(t, `UPDATE decision_request SET withdrawn_by_task_id = $2 WHERE id = $1`, f.requestID, f.taskID)
				}
			}

			decisionPass(t, runner)
			closed := f.closedEpisodes(t)
			if len(closed) != 1 || closed[0].reason != want {
				t.Fatalf("closed episodes = %+v, want one closed %q", closed, want)
			}
			if got := f.continuations(t); got != 1 {
				t.Fatalf("continuation runs = %d, want 1", got)
			}
		})
	}
}

// An episode that cannot progress must not take a batch slot: more than one
// batch of closed-issue episodes, or of episodes with a refused dispatch, sorts
// ahead of a ready episode and the ready one is still selected.
func TestDecisionEpisodeCandidatesAreNotStarvedByStuckEpisodes(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.answerRequest(t, f.requestID, -time.Minute, false)
	var readyID string
	dbfx.QueryRow(t, `SELECT id::text FROM decision_wait WHERE issue_id = $1 AND closed_at IS NULL`, f.issueID).Scan(&readyID)

	// Low ids sort first. Each filler needs its own issue (one open episode per issue).
	// Fillers are test-owned and outlive the workspace cleanup, so they go before and after.
	const fillerIDs = `DELETE FROM decision_wait WHERE id::text LIKE '00000000-0000-0000-000_-%'`
	dbfx.Exec(t, fillerIDs)
	t.Cleanup(func() { dbfx.Exec(t, fillerIDs) })
	for i := 1; i <= decisionBatchSize+1; i++ {
		for kind, status := range map[string]string{"closed": "done", "refused": "in_progress"} {
			issueID := dbfx.Issue(t, "Filler "+kind, testutil.Cols{"status": status})
			prefix := "00000000-0000-0000-0000-"
			if kind == "refused" {
				prefix = "00000000-0000-0000-0001-"
			}
			dbfx.Exec(t, `INSERT INTO decision_wait (id, workspace_id, issue_id, asking_agent_id, human_user_id, request_ids, restore_status, dispatch_refused_since)
				SELECT $1::uuid, workspace_id, $2::uuid, asking_agent_id, human_user_id, '{}', 'in_progress',
					CASE WHEN $3::text = 'refused' THEN now() END
				FROM decision_wait WHERE id = $4::uuid`, fmt.Sprintf("%s%012d", prefix, i), issueID, kind, readyID)
		}
	}

	waits, err := testHandler.Queries.ListDecisionEpisodeCandidates(context.Background(), decisionBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, wait := range waits {
		found = found || uuidToString(wait.ID) == readyID
		if strings.HasPrefix(uuidToString(wait.ID), "00000000-0000-0000-0000-") {
			t.Fatalf("closed-issue episode %s was selected; it cannot progress before the system close", uuidToString(wait.ID))
		}
	}
	if !found {
		t.Fatalf("the ready episode is not among %d candidates behind %d stuck ones", len(waits), 2*(decisionBatchSize+1))
	}
}
