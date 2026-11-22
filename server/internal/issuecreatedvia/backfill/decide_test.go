package backfill

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type failingQ struct {
	task       db.AgentTaskQueue
	commentErr error
	parentErr  error
}

func (f failingQ) GetAgentTask(context.Context, pgtype.UUID) (db.AgentTaskQueue, error) {
	return f.task, nil
}
func (f failingQ) GetComment(context.Context, pgtype.UUID) (db.Comment, error) {
	return db.Comment{}, f.commentErr
}
func (f failingQ) GetIssueInWorkspace(context.Context, db.GetIssueInWorkspaceParams) (db.Issue, error) {
	return db.Issue{}, f.parentErr
}

func id(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }

func agentRow(parent pgtype.UUID) row {
	return row{
		ID: id(9), CreatorType: "agent", CreatorID: id(1),
		OriginType: pgtype.Text{String: "agent_create", Valid: true}, OriginID: id(2), ParentID: parent,
	}
}

// A failed lookup must not store a guess: a lost comment lookup would give
// `agent` for a member comment, a lost parent lookup `dm` for a same-run child.
func TestDecideDoesNotGuessOnLookupFailure(t *testing.T) {
	boom := errors.New("connection reset")
	legacyComment := db.AgentTaskQueue{AgentID: id(1), TriggerCommentID: id(3)}
	chatTask := db.AgentTaskQueue{AgentID: id(1), ChatSessionID: id(4)}

	t.Run("comment lookup fails", func(t *testing.T) {
		d, err := decide(context.Background(), failingQ{task: legacyComment, commentErr: boom}, agentRow(pgtype.UUID{}))
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
		if d.Value != "" {
			t.Errorf("decision carries a guessed value %q", d.Value)
		}
	})
	t.Run("parent lookup fails", func(t *testing.T) {
		d, err := decide(context.Background(), failingQ{task: chatTask, parentErr: boom}, agentRow(id(5)))
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want %v", err, boom)
		}
		if d.Value != "" {
			t.Errorf("decision carries a guessed value %q", d.Value)
		}
	})
	t.Run("comment gone is unresolved", func(t *testing.T) {
		d, err := decide(context.Background(), failingQ{task: legacyComment, commentErr: pgx.ErrNoRows}, agentRow(pgtype.UUID{}))
		if err != nil || !d.Unresolved || d.changes() {
			t.Errorf("decision = %+v, err = %v; want unresolved and unchanged", d, err)
		}
	})
	t.Run("parent gone is unresolved", func(t *testing.T) {
		d, err := decide(context.Background(), failingQ{task: chatTask, parentErr: pgx.ErrNoRows}, agentRow(id(5)))
		if err != nil || !d.Unresolved || d.changes() {
			t.Errorf("decision = %+v, err = %v; want unresolved and unchanged", d, err)
		}
	})
}
