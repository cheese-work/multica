//go:build issue_channels_red

package main

// CHE-5906 (spike C1 of CHE-5673 plan v2): failing tests for slice C2,
// GET /api/channels. Build tag issue_channels_red keeps them out of main CI.
//
//	go test -tags issue_channels_red -run TestChannelListC2 ./cmd/server/
//
// Response shape these tests pin (C2 may change it, but then it must change the
// tests in the same PR):
//
//	{"channels":[{"issue_id","thread_count","waiting_count"}]}
//
// Not covered here, and why:
//   - "opened" mark: no store exists today (spike note Q1), so it cannot be
//     seeded until C2-table lands thread_participant.
//   - unreadable issue: every workspace member reads every workspace issue
//     today; C2 must name the read check before this case can be written.
//   - summary rows: need parent/project fixtures; C2 adds them.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type c2Row struct {
	IssueID      string `json:"issue_id"`
	ThreadCount  int    `json:"thread_count"`
	WaitingCount int    `json:"waiting_count"`
}

func c2List(t *testing.T) []c2Row {
	t.Helper()
	resp := authRequest(t, "GET", "/api/channels", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/channels = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Channels []c2Row `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Channels
}

func c2Find(rows []c2Row, issueID string) *c2Row {
	for i := range rows {
		if rows[i].IssueID == issueID {
			return &rows[i]
		}
	}
	return nil
}

func TestChannelListC2(t *testing.T) {
	fx := workspaceFixture(t)
	runtime := fx.Runtime(t, "c2-runtime")
	agent := fx.Agent(t, "c2-agent", runtime)

	// agentRoot: agent comment, hours ago. reply: agent child comment, later.
	agentRoot := func(issue string, ago string) string {
		return fx.Comment(t, issue, "agent root", testutil.Cols{
			"author_type": "agent", "author_id": agent,
			"created_at": testutil.Raw(fmt.Sprintf("now() - interval '%s'", ago)),
		})
	}
	reply := func(issue, parent string, ago string) string {
		return fx.Comment(t, issue, "agent reply", testutil.Cols{
			"author_type": "agent", "author_id": agent, "parent_id": parent,
			"created_at": testutil.Raw(fmt.Sprintf("now() - interval '%s'", ago)),
		})
	}
	// tag: the `mentioned` Inbox item notifyMentionedMembers writes today. The
	// comment link lives in details; notify_only is NOT written today (note Q2),
	// so details.notify_only is the shape C2/C4 must add.
	tag := func(issue, comment string, notifyOnly bool) {
		details := fmt.Sprintf(`{"comment_id":%q,"mention_source":"comment","notify_only":%t}`, comment, notifyOnly)
		fx.Insert(t, "inbox_item", testutil.Cols{
			"workspace_id": testWorkspaceID, "recipient_type": "member", "recipient_id": testUserID,
			"type": "mentioned", "issue_id": issue, "title": "tagged", "details": details,
		})
	}

	tagNoReply := fx.Issue(t, "c2 tagged, no reply")
	tag(tagNoReply, agentRoot(tagNoReply, "2 hours"), false)

	tagReplied := fx.Issue(t, "c2 tagged, then replied")
	r := agentRoot(tagReplied, "3 hours")
	tag(tagReplied, r, false)
	reply(tagReplied, r, "2 hours")

	wrote := fx.Issue(t, "c2 member wrote")
	fx.Comment(t, wrote, "my comment") // author = member (testUserID)

	notifyOnly := fx.Issue(t, "c2 notify_only tag, replied")
	n := agentRoot(notifyOnly, "3 hours")
	tag(notifyOnly, n, true)
	reply(notifyOnly, n, "2 hours")

	rows := c2List(t)

	t.Run("tag with no reply is not a channel row", func(t *testing.T) {
		if got := c2Find(rows, tagNoReply); got != nil {
			t.Fatalf("got row %+v, want none", *got)
		}
	})
	t.Run("tag then reply makes a row that is waiting for the member", func(t *testing.T) {
		got := c2Find(rows, tagReplied)
		if got == nil || got.ThreadCount != 1 || got.WaitingCount != 1 {
			t.Fatalf("got %+v, want thread_count=1 waiting_count=1", got)
		}
	})
	t.Run("member comment makes a row that is not waiting", func(t *testing.T) {
		got := c2Find(rows, wrote)
		if got == nil || got.ThreadCount != 1 || got.WaitingCount != 0 {
			t.Fatalf("got %+v, want thread_count=1 waiting_count=0", got)
		}
	})
	t.Run("notify_only tag still joins but is not waiting", func(t *testing.T) {
		got := c2Find(rows, notifyOnly)
		if got == nil || got.ThreadCount != 1 || got.WaitingCount != 0 {
			t.Fatalf("got %+v, want thread_count=1 waiting_count=0", got)
		}
	})
	t.Run("waiting rows sort before quiet rows", func(t *testing.T) {
		pos := map[string]int{}
		for i, r := range rows {
			pos[r.IssueID] = i
		}
		w, okW := pos[tagReplied]
		q, okQ := pos[wrote]
		if !okW || !okQ || w > q {
			t.Fatalf("order: waiting row at %d (found %v), quiet row at %d (found %v)", w, okW, q, okQ)
		}
	})
}
