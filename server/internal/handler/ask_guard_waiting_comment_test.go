package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// S4 (CHE-7289): an agent comment on an issue that waits (in_review, blocked,
// need_decision) must carry a typed audience. The tests post the same plain
// text everywhere; only the typed fields and the structural links change the
// outcome.

func setWaitingAudienceMode(t *testing.T, mode string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'), '{waiting_audience_guard}', to_jsonb($1::text)) WHERE id = $2`,
		mode, testWorkspaceID); err != nil {
		t.Fatalf("set waiting_audience_guard: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings = settings - 'waiting_audience_guard' WHERE id = $1`, testWorkspaceID)
	})
}

func (f askGuardCommentFixture) setStatus(t *testing.T, status string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue SET status = $1 WHERE id = $2`, status, f.issueID)
}

func (f askGuardCommentFixture) waitingHits(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1 AND operation_kind = 'waiting_comment'`, f.issueID)
}

// The CHE-4960 shape: an agent comment on an in_review issue, no member link,
// no typed kind, wording that asks a person to act.
const waitingPlainAsk = "Cheese, please pick option A or B so the work can continue."

func TestWaitingAudienceBlockRefusesUnmarkedComment(t *testing.T) {
	setWaitingAudienceMode(t, "block")
	for _, status := range []string{"in_review", "blocked", "need_decision"} {
		t.Run(status, func(t *testing.T) {
			f := newAskGuardCommentFixture(t)
			f.setStatus(t, status)
			res := f.post(t, map[string]any{"content": waitingPlainAsk}).Want(http.StatusUnprocessableEntity)
			for _, fix := range []string{"--ask", "--no-ask"} {
				if !strings.Contains(res.Text(), fix) {
					t.Errorf("422 body lacks %q: %s", fix, res.Text())
				}
			}
			if n := f.comments(t); n != 0 {
				t.Errorf("refused comment saved %d rows", n)
			}
			if n := f.waitingHits(t); n != 1 {
				t.Errorf("hits = %d, want 1", n)
			}
		})
	}
}

func TestWaitingAudienceWarnRecordsHitAndPosts(t *testing.T) {
	setWaitingAudienceMode(t, "warn")
	f := newAskGuardCommentFixture(t)
	f.setStatus(t, "in_review")
	f.post(t, map[string]any{"content": waitingPlainAsk}).Want(http.StatusCreated)
	if n := f.waitingHits(t); n != 1 {
		t.Errorf("warn hits = %d, want 1", n)
	}
}

func TestWaitingAudienceOffWritesNothing(t *testing.T) {
	f := newAskGuardCommentFixture(t)
	f.setStatus(t, "in_review")
	f.post(t, map[string]any{"content": waitingPlainAsk}).Want(http.StatusCreated)
	if n := f.waitingHits(t); n != 0 {
		t.Errorf("off hits = %d, want 0", n)
	}
}

func TestWaitingAudienceTypedAudiencePasses(t *testing.T) {
	setWaitingAudienceMode(t, "block")
	f := newAskGuardCommentFixture(t)
	f.setStatus(t, "in_review")
	agentParent, _ := f.post(t, map[string]any{"content": "agent note", "no_ask": true}).Want(http.StatusCreated).Map()["id"].(string)
	cases := map[string]map[string]any{
		"--ask member":      {"content": waitingPlainAsk, "ask": f.humanID},
		"--no-ask":          {"content": waitingPlainAsk, "no_ask": true},
		"notify only":       {"content": waitingPlainAsk, "notify_only": true},
		"member mention":    {"content": waitingPlainAsk + " " + f.mention(), "notify_only": true},
		"agent mention":     {"content": waitingPlainAsk + " [@A](mention://agent/" + f.agentID + ")"},
		"reply under agent": {"content": waitingPlainAsk, "parent_id": agentParent},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) { f.post(t, body).Want(http.StatusCreated) })
	}
	if n := f.waitingHits(t); n != 0 {
		t.Errorf("typed audiences wrote %d hits", n)
	}
}

func TestWaitingAudienceOtherStatusesAndAuthorsPass(t *testing.T) {
	setWaitingAudienceMode(t, "block")
	for _, status := range []string{"in_progress", "todo", "agent_review", "done"} {
		t.Run(status, func(t *testing.T) {
			f := newAskGuardCommentFixture(t)
			f.setStatus(t, status)
			f.post(t, map[string]any{"content": waitingPlainAsk}).Want(http.StatusCreated)
			if n := f.waitingHits(t); n != 0 {
				t.Errorf("hits = %d, want 0", n)
			}
		})
	}
	t.Run("member author", func(t *testing.T) {
		f := newAskGuardCommentFixture(t)
		f.setStatus(t, "in_review")
		req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/comments", map[string]any{"content": waitingPlainAsk}), "id", f.issueID)
		testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated)
		if n := f.waitingHits(t); n != 0 {
			t.Errorf("member comment wrote %d hits", n)
		}
	})
}

func TestWaitingAudienceBadTypedFieldsAre400(t *testing.T) {
	f := newAskGuardCommentFixture(t)
	f.setStatus(t, "in_review")
	for name, body := range map[string]map[string]any{
		"ask and no_ask":   {"content": "x", "ask": f.humanID, "no_ask": true},
		"ask not a uuid":   {"content": "x", "ask": "cheese"},
		"ask not a member": {"content": "x", "ask": "00000000-0000-4000-8000-000000000000"},
	} {
		t.Run(name, func(t *testing.T) { f.post(t, body).Want(http.StatusBadRequest) })
	}
}

// Chat-lane and unbound runs have no run to recover through: block only warns.
func TestWaitingAudienceChatLaneWarnsOnly(t *testing.T) {
	setWaitingAudienceMode(t, "block")
	f := newAskGuardCommentFixture(t)
	f.setStatus(t, "in_review")
	session := dbfx.ChatSession(t, f.agentID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_session_id = $1 WHERE id = $2`, session, f.taskID)
	f.post(t, map[string]any{"content": waitingPlainAsk}).Want(http.StatusCreated)
	if n := f.waitingHits(t); n != 1 {
		t.Errorf("hits = %d, want 1", n)
	}
}

// S2 and S4 both on block: S2 still refuses an untyped member mention (its
// protection is kept), while the typed audiences S4 accepts still post when no
// untyped member link is present.
func TestWaitingAudienceCombinedWithS2Block(t *testing.T) {
	setAskGuardMode(t, "block")
	setWaitingAudienceMode(t, "block")
	f := newAskGuardCommentFixture(t)
	f.setStatus(t, "in_review")
	f.post(t, map[string]any{"content": waitingPlainAsk, "ask": f.humanID}).Want(http.StatusCreated)
	f.post(t, map[string]any{"content": waitingPlainAsk, "no_ask": true}).Want(http.StatusCreated)
	f.post(t, map[string]any{"content": waitingPlainAsk + " " + f.mention(), "notify_only": true}).Want(http.StatusCreated)
	f.post(t, map[string]any{"content": waitingPlainAsk + " " + f.mention(), "no_ask": true}).Want(http.StatusUnprocessableEntity)
	if n := f.waitingHits(t); n != 0 {
		t.Errorf("typed audiences wrote %d waiting hits", n)
	}
}
