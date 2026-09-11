package service

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// TestPublishThreadUnresolvedOnReply_EmitsOneEventPerClearedRow is the round
// 6 review's required regression: commentguard.LockThreadForReplyAndClearResolution
// returns every row ClearThreadResolutionForReply cleared (fixed in
// 46ed4ce8d, proven at the helper/database layer by
// TestLockThreadForReplyAndClearResolution_ReturnsAllClearedRows), but that
// test never drove the result through the actual publisher. A regression
// where PublishThreadUnresolvedOnReply silently published only cleared[0]
// (e.g. a stray "cleared[:1]" or an early return) would pass every existing
// test and still under-report every multi-row clear to realtime listeners.
//
// This test exercises the real publisher (no DB required — it is a pure
// function of the []db.Comment slice LockThreadForReplyAndClearResolution
// already returns) against a real *events.Bus, using the same two-row
// fixture shape as TestLockThreadForReplyAndClearResolution_ReturnsAllClearedRows.
// It asserts exactly one comment:unresolved event per cleared comment ID —
// no missing row, no duplicate, and (via SubscribeAll) no unrelated event
// type — which is the contract the round 6 correction packet named.
func TestPublishThreadUnresolvedOnReply_EmitsOneEventPerClearedRow(t *testing.T) {
	bus := events.New()
	svc := &TaskService{Bus: bus}

	workspaceID := util.MustParseUUID("11111111-1111-4111-8111-111111111111")
	issueID := util.MustParseUUID("22222222-2222-4222-8222-222222222222")
	replyAID := util.MustParseUUID("33333333-3333-4333-8333-333333333333")
	replyBID := util.MustParseUUID("44444444-4444-4444-8444-444444444444")

	cleared := []db.Comment{
		{ID: replyAID, IssueID: issueID, WorkspaceID: workspaceID, AuthorType: "member", Content: "reply a"},
		{ID: replyBID, IssueID: issueID, WorkspaceID: workspaceID, AuthorType: "member", Content: "reply b"},
	}

	var allEvents []events.Event
	bus.SubscribeAll(func(e events.Event) { allEvents = append(allEvents, e) })

	svc.PublishThreadUnresolvedOnReply(cleared, util.UUIDToString(workspaceID), "system", "")

	if len(allEvents) != len(cleared) {
		t.Fatalf("published %d events for %d cleared rows, want exactly %d — got types: %v",
			len(allEvents), len(cleared), len(cleared), eventTypes(allEvents))
	}

	seen := map[string]int{}
	for _, e := range allEvents {
		if e.Type != protocol.EventCommentUnresolved {
			t.Fatalf("unrelated event type published: %s, want only %s", e.Type, protocol.EventCommentUnresolved)
		}
		if e.WorkspaceID != util.UUIDToString(workspaceID) {
			t.Fatalf("event workspace_id = %s, want %s", e.WorkspaceID, util.UUIDToString(workspaceID))
		}
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			t.Fatalf("event payload not a map[string]any: %+v", e.Payload)
		}
		commentField, ok := payload["comment"].(map[string]any)
		if !ok {
			t.Fatalf("event payload missing comment field: %+v", payload)
		}
		id, ok := commentField["id"].(string)
		if !ok {
			t.Fatalf("event comment.id not a string: %+v", commentField)
		}
		seen[id]++
	}

	for _, want := range []string{util.UUIDToString(replyAID), util.UUIDToString(replyBID)} {
		switch seen[want] {
		case 0:
			t.Fatalf("no comment:unresolved event published for cleared row %s — exactly the row the old rows[0]-only code silently dropped", want)
		case 1:
			// exactly right
		default:
			t.Fatalf("comment:unresolved published %d times for cleared row %s, want exactly 1", seen[want], want)
		}
	}
}

// TestPublishThreadUnresolvedOnReply_EmptyClearedIsNoOp confirms the
// no-clear case (a reply landing in an already-unresolved thread) publishes
// nothing, so the "no ... unrelated event" half of the round 6 requirement
// also holds for the common zero-row path, not only the multi-row one.
func TestPublishThreadUnresolvedOnReply_EmptyClearedIsNoOp(t *testing.T) {
	bus := events.New()
	svc := &TaskService{Bus: bus}

	var published int
	bus.SubscribeAll(func(events.Event) { published++ })

	svc.PublishThreadUnresolvedOnReply(nil, "11111111-1111-4111-8111-111111111111", "system", "")

	if published != 0 {
		t.Fatalf("published %d events for an empty cleared slice, want 0", published)
	}
}

func eventTypes(evs []events.Event) []string {
	types := make([]string, len(evs))
	for i, e := range evs {
		types[i] = e.Type
	}
	return types
}
