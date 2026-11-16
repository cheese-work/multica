package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// CHE-2819 L2a: an agent-token read sees only the current segment; old content
// reaches an agent only through the explicit segment read, on its own DM.

// segmentDM is a DM with two closed segments and a current one:
// seg1 = 5 OLD-* rows + divider SUMMARY-ONE, seg2 = SEG2-MSG + divider
// SUMMARY-TWO, current = CURRENT-MSG.
type segmentDM struct {
	carryOverDM
	d1, d2 string
	taskID string
}

func newSegmentDM(t *testing.T) segmentDM {
	t.Helper()
	dm := newCarryOverDM(t)
	dm.compact(t, "SUMMARY-ONE")
	dm.reply(t, "SEG2-MSG")
	dm.compact(t, "SUMMARY-TWO")
	dm.reply(t, "CURRENT-MSG")
	var d1, d2 string
	rows, err := testPool.Query(t.Context(), `SELECT id FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'compaction_summary' ORDER BY created_at, id`, dm.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for i, dst := range []*string{&d1, &d2} {
		if !rows.Next() {
			t.Fatalf("divider %d missing", i)
		}
		if err := rows.Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	return segmentDM{carryOverDM: dm, d1: d1, d2: d2, taskID: newChatHistoryTaskForSession(t, dm.sessionID)}
}

// get calls handler as the DM's own task token (task=true) or as the signed-in
// member, against the given session id.
func (dm segmentDM) get(t *testing.T, handler http.HandlerFunc, path, sessionID string, task bool) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/chat/sessions/" + sessionID + path
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	if task {
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Task-ID", dm.taskID)
	}
	req = withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", sessionID))
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func segTexts(t *testing.T, msgs []ChatMessageResponse) []string {
	t.Helper()
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Content
	}
	return out
}

func wantSegTexts(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("contents = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("contents = %q, want %q", got, want)
		}
	}
}

func TestAgentSegmentFloor_MessageReadsHideOldSegments(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newSegmentDM(t)

	var all []ChatMessageResponse
	w := dm.get(t, testHandler.ListChatMessages, "/messages", dm.sessionID, true)
	if w.Code != http.StatusOK {
		t.Fatalf("agent ListChatMessages = %d %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &all)
	wantSegTexts(t, segTexts(t, all), "CURRENT-MSG")

	var page ChatMessagesPageResponse
	w = dm.get(t, testHandler.ListChatMessagesPage, "/messages/page", dm.sessionID, true)
	if w.Code != http.StatusOK {
		t.Fatalf("agent page = %d %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &page)
	wantSegTexts(t, segTexts(t, page.Messages), "CURRENT-MSG")
	if page.HasMore {
		t.Fatal("page.HasMore = true: rows behind the floor must not count")
	}

	// A cursor at the floor returns an empty page, not the old segment.
	var floorAt, floorID string
	if err := testPool.QueryRow(t.Context(), `SELECT to_char(segment_floor_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), segment_floor_id FROM chat_session WHERE id = $1`, dm.sessionID).Scan(&floorAt, &floorID); err != nil {
		t.Fatal(err)
	}
	w = dm.get(t, testHandler.ListChatMessagesPage, "/messages/page?before_created_at="+floorAt+"&before_id="+floorID, dm.sessionID, true)
	page = ChatMessagesPageResponse{}
	_ = json.Unmarshal(w.Body.Bytes(), &page)
	if w.Code != http.StatusOK || len(page.Messages) != 0 {
		t.Fatalf("cursor at floor = %d %q, want empty page", w.Code, segTexts(t, page.Messages))
	}

	// An anchor in an old segment is a 404 for an agent.
	var oldID string
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM chat_message WHERE chat_session_id = $1 AND content = 'OLD-ANSWER-LAST'`, dm.sessionID).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	if w = dm.get(t, testHandler.ListChatMessages, "/messages?around="+oldID, dm.sessionID, true); w.Code != http.StatusNotFound {
		t.Fatalf("agent around old anchor = %d, want 404", w.Code)
	}

	// The signed-in member keeps the full transcript.
	all = nil
	w = dm.get(t, testHandler.ListChatMessages, "/messages", dm.sessionID, false)
	_ = json.Unmarshal(w.Body.Bytes(), &all)
	if len(all) != 9 {
		t.Fatalf("member sees %d messages, want all 9: %q", len(all), segTexts(t, all))
	}
}

func TestAgentSegmentFloor_ChatHistoryHidesOldSegments(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newSegmentDM(t)
	for path, handler := range map[string]http.HandlerFunc{
		"/api/chat/history?limit=50": testHandler.GetChatChannelHistory,
		"/api/chat/thread?limit=50":  testHandler.GetChatThread,
	} {
		w := httptest.NewRecorder()
		handler(w, historyReq(t, path, dm.taskID))
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, w.Code, w.Body.String())
		}
		var resp ChatChannelHistoryResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Messages) != 1 || resp.Messages[0].Text != "CURRENT-MSG" {
			t.Fatalf("%s messages = %+v, want only CURRENT-MSG", path, resp.Messages)
		}
	}
}

func TestAgentSegmentRead_ListsAndReadsOldSegments(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newSegmentDM(t)

	w := dm.get(t, testHandler.ListChatSegments, "/segments", dm.sessionID, true)
	if w.Code != http.StatusOK {
		t.Fatalf("segments = %d %s", w.Code, w.Body.String())
	}
	var list ChatSegmentsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Segments) != 2 ||
		list.Segments[0].DividerID != dm.d1 || list.Segments[0].MessageCount != 6 ||
		list.Segments[1].DividerID != dm.d2 || list.Segments[1].MessageCount != 2 {
		t.Fatalf("segments = %+v, want [d1:6 d2:2]", list.Segments)
	}

	read := func(divider, extra string) ChatMessagesPageResponse {
		w := dm.get(t, testHandler.GetChatSegment, "/segment?divider="+divider+extra, dm.sessionID, true)
		if w.Code != http.StatusOK {
			t.Fatalf("segment %s = %d %s", divider, w.Code, w.Body.String())
		}
		var page ChatMessagesPageResponse
		_ = json.Unmarshal(w.Body.Bytes(), &page)
		return page
	}
	wantSegTexts(t, segTexts(t, read(dm.d2, "").Messages), "SEG2-MSG", "SUMMARY-TWO")
	first := read(dm.d1, "")
	wantSegTexts(t, segTexts(t, first.Messages),
		"OLD-QUESTION-FIRST", "OLD-ANSWER-FIRST", "OLD-QUESTION-SECOND", "OLD-ANSWER-LAST", "OLD-USER-AFTER-ANSWER", "SUMMARY-ONE")

	// Paged: the newest two rows, then a cursor to the rest.
	p1 := read(dm.d1, "&limit=2")
	wantSegTexts(t, segTexts(t, p1.Messages), "OLD-USER-AFTER-ANSWER", "SUMMARY-ONE")
	if !p1.HasMore || p1.NextCursor == nil {
		t.Fatalf("page 1 = %+v, want has_more and a cursor", p1)
	}
	p2 := read(dm.d1, "&limit=10&before_created_at="+url.QueryEscape(p1.NextCursor.CreatedAt)+"&before_id="+p1.NextCursor.ID)
	wantSegTexts(t, segTexts(t, p2.Messages), "OLD-QUESTION-FIRST", "OLD-ANSWER-FIRST", "OLD-QUESTION-SECOND", "OLD-ANSWER-LAST")
	if p2.HasMore {
		t.Fatal("page 2 has_more, want the segment end")
	}
}

func TestAgentSegmentRead_RefusesAnotherDM(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newSegmentDM(t)
	// A subtest gives the second DM's agent a distinct name.
	t.Run("other", func(t *testing.T) { refusesOtherDM(t, dm, newSegmentDM(t)) })
}

func refusesOtherDM(t *testing.T, dm, other segmentDM) {
	t.Helper()

	// Another DM's divider on this DM's session: not found.
	if w := dm.get(t, testHandler.GetChatSegment, "/segment?divider="+other.d1, dm.sessionID, true); w.Code != http.StatusNotFound {
		t.Fatalf("other DM's divider = %d, want 404", w.Code)
	}
	// A non-divider message id and a missing divider: not found, not a bad request leak.
	var plain string
	if err := testPool.QueryRow(t.Context(), `SELECT id FROM chat_message WHERE chat_session_id = $1 AND content = 'CURRENT-MSG'`, dm.sessionID).Scan(&plain); err != nil {
		t.Fatal(err)
	}
	if w := dm.get(t, testHandler.GetChatSegment, "/segment?divider="+plain, dm.sessionID, true); w.Code != http.StatusNotFound {
		t.Fatalf("non-divider id = %d, want 404", w.Code)
	}
	// This DM's task token against the other DM's session: forbidden.
	if w := dm.get(t, testHandler.GetChatSegment, "/segment?divider="+other.d1, other.sessionID, true); w.Code != http.StatusForbidden {
		t.Fatalf("other session = %d, want 403", w.Code)
	}
	if w := dm.get(t, testHandler.ListChatSegments, "/segments", other.sessionID, true); w.Code != http.StatusForbidden {
		t.Fatalf("segments of other session = %d, want 403", w.Code)
	}
}
