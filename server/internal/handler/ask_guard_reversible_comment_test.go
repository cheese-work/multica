package handler

import (
	"net/http"
	"strings"
	"testing"
)

// CHE-5754, CHE-5281 incident (comment 01a1d33c): a plain agent comment with no
// member mention asked Cheese to "reply A or B" on a blocked issue. No text is
// read; the structural facts are an agent task, no typed kind, and an issue
// waiting on a person (blocked or need_decision).
const reversibleAskIncidentText = "**Next action (Cheese, 1 minute): reply A or B below.**\n\n- A (recommended): move `agent_review` to x99-codex-terra.\n- B: leave WF006, rely on the exception."

func TestReversibleAskGuardPlainCommentOnWaitingIssue(t *testing.T) {
	for _, tc := range []struct {
		name, mode, status string
		body               map[string]any
		want               int
		saved              int
	}{
		{"block refuses on blocked", "block", "blocked", nil, http.StatusUnprocessableEntity, 0},
		{"block refuses on need_decision", "block", "need_decision", nil, http.StatusUnprocessableEntity, 0},
		{"block allows notify-only", "block", "blocked", map[string]any{"notify_only": true}, http.StatusCreated, 1},
		{"block allows decision intent", "block", "blocked", map[string]any{"decision_intent": true}, http.StatusCreated, 1},
		{"block allows a working issue", "block", "in_progress", nil, http.StatusCreated, 1},
		{"warn allows on blocked", "warn", "blocked", nil, http.StatusCreated, 1},
		{"off allows on blocked", "off", "blocked", nil, http.StatusCreated, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAskGuardCommentFixture(t)
			setReversibleAskGuard(t, tc.mode)
			dbfx.Exec(t, `UPDATE issue SET status = $1 WHERE id = $2`, tc.status, f.issueID)
			body := map[string]any{"content": reversibleAskIncidentText}
			if tc.body["decision_intent"] == true {
				body["content"] = f.mention() + " " + reversibleAskIncidentText
			}
			for k, v := range tc.body {
				body[k] = v
			}
			res := f.post(t, body).Want(tc.want)
			if tc.want == http.StatusUnprocessableEntity {
				for _, fix := range []string{"Standing Delivery Authorization", "multica decision request", "--notify-only"} {
					if !strings.Contains(res.Text(), fix) {
						t.Errorf("422 body lacks %q: %s", fix, res.Text())
					}
				}
			}
			if n := f.comments(t); n != tc.saved {
				t.Errorf("comments = %d, want %d", n, tc.saved)
			}
		})
	}
}
