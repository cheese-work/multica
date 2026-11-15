package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5754, CHE-5281 incident (comment 01a1d33c): a plain agent comment with no
// mention asked Cheese to "reply A or B". The comment was a reply under a
// member's comment on a blocked issue. No text is read; the structural facts
// are an agent task, no typed kind, a waiting issue (blocked or need_decision),
// a parent comment written by a member and no agent or squad mention. A
// top-level report, a reply under an agent and an agent handoff stay allowed.
const reversibleAskIncidentText = "**Next action (Cheese, 1 minute): reply A or B below.**\n\n- A (recommended): move `agent_review` to x99-codex-terra.\n- B: leave WF006, rely on the exception."

func TestReversibleAskGuardPlainCommentOnWaitingIssue(t *testing.T) {
	type tc struct {
		name, mode, status, parent string // parent: "" top-level, "member", "agent"
		body                       map[string]any
		content                    func(f askGuardCommentFixture) string
		want, saved                int
	}
	incident := func(askGuardCommentFixture) string { return reversibleAskIncidentText }
	handoff := func(f askGuardCommentFixture) string {
		return "[@Next](mention://agent/" + f.agentID + ") please review the PR and report back."
	}
	var cases []tc
	for _, status := range []string{"blocked", "need_decision"} {
		cases = append(cases,
			tc{"block refuses the incident reply on " + status, "block", status, "member", nil, incident, http.StatusUnprocessableEntity, 0},
			tc{"block allows a top-level report on " + status, "block", status, "", nil, incident, http.StatusCreated, 1},
			tc{"block allows a reply under an agent on " + status, "block", status, "agent", nil, incident, http.StatusCreated, 1},
			tc{"block allows an agent handoff reply on " + status, "block", status, "member", nil, handoff, http.StatusCreated, 1},
			tc{"block allows an agent handoff on " + status, "block", status, "", nil, handoff, http.StatusCreated, 1},
		)
	}
	cases = append(cases,
		tc{"block allows notify-only", "block", "blocked", "member", map[string]any{"notify_only": true}, incident, http.StatusCreated, 1},
		tc{"block allows a working issue", "block", "in_progress", "member", nil, incident, http.StatusCreated, 1},
		tc{"warn allows the incident reply", "warn", "blocked", "member", nil, incident, http.StatusCreated, 1},
		tc{"off allows the incident reply", "off", "blocked", "member", nil, incident, http.StatusCreated, 1},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAskGuardCommentFixture(t)
			setReversibleAskGuard(t, c.mode)
			dbfx.Exec(t, `UPDATE issue SET status = $1 WHERE id = $2`, c.status, f.issueID)
			body := map[string]any{"content": c.content(f)}
			base := 0
			if c.parent != "" {
				author, authorID := "member", f.humanID
				if c.parent == "agent" {
					author, authorID = "agent", f.agentID
				}
				body["parent_id"] = dbfx.Insert(t, "comment", testutil.Cols{"issue_id": f.issueID, "workspace_id": testWorkspaceID, "author_type": author, "author_id": authorID, "content": "done"})
				base = 1
			}
			for k, v := range c.body {
				body[k] = v
			}
			res := f.post(t, body).Want(c.want)
			if c.want == http.StatusUnprocessableEntity {
				for _, fix := range []string{"Standing Delivery Authorization", "multica decision request", "--notify-only"} {
					if !strings.Contains(res.Text(), fix) {
						t.Errorf("422 body lacks %q: %s", fix, res.Text())
					}
				}
			}
			if n := f.comments(t) - base; n != c.saved {
				t.Errorf("comments = %d, want %d", n, c.saved)
			}
		})
	}
}
