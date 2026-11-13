package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
)

var nonReversibleClasses = []string{"spend", "deploy", "publish", "irreversible", "product_direction"}

// Every class except reversible is answered on the card (CHE-2798 P5 (i), S2b).
func TestDecisionRecordRefusesNonReversibleClasses(test *testing.T) {
	for _, class := range nonReversibleClasses {
		test.Run(class+"/dm_message", func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			dbfx.Exec(test, `UPDATE decision_request SET escalation_class = $1 WHERE id = $2`, class, fixture.decision.ID)
			_, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || refusal.status != http.StatusForbidden {
				test.Fatalf("want 403 refusal, got %v", err)
			}
			if card := "chat?session=" + uuidToString(fixture.decision.ChatSessionID); !strings.Contains(refusal.message, card) {
				test.Fatalf("refusal lacks the card URL %q: %s", card, refusal.message)
			}
			fixture.assertRecordCounts(test, 0)
		})
		test.Run(class+"/source_comment", func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			dbfx.Exec(test, `UPDATE decision_request SET escalation_class = $1 WHERE id = $2`, class, fixture.decision.ID)
			sourceID := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "member", "author_id": fixture.humanID, "content": "Go."})
			dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, fixture.taskID)
			input := decisionRecordInput{ParentID: parseUUID(fixture.parentID), SourceCommentID: parseUUID(sourceID)}
			_, err := testHandler.createAskedDecisionRecord(fixture.request(), fixture.issue, input)
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || refusal.status != http.StatusForbidden || !strings.Contains(refusal.message, "chat?session=") {
				test.Fatalf("want 403 refusal with card URL, got %v", err)
			}
			fixture.assertRecordCounts(test, 0)
		})
		test.Run(class+"/unprompted", func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			input := fixture.recordInput()
			input.ParentID, input.EscalationClass = pgtype.UUID{}, class
			_, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || refusal.status != http.StatusForbidden || strings.Contains(refusal.message, "chat?session=") {
				test.Fatalf("want 403 refusal without a card URL, got %v", err)
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND origin = 'unprompted'`, fixture.issueID) != 0 {
				test.Fatal("refusal committed a request")
			}
		})
	}
}

// Route level: a source comment created over a PAT and recorded by a valid task token
// against a non-reversible ask is refused; the reversible asked record still works.
func TestDecisionRecordRouteCardOnly(test *testing.T) {
	for _, mutation := range []string{"pat_source_comment", "deploy_unprompted", "deploy_asked", "reversible_asked"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			request := fixture.recordRequest()
			body := `{"decision_record":true,"message_ids":["` + fixture.messageID + `"],"parent_id":"` + fixture.parentID + `"}`
			wanted := http.StatusForbidden
			switch mutation {
			case "pat_source_comment":
				dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'deploy' WHERE id = $1`, fixture.decision.ID)
				patRequest := newRequest(http.MethodPost, "/", map[string]any{"content": "Go."})
				patRequest.Header.Set("X-Actor-Source", "pat")
				patRequest.Header.Set("X-User-ID", fixture.humanID)
				var source CommentResponse
				testutil.Call(test, testHandler.CreateComment, withIssueParam(patRequest, fixture.issueID)).Want(http.StatusCreated).JSON(&source)
				dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, fixture.taskID)
				request = fixture.request()
				body = `{"decision_record":true,"source_comment_id":"` + source.ID + `","parent_id":"` + fixture.parentID + `"}`
			case "deploy_unprompted":
				body = `{"decision_record":true,"message_ids":["` + fixture.messageID + `"],"escalation_class":"deploy"}`
			case "deploy_asked":
				dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'deploy' WHERE id = $1`, fixture.decision.ID)
			case "reversible_asked":
				wanted = http.StatusCreated
			}
			request.Body = ioBody(body)
			response := httptest.NewRecorder()
			testHandler.CreateComment(response, withIssueParam(request, fixture.issueID))
			if response.Code != wanted {
				test.Fatalf("status=%d want=%d body=%s", response.Code, wanted, response.Body.String())
			}
			if wanted == http.StatusForbidden {
				if mutation != "deploy_unprompted" && !strings.Contains(response.Body.String(), "chat?session="+uuidToString(fixture.decision.ChatSessionID)) {
					test.Fatalf("refusal lacks the card URL: %s", response.Body.String())
				}
				fixture.assertRecordCounts(test, 0)
			}
		})
	}
}

// A retry of a record made while the request was reversible must not replay once the
// request is raised to a card-only class.
func TestDecisionRecordReplayRefusedAfterClassRaise(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	if _, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput()); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1`, fixture.decision.ID)
	_, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || refusal.status != http.StatusForbidden || !strings.Contains(refusal.message, "chat?session=") {
		test.Fatalf("replay after class raise must be refused with the card URL, got %v", err)
	}
}
