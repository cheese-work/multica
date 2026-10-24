package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-1874 PR 2b: the DM's member lists and cancels pending wakeups, and the
// answer follows a continuation: a wakeup set in the old DM is still pending in
// the new one.

func (dm wakeupDM) memberList(t *testing.T, sessionID string) []wakeupJSON {
	t.Helper()
	req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+sessionID+"/wakeups", nil), "sessionId", sessionID))
	var out struct {
		Wakeups []wakeupJSON `json:"wakeups"`
	}
	testutil.Call(t, testHandler.ListChatSessionWakeups, req).Want(http.StatusOK).JSON(&out)
	return out.Wakeups
}

func (dm wakeupDM) memberCancel(t *testing.T, sessionID, wakeupID string) *testutil.Response {
	t.Helper()
	req := withChatTestWorkspaceCtx(t, withURLParams(
		newRequest(http.MethodDelete, "/api/chat/sessions/"+sessionID+"/wakeups/"+wakeupID, nil), "sessionId", sessionID, "wakeupId", wakeupID))
	return testutil.Call(t, testHandler.CancelChatSessionWakeup, req)
}

func TestMemberListsPendingWakeupsOfTheDMAndItsLineage(t *testing.T) {
	dm := newWakeupDM(t)
	kept := dm.create(t, "Still pending.", time.Hour)
	cancelled := dm.create(t, "Cancelled one.", 2*time.Hour)
	fired := dm.create(t, "Already fired.", 3*time.Hour)
	dm.memberCancel(t, dm.sessionID, cancelled.ID).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE chat_wakeup SET status = 'fired' WHERE id = $1`, fired.ID)

	got := dm.memberList(t, dm.sessionID)
	if len(got) != 1 || got[0].ID != kept.ID {
		t.Fatalf("list = %+v, want only the pending wakeup %s", got, kept.ID)
	}

	next := dm.continued(t)
	if got := dm.memberList(t, next.sessionID); len(got) != 1 || got[0].ID != kept.ID {
		t.Fatalf("successor list = %+v, want the inherited wakeup %s", got, kept.ID)
	}
}

func TestMemberCanOnlyReadTheirOwnDMsWakeups(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "Mine.", time.Hour)
	other := dbfx.User(t, "Other member", "wakeup-other-"+dm.sessionID+"@example.test")
	dbfx.Member(t, testWorkspaceID, other, "member")
	asOther := func(method, path string) *http.Request {
		req := newRequestAs(other, method, path, nil)
		return withChatTestWorkspaceCtx(t, withURLParams(req, "sessionId", dm.sessionID, "wakeupId", w.ID))
	}
	testutil.Call(t, testHandler.ListChatSessionWakeups, asOther(http.MethodGet, "/api/chat/sessions/"+dm.sessionID+"/wakeups")).Want(http.StatusForbidden)
	testutil.Call(t, testHandler.CancelChatSessionWakeup, asOther(http.MethodDelete, "/api/chat/sessions/"+dm.sessionID+"/wakeups/"+w.ID)).Want(http.StatusForbidden)
	if status, _ := wakeupStatus(t, w.ID); status != "pending" {
		t.Fatalf("status = %s, want pending after the refused cancel", status)
	}
}

func TestMemberCancelStopsTheWakeupIncludingAnInheritedOne(t *testing.T) {
	dm := newWakeupDM(t)
	own := dm.create(t, "Cancel from the same DM.", time.Hour)
	inherited := dm.create(t, "Cancel from the successor.", time.Hour)

	dm.memberCancel(t, dm.sessionID, own.ID).Want(http.StatusOK)
	dm.memberCancel(t, dm.sessionID, own.ID).Want(http.StatusNotFound) // no longer pending

	next := dm.continued(t)
	dm.memberCancel(t, next.sessionID, inherited.ID).Want(http.StatusOK)
	for _, id := range []string{own.ID, inherited.ID} {
		if status, _ := wakeupStatus(t, id); status != "cancelled" {
			t.Fatalf("wakeup %s status = %s, want cancelled", id, status)
		}
		dm.makeDue(t, id)
	}
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("cancelled wakeup fired in the old DM: %v", got)
	}
	if got := dm.wakeupTasks(t, next.sessionID); len(got) != 0 {
		t.Fatalf("cancelled wakeup fired in the new DM: %v", got)
	}

	// Another DM's wakeup id is not found, even for the same member.
	foreign := dm.otherDM(t)
	theirs := foreign.create(t, "Not in this lineage.", time.Hour)
	dm.memberCancel(t, dm.sessionID, theirs.ID).Want(http.StatusNotFound)
}

// A conversation can be continued many times. The lineage of a DM is not capped
// at a fixed number of ancestors, or an old pending wakeup would drop out of the
// list, the cancel scope and the pending cap.
func TestMemberSeesAnInheritedWakeupAcrossALongContinuationChain(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "Set in the oldest DM.", time.Hour)

	previous := dm.sessionID
	for i := 0; i < 20; i++ {
		var next string
		dbfx.QueryRow(t, `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, explicitly_created_at, continued_from_session_id)
			VALUES ($1, $2, $3, 'Continuation', now(), $4) RETURNING id::text`, testWorkspaceID, dm.agentID, testUserID, previous).Scan(&next)
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, next) })
		previous = next
	}

	got := dm.memberList(t, previous)
	if len(got) != 1 || got[0].ID != w.ID {
		t.Fatalf("the newest DM of a 21-DM chain lists %+v, want the wakeup %s from the oldest", got, w.ID)
	}
	dm.memberCancel(t, previous, w.ID).Want(http.StatusOK)
}
