package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2895: a retry claim restamps old human input across the segment floor.
// When the ask card is not yet visible to that claim (sibling-DM ask still
// uncommitted), the restamp lands the old input after the ask, where "after the
// ping" would pass it as a fresh answer. A restamped message is never a source.
func TestAskedDecisionRecordRefusesReanchoredSource(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	// Old input owned by the retry's root task, older than the ask.
	dbfx.Exec(test, `UPDATE chat_message SET created_at = 'epoch', task_id = $1 WHERE id = $2`, fixture.chatTaskID, fixture.messageID)
	dbfx.Exec(test, `UPDATE chat_session SET segment_floor_at = clock_timestamp(), segment_floor_id = gen_random_uuid() WHERE id = $1`, fixture.sessionID)
	// The ask is not visible to the claim's snapshot: hide it, restamp, restore.
	dbfx.Exec(test, `UPDATE decision_request SET ping_message_id = NULL WHERE id = $1`, fixture.decision.ID)
	if err := testHandler.Queries.ReanchorChatInputAcrossFloor(context.Background(), db.ReanchorChatInputAcrossFloorParams{
		ChatSessionID: parseUUID(fixture.sessionID),
		InputTaskID:   parseUUID(fixture.chatTaskID),
		DispatchedAt:  pgtype.Timestamptz{Time: fixture.decision.CreatedAt.Time, Valid: true},
	}); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE decision_request SET ping_message_id = $1 WHERE id = $2`, fixture.decision.PingMessageID, fixture.decision.ID)

	result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || refusal.status != http.StatusBadRequest || result.Answer.ID.Valid {
		test.Fatalf("want 400 refusal with no answer, got %+v / %v", result, err)
	}
	fixture.assertRecordCounts(test, 0)
}
