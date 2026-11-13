package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The wording differs per language and the outcome must not: no check reads
// the text, only the structural mention link does.
var askGuardCommentTexts = map[string]string{
	"english":    "Please decide whether to waive or rewrite it: %s",
	"vietnamese": "Vui lòng quyết định bỏ qua hay viết lại: %s",
	"chinese":    "请决定是豁免还是重写：%s",
}

// The notification parser scans raw content, so a link in code still notifies.
var askGuardMentionShapes = map[string]string{
	"plain text":  "%s",
	"inline code": "`%s`",
	"fenced code": "```\n%s\n```",
}

type askGuardCommentFixture struct{ intentFixture }

func newAskGuardCommentFixture(t *testing.T) askGuardCommentFixture {
	t.Helper()
	f := askGuardCommentFixture{newIntentFixture(t)}
	dbfx.Cleanup(t, `DELETE FROM comment WHERE issue_id = $1`, f.issueID)
	dbfx.Cleanup(t, `DELETE FROM ask_guard_event WHERE issue_id = $1`, f.issueID)
	return f
}

func (f askGuardCommentFixture) postAs(t *testing.T, taskID string, body map[string]any) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", body), "id", f.issueID)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", f.agentID)
	req.Header.Set("X-Task-ID", taskID)
	return testutil.Call(t, testHandler.CreateComment, req)
}

func (f askGuardCommentFixture) post(t *testing.T, body map[string]any) *testutil.Response {
	t.Helper()
	return f.postAs(t, f.taskID, body)
}

func (f askGuardCommentFixture) comments(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, f.issueID)
}

func (f askGuardCommentFixture) events(t *testing.T, mode string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1 AND mode = $2`, f.issueID, mode)
}

func (f askGuardCommentFixture) content(text, shape string) string {
	link := strings.Replace(shape, "%s", f.mention(), 1)
	return strings.Replace(text, "%s", link, 1)
}

func TestAskGuardCommentRefusesPlainMemberMention(t *testing.T) {
	setAskGuardMode(t, "block")
	for lang, text := range askGuardCommentTexts {
		for shape, wrap := range askGuardMentionShapes {
			t.Run(lang+"/"+shape, func(t *testing.T) {
				f := newAskGuardCommentFixture(t)
				res := f.post(t, map[string]any{"content": f.content(text, wrap)}).Want(http.StatusUnprocessableEntity)
				for _, fix := range []string{"multica decision request", "--notify-only"} {
					if !strings.Contains(res.Text(), fix) {
						t.Errorf("422 body lacks %q: %s", fix, res.Text())
					}
				}
				if n := f.comments(t); n != 0 {
					t.Errorf("refused comment saved %d rows", n)
				}
				if n := f.events(t, "block"); n != 1 {
					t.Errorf("block events = %d, want 1", n)
				}
			})
		}
	}
}

func TestAskGuardCommentAllowedKinds(t *testing.T) {
	setAskGuardMode(t, "block")
	for lang, text := range askGuardCommentTexts {
		t.Run(lang, func(t *testing.T) {
			f := newAskGuardCommentFixture(t)
			content := f.content(text, "%s")
			cases := []struct {
				name string
				body map[string]any
			}{
				{"notify only", map[string]any{"content": content, "notify_only": true}},
				{"decision intent", map[string]any{"content": content, "decision_intent": true}},
				{"no member mention", map[string]any{"content": strings.Replace(text, "%s", "nobody", 1)}},
				{"agent mention only", map[string]any{"content": strings.Replace(text, "%s", "[@A](mention://agent/"+f.agentID+")", 1)}},
			}
			for _, tc := range cases {
				f.post(t, tc.body).Want(http.StatusCreated)
			}
			if n := f.events(t, "block"); n != 0 {
				t.Errorf("allowed kinds wrote %d block events", n)
			}
			if n := f.comments(t); n != len(cases) {
				t.Errorf("comments = %d, want %d", n, len(cases))
			}
		})
	}
}

func TestAskGuardCommentNotifyOnlyConflicts(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardCommentFixture(t)
	for _, kind := range []string{"decision_intent", "decision_record", "decision_veto"} {
		f.post(t, map[string]any{"content": f.mention(), "notify_only": true, kind: true}).Want(http.StatusBadRequest)
	}
}

func TestAskGuardCommentModes(t *testing.T) {
	t.Run("off writes nothing", func(t *testing.T) {
		f := newAskGuardCommentFixture(t)
		f.post(t, map[string]any{"content": f.mention()}).Want(http.StatusCreated)
		if n := f.events(t, "warn") + f.events(t, "block"); n != 0 {
			t.Errorf("off mode wrote %d events", n)
		}
	})
	t.Run("warn stores an event and posts", func(t *testing.T) {
		setAskGuardMode(t, "warn")
		f := newAskGuardCommentFixture(t)
		f.post(t, map[string]any{"content": f.mention()}).Want(http.StatusCreated)
		if n := f.events(t, "warn"); n != 1 {
			t.Errorf("warn events = %d, want 1", n)
		}
	})
}

func TestAskGuardCommentRetryFindsSameEvent(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardCommentFixture(t)
	for range 2 {
		f.post(t, map[string]any{"content": f.mention()}).Want(http.StatusUnprocessableEntity)
	}
	if n := f.events(t, "block"); n != 1 {
		t.Errorf("a retry made %d events, want 1", n)
	}
}

// A chat-lane task has a live DM with the person, so block mode only warns.
func TestAskGuardCommentChatLaneWarnsOnly(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardCommentFixture(t)
	session := dbfx.ChatSession(t, f.agentID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_session_id = $1 WHERE id = $2`, session, f.taskID)
	f.post(t, map[string]any{"content": f.mention()}).Want(http.StatusCreated)
	if n, b := f.events(t, "warn"), f.events(t, "block"); n != 1 || b != 0 {
		t.Errorf("chat lane events warn=%d block=%d, want 1 and 0", n, b)
	}
}

func TestAskGuardCommentMemberAuthorIsNotGuarded(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardCommentFixture(t)
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": f.mention()}), "id", f.issueID)
	testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated)
	if n := f.events(t, "block") + f.events(t, "warn"); n != 0 {
		t.Errorf("member comment wrote %d events", n)
	}
}

// The veto and the decision request's own ask comment are typed kinds: block
// mode must not touch them, even when they mention a member.
func TestAskGuardCommentTypedDecisionKindsPassInBlockMode(t *testing.T) {
	setAskGuardMode(t, "block")
	t.Run("veto with a member mention", func(t *testing.T) {
		fixture := newAskedRecordFixture(t)
		record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
		if err != nil {
			t.Fatal(err)
		}
		reason := "Wrong source, ask [@Human](mention://member/" + testUserID + ") again"
		res := vetoDecisionHTTP(testHandler, fixture, uuidToString(record.Comment.ID), reason, true)
		if res.Code != http.StatusCreated {
			t.Fatalf("veto status = %d: %s", res.Code, res.Body.String())
		}
	})
	t.Run("decision request ask comment", TestDecisionRequestCoreRoundTrip)
}

func (f askGuardCommentFixture) edit(t *testing.T, commentID, content string) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPut, "/api/comments/"+commentID, map[string]any{"content": content}), "commentId", commentID)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", f.agentID)
	req.Header.Set("X-Task-ID", f.taskID)
	return testutil.Call(t, testHandler.UpdateComment, req)
}

// An edit must not be a way around the guard: adding a member link to an
// existing agent comment is refused, while keeping an old link is not.
func TestAskGuardCommentEditAddingMemberMention(t *testing.T) {
	setAskGuardMode(t, "block")
	t.Run("added link is refused", func(t *testing.T) {
		f := newAskGuardCommentFixture(t)
		id, _ := f.post(t, map[string]any{"content": "Receipt posted"}).Want(http.StatusCreated).Map()["id"].(string)
		res := f.edit(t, id, "Receipt posted "+f.mention()).Want(http.StatusUnprocessableEntity)
		if !strings.Contains(res.Text(), "--notify-only") {
			t.Errorf("422 body lacks the fix: %s", res.Text())
		}
		if n := f.events(t, "block"); n != 1 {
			t.Errorf("block events = %d, want 1", n)
		}
	})
	t.Run("kept link is allowed", func(t *testing.T) {
		f := newAskGuardCommentFixture(t)
		id, _ := f.post(t, map[string]any{"content": "FYI " + f.mention(), "notify_only": true}).Want(http.StatusCreated).Map()["id"].(string)
		f.edit(t, id, "FYI again "+f.mention()).Want(http.StatusOK)
		if n := f.events(t, "block"); n != 0 {
			t.Errorf("block events = %d, want 0", n)
		}
	})
	t.Run("warn mode posts the edit", func(t *testing.T) {
		setAskGuardMode(t, "warn")
		f := newAskGuardCommentFixture(t)
		id, _ := f.post(t, map[string]any{"content": "Receipt posted"}).Want(http.StatusCreated).Map()["id"].(string)
		f.edit(t, id, "Receipt posted "+f.mention()).Want(http.StatusOK)
		if n := f.events(t, "warn"); n != 1 {
			t.Errorf("warn events = %d, want 1", n)
		}
	})
}
