package handler

import (
	"testing"
)

// CHE-7535 / CHE-7278 S1: a delivered receipt in upstream's chat_task_supplement
// shape, which migration 999q00 and its copy hook leave behind, must still
// authorize a decision record for the run it was delivered to. The fixture
// seeds exactly one running chat task, so it does not hit the
// one-active-task-per-chat index that quarantines TestSteeredMessageDecisionRecord
// (CHE-4909). The copy itself is covered in cmd/migrate.
func TestConvertedDeliveredSupplementAuthorizesRunningTaskRecord(t *testing.T) {
	fixture := newAskedRecordFixture(t)
	fixture.steerMessage(t, fixture.messageID, fixture.chatTaskID, "delivered")
	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil || result.Answer.ContentSnapshot != "  Yes.\nKeep the exact words.  " {
		t.Fatalf("converted delivered receipt refused or rewritten: %+v / %v", result.Answer, err)
	}
	fixture.assertRecordCounts(t, 1)
}
