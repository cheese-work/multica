package issuecreatedvia

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeQ struct {
	task       db.AgentTaskQueue
	taskErr    error
	comment    db.Comment
	commentErr error
	parent     db.Issue
	parentErr  error
}

func (f fakeQ) GetAgentTask(context.Context, pgtype.UUID) (db.AgentTaskQueue, error) {
	return f.task, f.taskErr
}
func (f fakeQ) GetComment(context.Context, pgtype.UUID) (db.Comment, error) {
	return f.comment, f.commentErr
}
func (f fakeQ) GetIssueInWorkspace(context.Context, db.GetIssueInWorkspaceParams) (db.Issue, error) {
	return f.parent, f.parentErr
}

func uid(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }

func legacyCommentRequest() (fakeQ, Request) {
	q := fakeQ{task: db.AgentTaskQueue{AgentID: uid(1), TriggerCommentID: uid(3)}}
	return q, Request{CreatorAgent: true, OriginType: OriginAgentCreate, AgentID: uid(1), TaskID: uid(2)}
}

func TestBuildInputStrictFailsOnLookupError(t *testing.T) {
	boom := errors.New("connection reset")
	t.Run("comment", func(t *testing.T) {
		q, r := legacyCommentRequest()
		q.commentErr = boom
		if _, err := BuildInputStrict(context.Background(), q, r); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
		// The create path keeps its fallback: agent, not a failure.
		if got := ForCreate(context.Background(), q, r); got != Agent {
			t.Errorf("create path = %q, want %q", got, Agent)
		}
	})
	t.Run("comment gone", func(t *testing.T) {
		q, r := legacyCommentRequest()
		q.commentErr = pgx.ErrNoRows
		if _, err := BuildInputStrict(context.Background(), q, r); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("err = %v, want ErrNoRows", err)
		}
	})
	t.Run("parent", func(t *testing.T) {
		q := fakeQ{task: db.AgentTaskQueue{AgentID: uid(1), ChatSessionID: uid(4)}, parentErr: boom}
		r := Request{CreatorAgent: true, OriginType: OriginAgentCreate, AgentID: uid(1), TaskID: uid(2), ParentID: uid(5)}
		if _, err := BuildInputStrict(context.Background(), q, r); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
		// The create path keeps its fallback: the chat task gives dm.
		if got := ForCreate(context.Background(), q, r); got != DM {
			t.Errorf("create path = %q, want %q", got, DM)
		}
	})
	t.Run("task", func(t *testing.T) {
		q, r := legacyCommentRequest()
		q.taskErr = boom
		if _, err := BuildInputStrict(context.Background(), q, r); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
	})
}
