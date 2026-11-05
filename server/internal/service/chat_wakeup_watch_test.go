package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (f chatWakeupFixture) watch(t *testing.T, in ChatWakeupInput) (db.ChatWakeup, error) {
	t.Helper()
	in.TaskID, in.IssueID = f.taskID, util.MustParseUUID(f.issueID)
	if in.At.IsZero() {
		in.At = time.Now().Add(time.Hour)
	}
	if in.Instruction == "" {
		in.Instruction = "relay the result"
	}
	return f.svc.Create(context.Background(), in)
}

func (f chatWakeupFixture) comment(t *testing.T, authorType, authorID, body string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content)
		 SELECT id, workspace_id, $2, $3::uuid, $4 FROM issue WHERE id = $1`,
		f.issueID, authorType, authorID, body); err != nil {
		t.Fatal(err)
	}
}

func (f chatWakeupFixture) tick(t *testing.T) {
	t.Helper()
	if err := f.svc.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

func (f chatWakeupFixture) lastTurn(t *testing.T) string {
	t.Helper()
	var content string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT m.content FROM chat_message m JOIN agent_task_queue t ON t.id = m.task_id
		WHERE m.chat_session_id = $1 AND t.status = 'queued'`, f.sessionID).Scan(&content); err != nil {
		t.Fatalf("load scheduled message: %v", err)
	}
	return content
}

// The watched agent comments: the DM gets one turn that quotes the comment.
func TestChatWatchFiresOnMatchingCommentAndQuotesIt(t *testing.T) {
	f := newChatWakeupFixture(t)
	w, err := f.watch(t, ChatWakeupInput{Event: "comment.created", ActorType: "agent", ActorID: util.MustParseUUID(f.agentID)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.tick(t)
	if got := f.status(t, w); got != "pending" || f.queuedTurns(t) != 0 {
		t.Fatalf("before the comment: status = %q, queued = %d; want pending, 0", got, f.queuedTurns(t))
	}
	f.comment(t, "member", f.userID, "wrong actor")
	f.tick(t)
	if got := f.status(t, w); got != "pending" {
		t.Fatalf("a comment by another actor fired the watch: %q", got)
	}
	f.comment(t, "agent", f.agentID, "Verdict PASS, SHA abc123")
	f.tick(t)
	f.tick(t)
	if got := f.status(t, w); got != "fired" || f.queuedTurns(t) != 1 {
		t.Fatalf("status = %q, queued = %d; want fired, 1", got, f.queuedTurns(t))
	}
	if c := f.lastTurn(t); !strings.Contains(c, "Verdict PASS, SHA abc123") || !strings.Contains(c, "relay the result") {
		t.Fatalf("turn = %q, want the instruction and the quoted comment", c)
	}
}

func TestChatWatchIgnoresCommentsOlderThanTheWatch(t *testing.T) {
	f := newChatWakeupFixture(t)
	f.comment(t, "member", f.userID, "old")
	w, err := f.watch(t, ChatWakeupInput{Event: "comment.created"})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if got := f.status(t, w); got != "pending" {
		t.Fatalf("status = %q, want pending", got)
	}
}

func TestChatWatchFiresWhenIssueReachesStatus(t *testing.T) {
	f := newChatWakeupFixture(t)
	w, err := f.watch(t, ChatWakeupInput{UntilStatus: "in_review"})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if got := f.status(t, w); got != "pending" {
		t.Fatalf("status = %q, want pending", got)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE issue SET status = 'in_review' WHERE id = $1`, f.issueID); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if got := f.status(t, w); got != "fired" || !strings.Contains(f.lastTurn(t), "in_review") {
		t.Fatalf("status = %q; want fired with the status in the turn", got)
	}
}

func TestChatWatchTimeout(t *testing.T) {
	for _, wake := range []bool{true, false} {
		f := newChatWakeupFixture(t)
		w, err := f.watch(t, ChatWakeupInput{Event: "comment.created", WakeOnTimeout: wake})
		if err != nil {
			t.Fatal(err)
		}
		f.makeDue(t, w)
		f.tick(t)
		want, turns := "cancelled", 0
		if wake {
			want, turns = "fired", 1
		}
		if got := f.status(t, w); got != want || f.queuedTurns(t) != turns {
			t.Fatalf("wake=%v: status = %q, queued = %d; want %s, %d", wake, got, f.queuedTurns(t), want, turns)
		}
		if wake && !strings.Contains(strings.ToLower(f.lastTurn(t)), "timed out") {
			t.Fatalf("turn = %q, want a timed-out label", f.lastTurn(t))
		}
	}
}

func TestChatWatchValidation(t *testing.T) {
	f := newChatWakeupFixture(t)
	for name, in := range map[string]ChatWakeupInput{
		"neither event nor status": {},
		"both":                     {Event: "comment.created", UntilStatus: "done"},
		"unknown event":            {Event: "issue.deleted"},
		"actor without type":       {Event: "comment.created", ActorID: util.MustParseUUID(f.agentID)},
		"bad actor type":           {Event: "comment.created", ActorType: "bot"},
	} {
		if _, err := f.watch(t, in); !errors.Is(err, ErrChatWatchInvalid) {
			t.Errorf("%s: err = %v, want ErrChatWatchInvalid", name, err)
		}
	}
}

// The DM's human must be able to read the issue: one in another workspace is refused.
func TestChatWatchRefusesIssueOutsideTheDMWorkspace(t *testing.T) {
	f := newChatWakeupFixture(t)
	_, _, _, otherIssue := seedAttributionFixture(t, f.pool)
	in := ChatWakeupInput{TaskID: f.taskID, IssueID: util.MustParseUUID(otherIssue), Event: "comment.created", Instruction: "x", At: time.Now().Add(time.Hour)}
	if _, err := f.svc.Create(context.Background(), in); !errors.Is(err, ErrChatWatchIssueAccess) {
		t.Fatalf("err = %v, want ErrChatWatchIssueAccess", err)
	}
}
