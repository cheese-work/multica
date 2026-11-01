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

// Route level: a PAT-authored source comment and a non-reversible DM record both fail;
// the reversible record keeps working.
func TestDecisionRecordRouteCardOnly(test *testing.T) {
	for _, mutation := range []string{"pat_source_comment", "deploy_unprompted", "deploy_asked", "reversible_asked"} {
		test.Run(mutation, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			request := fixture.recordRequest()
			body := `{"decision_record":true,"message_ids":["` + fixture.messageID + `"],"parent_id":"` + fixture.parentID + `"}`
			wanted := http.StatusForbidden
			switch mutation {
			case "pat_source_comment":
				sourceID := dbfx.Insert(test, "comment", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "author_type": "member", "author_id": fixture.humanID, "content": "Go."})
				body = `{"decision_record":true,"source_comment_id":"` + sourceID + `","parent_id":"` + fixture.parentID + `"}`
				request.Header.Set("X-Actor-Source", "pat")
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
				fixture.assertRecordCounts(test, 0)
			}
		})
	}
}
