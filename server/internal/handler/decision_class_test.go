package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestDecisionClassRecordIntegration(test *testing.T) {
	for _, origin := range []string{"asked", "unprompted"} {
		for _, class := range []string{"spend", "deploy", "publish", "irreversible", "product_direction"} {
			test.Run(origin+"/"+class, func(test *testing.T) {
				fixture := newAskedRecordFixture(test)
				input := fixture.recordInput()
				if origin == "unprompted" {
					input.ParentID, input.EscalationClass = pgtype.UUID{}, "reversible"
				}
				record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
				if err != nil {
					test.Fatal(err)
				}
				request := fixture.recordRequest()
				request.Header.Set("X-Actor-Source", "interactive_jwt")
				request.Header.Set("X-User-ID", fixture.humanID)
				request.Body = ioBody(`{"escalation_class":"` + class + `"}`)
				response := httptest.NewRecorder()
				testHandler.RaiseDecisionClass(response, withIssueParam(request, uuidToString(record.Request.ID)))
				if response.Code != http.StatusOK {
					test.Fatalf("raise status=%d body=%s", response.Code, response.Body.String())
				}
				if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND wake_at IS NULL AND effective_at > clock_timestamp() + interval '9 minutes' AND content_snapshot = $2`, record.Answer.ID, record.Answer.ContentSnapshot) != 1 {
					test.Fatal("raise changed evidence or made a pending record effective")
				}
				result, err := testHandler.disclaimDecisionAnswerTx(request, fixture.issue.WorkspaceID, record.Answer.ID, parseUUID(fixture.humanID))
				if err != nil {
					test.Fatal(err)
				}
				wantedStatus := "open"
				if origin == "unprompted" {
					wantedStatus = "cancelled"
				}
				if result.Request.Status != wantedStatus || result.Request.EscalationClass != class || result.Answer.WakeAt.Valid || !result.Answer.DisclaimedAt.Valid || dbfx.Count(test, `SELECT count(*) FROM decision_class_change WHERE decision_request_id = $1 AND changed_by_user_id = $2`, record.Request.ID, fixture.humanID) != 1 {
					test.Fatalf("record/raise/disclaim integration failed: %+v", result)
				}
			})
		}
	}
}
