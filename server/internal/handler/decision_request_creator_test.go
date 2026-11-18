package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// creatorCardFixture is a held root issue Y that a running task T created, with
// no card on it. T runs on another issue X (the single-call fixture): the intake
// run of CHE-6911, which creates the issue and opens the card in one run.
type creatorCardFixture struct {
	singleCallFixture
	hold     holdFixture
	issueY   string
	commentY string
}

// exemptAllEscalationClasses makes the hold entry name every class a card may carry.
func exemptAllEscalationClasses(entry map[string]any) {
	entry["exempt_escalation_classes"] = []string{"spend", "deploy", "publish", "irreversible", "product_direction"}
}

func newCreatorCardFixture(test *testing.T) creatorCardFixture {
	test.Helper()
	fixture := creatorCardFixture{singleCallFixture: newSingleCallFixture(test), hold: newHoldFixture(test, exemptAllEscalationClasses)}
	fixture.issueY = fixture.createdBy(test, fixture.taskID, "agent_create")
	return fixture
}

// createdBy files a held backlog issue that names the task as its origin.
func (fixture *creatorCardFixture) createdBy(test *testing.T, taskID, originType string) string {
	test.Helper()
	issueID := fixture.hold.issue(test, "feat(intake): new request", testutil.Cols{
		"assignee_type": "agent", "assignee_id": fixture.agentID, "origin_type": originType, "origin_id": taskID,
	})
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(test, `DELETE FROM decision_request WHERE issue_id = $1`, issueID)
	fixture.commentY = dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": issueID, "author_type": "agent", "author_id": fixture.agentID, "source_task_id": taskID, "content": "Intake summary."})
	return issueID
}

// request is the card of the given class on issue Y, sent by the task.
func (fixture creatorCardFixture) request(taskID, issueID, class string) *http.Request {
	body := fixture.body()
	body["issue_id"], body["escalation_class"] = issueID, class
	ask := fixture.details
	ask.SourceIssueID, ask.SourceCommentID = issueID, fixture.commentY
	body["ask"] = ask
	request := fixture.call(body)
	request.Header.Set("X-Task-ID", taskID)
	return request
}

func (fixture creatorCardFixture) requests(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, fixture.issueY)
}

// answerAndWake plays the person's answer: held for ten minutes (Y stays held,
// with exempt_after), then effective at wake_at (Y is not held), then delivered by DeliverDecisionWake.
func (fixture creatorCardFixture) answerAndWake(test *testing.T, requestID string) {
	test.Helper()
	// The person answers. The answer is on hold for ten minutes: Y stays held.
	dbfx.Exec(test, `UPDATE decision_request SET status = 'answered' WHERE id = $1`, requestID)
	dbfx.Insert(test, "decision_answer", testutil.Cols{
		"decision_request_id": requestID, "answer_source": "card", "answered_by_user_id": fixture.humanID,
		"chat_message_ids": testutil.Raw("'{}'::uuid[]"), "content_snapshot": "yes", "issue_comment_id": fixture.commentY,
		"effective_at": testutil.Raw("now() + interval '10 minutes'"), "wake_at": testutil.Raw("now() + interval '10 minutes'"),
	})
	if field := wantHold(test, fixture.issueY, true, true, "held"); field["exempt_after"] == nil {
		test.Fatalf("held answer has no exempt_after: %v", field)
	}

	// At wake_at the answer is effective: Y is not held and one run starts on it.
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE decision_request_id = $1`, requestID)
	dbfx.Cleanup(test, `UPDATE agent_task_queue SET status = 'cancelled' WHERE issue_id = $1 AND status IN ('queued', 'running')`, fixture.issueY)
	wantHold(test, fixture.issueY, false, false, "exempt_escalation_class")
	if err := testHandler.DeliverDecisionWake(context.Background(), parseUUID(testWorkspaceID), parseUUID(requestID)); err != nil {
		test.Fatal(err)
	}
	if runs := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, fixture.issueY); runs != 1 {
		test.Fatalf("runs on Y after the answer took effect = %d, want 1", runs)
	}
}

// Criterion 12: the run that created the issue opens the card on it, and the
// issue stays held until the answer's wake_at.
func TestDecisionRequestFromCreatingTaskOfEscalationClass(test *testing.T) {
	for _, class := range []string{"spend", "deploy", "publish", "irreversible", "product_direction"} {
		test.Run(class, func(test *testing.T) {
			fixture := newCreatorCardFixture(test)
			wantHold(test, fixture.issueY, true, true, "held")
			var created struct {
				ID string `json:"id"`
			}
			testutil.Call(test, testHandler.RequestDecision, fixture.request(fixture.taskID, fixture.issueY, class)).Want(http.StatusCreated).JSON(&created)
			if fixture.requests(test) != 1 || dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE message_kind = 'decision_request' AND content LIKE '%' || $1 || '%'`, fixture.issueY) < 1 {
				test.Fatal("the card was not stored and delivered")
			}
			wantHold(test, fixture.issueY, true, true, "held")

			fixture.answerAndWake(test, created.ID)
		})
	}
}

// The DM task already asks on any issue of its person's DM. Criterion 12 records
// where the answer goes: the card renders in the DM, the issue stays in backlog
// (the DM task is not the issue's, so there is no park), and the issue is held
// until wake_at.
func TestDecisionRequestFromDMTaskOnCreatedIssue(test *testing.T) {
	for _, class := range []string{"spend", "deploy", "publish", "irreversible", "product_direction"} {
		test.Run(class, func(test *testing.T) {
			dm := newDMDecisionFixture(test)
			fixture := creatorCardFixture{singleCallFixture: dm.singleCallFixture, hold: newHoldFixture(test, exemptAllEscalationClasses)}
			// The DM task is the task that creates Y.
			fixture.issueY = fixture.createdBy(test, dm.chatTaskID, "agent_create")
			body := fixture.body()
			delete(body, "for")
			delete(body, "footer")
			body["issue_id"], body["escalation_class"] = fixture.issueY, class
			ask := fixture.details
			ask.SourceIssueID, ask.SourceCommentID = fixture.issueY, fixture.commentY
			body["ask"] = ask
			request := dm.call(body)
			request.Header.Set("X-Task-ID", dm.chatTaskID)
			var created struct {
				ID            string `json:"id"`
				ChatSessionID string `json:"chat_session_id"`
			}
			testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusCreated).JSON(&created)
			if created.ChatSessionID != dm.sessionID {
				test.Fatalf("card delivered in session %s, want the calling DM %s", created.ChatSessionID, dm.sessionID)
			}
			wantBacklogNoPark := func() {
				test.Helper()
				var status string
				dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, fixture.issueY).Scan(&status)
				if status != "backlog" || dbfx.Count(test, `SELECT count(*) FROM issue_decision_park WHERE issue_id = $1`, fixture.issueY) != 0 {
					test.Fatalf("issue status = %q, want backlog and no park row (a DM task is not the issue's task)", status)
				}
			}
			wantBacklogNoPark()
			wantHold(test, fixture.issueY, true, true, "held")

			// Answer, wake_at, delivery, derived run: the same steps as the creating task.
			fixture.answerAndWake(test, created.ID)

			// Where the answer goes: not back to the DM. The answer wakes the
			// issue's assignee on Y with a new run, and records that outcome.
			if got := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND wake_outcome = 'run_enqueued' AND woken_at IS NOT NULL`, created.ID); got != 1 {
				test.Fatalf("answers recorded as run_enqueued = %d, want 1", got)
			}
			if got := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued' AND agent_id = $2 AND chat_session_id IS NULL`, fixture.issueY, fixture.agentID); got != 1 {
				test.Fatalf("queued run for the assignee on Y (not in the DM) = %d, want 1", got)
			}
			wantBacklogNoPark()
		})
	}
}

// Criterion 12, refused: each case sends a card and gets 403 with no request stored.
func TestDecisionRequestOnCreatedIssueRefusals(test *testing.T) {
	test.Run("reversible class from the creating task", func(test *testing.T) {
		fixture := newCreatorCardFixture(test)
		testutil.Call(test, testHandler.RequestDecision, fixture.request(fixture.taskID, fixture.issueY, "reversible")).Want(http.StatusForbidden)
		if fixture.requests(test) != 0 {
			test.Fatal("a request was stored")
		}
	})
	test.Run("a running task that did not create the issue", func(test *testing.T) {
		fixture := newCreatorCardFixture(test)
		other := dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
		testutil.Call(test, testHandler.RequestDecision, fixture.request(other, fixture.issueY, "spend")).Want(http.StatusForbidden)
		if fixture.requests(test) != 0 {
			test.Fatal("a request was stored")
		}
	})
	test.Run("the creating task after it stopped running", func(test *testing.T) {
		fixture := newCreatorCardFixture(test)
		dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
		testutil.Call(test, testHandler.RequestDecision, fixture.request(fixture.taskID, fixture.issueY, "spend")).Want(http.StatusForbidden)
		if fixture.requests(test) != 0 {
			test.Fatal("a request was stored")
		}
	})
	test.Run("an issue whose origin is another task", func(test *testing.T) {
		fixture := newCreatorCardFixture(test)
		other := dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
		issueID := fixture.createdBy(test, other, "agent_create")
		testutil.Call(test, testHandler.RequestDecision, fixture.request(fixture.taskID, issueID, "spend")).Want(http.StatusForbidden)
		if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, issueID) != 0 {
			test.Fatal("a request was stored")
		}
	})
	test.Run("an issue whose origin is not agent_create", func(test *testing.T) {
		fixture := newCreatorCardFixture(test)
		issueID := fixture.createdBy(test, fixture.taskID, "quick_create")
		testutil.Call(test, testHandler.RequestDecision, fixture.request(fixture.taskID, issueID, "spend")).Want(http.StatusForbidden)
		if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, issueID) != 0 {
			test.Fatal("a request was stored")
		}
	})
}
