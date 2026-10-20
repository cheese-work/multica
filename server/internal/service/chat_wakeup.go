package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DM wakeup (CHE-1874): an agent in a web DM schedules its own follow-up turn.
// The scheduler starts it as an ordinary DM turn whose input is a hidden
// wakeup_trigger message, so it runs with the DM's history and raises the unread
// badge through the normal completion path.

const (
	ChatWakeupMaxPending     = 3
	ChatWakeupMaxAhead       = 24 * time.Hour
	ChatWakeupMaxInstruction = 12000

	// WakeupTaskKind marks a DM turn a wakeup started (agent_task_queue.task_kind).
	WakeupTaskKind = "wakeup"

	chatWakeupTickBatch = 50
	chatWakeupMaxHops   = 5
)

var (
	ErrChatWakeupInput = errors.New("invalid chat wakeup")
	ErrChatWakeupCap   = errors.New("this DM already has the maximum number of pending wakeups")
	// ErrChatWakeupUnavailable: the DM cannot take a wakeup (archived, continued
	// or bound to a channel).
	ErrChatWakeupUnavailable = errors.New("wakeups are only available in an active web DM")

	errChatWakeupRetry = errors.New("chat wakeup: retry next tick")
)

type ChatWakeupService struct{ Tasks *TaskService }

// ChatWakeupConditionInput is what the agent waits for besides the deadline:
// the watched issue (already resolved by the caller) and exactly one of a
// status key, a linked pull request event, or a new comment by an actor.
type ChatWakeupConditionInput struct {
	Issue     pgtype.UUID
	Status    string
	PR        string // checks_finished | merged
	CommentBy *ChatWakeupCommenter
}

type ChatWakeupCommenter struct {
	Type string `json:"type"` // member | agent
	ID   string `json:"id"`
}

// chatWakeupCondition is the stored form. Check is a normalized
// WakeupCondition, so the issue-wakeup evaluators decide status and pull
// request facts. Baseline is the pull request fingerprint at creation: finished
// checks that already existed belong to an older push.
type chatWakeupCondition struct {
	IssueID    string               `json:"issue_id"`
	Identifier string               `json:"identifier"`
	Check      json.RawMessage      `json:"check,omitempty"`
	CommentBy  *ChatWakeupCommenter `json:"comment_by,omitempty"`
	Baseline   string               `json:"baseline,omitempty"`
}

// Create validates and stores a wakeup for the DM, under the session lock so
// the pending cap cannot be raced past. With a condition, fireAt is the deadline.
func (s *ChatWakeupService) Create(ctx context.Context, sessionID, sourceTaskID pgtype.UUID, instruction string, fireAt, now time.Time, condition *ChatWakeupConditionInput) (db.ChatWakeup, error) {
	bad := func(msg string) (db.ChatWakeup, error) {
		return db.ChatWakeup{}, fmt.Errorf("%w: %s", ErrChatWakeupInput, msg)
	}
	instruction = strings.TrimSpace(instruction)
	switch {
	case instruction == "" || len(instruction) > ChatWakeupMaxInstruction || !utf8.ValidString(instruction):
		return bad(fmt.Sprintf("instruction must be UTF-8 text of 1–%d bytes", ChatWakeupMaxInstruction))
	case !fireAt.After(now):
		return bad("the wakeup time must be in the future")
	case fireAt.Sub(now) > ChatWakeupMaxAhead:
		return bad("the wakeup time must be within 24 hours")
	}
	var out db.ChatWakeup
	err := s.Tasks.runInTx(ctx, func(qtx *db.Queries) error {
		session, err := qtx.LockChatSessionForEnqueue(ctx, sessionID)
		if err != nil {
			return err
		}
		if session.Status != "active" || session.ContinuedIntoSessionID.Valid || s.channelBound(ctx, qtx, session.ID) {
			return ErrChatWakeupUnavailable
		}
		var stored []byte
		if condition != nil {
			if stored, err = s.buildCondition(ctx, session.WorkspaceID, *condition); err != nil {
				return err
			}
		}
		pending, err := qtx.CountPendingChatWakeups(ctx, session.ID)
		if err != nil {
			return err
		}
		if pending >= ChatWakeupMaxPending {
			return ErrChatWakeupCap
		}
		out, err = qtx.CreateChatWakeup(ctx, db.CreateChatWakeupParams{
			WorkspaceID:   session.WorkspaceID,
			ChatSessionID: session.ID,
			AgentID:       session.AgentID,
			CreatedBy:     session.CreatorID,
			SourceTaskID:  sourceTaskID,
			Instruction:   instruction,
			FireAt:        pgtype.Timestamptz{Time: fireAt.UTC(), Valid: true},
			Condition:     stored,
		})
		return err
	})
	return out, err
}

// buildCondition validates the condition against the workspace and returns its
// stored JSON. It reads on its own connection: the reads need no lock.
func (s *ChatWakeupService) buildCondition(ctx context.Context, workspaceID pgtype.UUID, in ChatWakeupConditionInput) ([]byte, error) {
	bad := func(msg string) ([]byte, error) { return nil, fmt.Errorf("%w: %s", ErrChatWakeupInput, msg) }
	predicates := 0
	for _, set := range []bool{in.Status != "", in.PR != "", in.CommentBy != nil} {
		if set {
			predicates++
		}
	}
	if predicates != 1 {
		return bad("a condition needs exactly one of status, pr or comment_by")
	}
	issue, err := s.Tasks.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: in.Issue, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) || !in.Issue.Valid {
		return bad("unknown issue")
	} else if err != nil {
		return nil, err
	}
	identifier, err := s.Tasks.Queries.GetChatWakeupIssueIdentifier(ctx, db.GetChatWakeupIssueIdentifierParams{ID: issue.ID, WorkspaceID: workspaceID})
	if err != nil {
		return nil, err
	}
	stored := chatWakeupCondition{IssueID: util.UUIDToString(issue.ID), Identifier: identifier}
	switch {
	case in.CommentBy != nil:
		if _, err := util.ParseUUID(in.CommentBy.ID); err != nil || (in.CommentBy.Type != "member" && in.CommentBy.Type != "agent") {
			return bad("comment_by needs a member or agent type and a UUID")
		}
		stored.CommentBy = in.CommentBy
	default:
		raw := map[string]string{"type": "issue_field", "field": "status", "value": in.Status}
		if in.PR != "" {
			raw = map[string]string{"type": "pull_request", "event": in.PR}
		}
		encoded, _ := json.Marshal(raw)
		tx, err := s.Tasks.TxStarter.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		if stored.Check, _, err = validateCondition(ctx, tx, issue, encoded); err != nil {
			if errors.Is(err, errBadCondition) {
				return bad(strings.TrimPrefix(err.Error(), ErrWakeupInput.Error()+": "+errBadCondition.Error()+": "))
			}
			return nil, err
		}
		if in.PR == "checks_finished" {
			// Best effort: a pull request that is not linked yet has no baseline.
			if _, fingerprint, _, err := (&IssueWakeupService{Tasks: s.Tasks}).evaluateCondition(ctx, tx, s.Tasks.Queries.WithTx(tx), db.IssueWakeup{IssueID: issue.ID, WorkspaceID: workspaceID, Condition: stored.Check}); err == nil {
				stored.Baseline = fingerprint
			}
		}
	}
	return json.Marshal(stored)
}

// conditionMet reports whether a wakeup's condition holds now, and what was
// observed. A wakeup without a condition, or one that cannot be evaluated yet
// (pull request not linked, snapshot pending), is not met.
func (s *ChatWakeupService) conditionMet(ctx context.Context, w db.ChatWakeup) (bool, string, error) {
	if len(w.Condition) == 0 {
		return false, "", nil
	}
	var c chatWakeupCondition
	if err := json.Unmarshal(w.Condition, &c); err != nil {
		return false, "", err
	}
	issueID, err := util.ParseUUID(c.IssueID)
	if err != nil {
		return false, "", err
	}
	if c.CommentBy != nil {
		authorID, err := util.ParseUUID(c.CommentBy.ID)
		if err != nil {
			return false, "", err
		}
		commentID, err := s.Tasks.Queries.GetChatWakeupNewComment(ctx, db.GetChatWakeupNewCommentParams{
			IssueID: issueID, WorkspaceID: w.WorkspaceID, AuthorType: c.CommentBy.Type, AuthorID: authorID, CreatedAt: w.CreatedAt,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "", nil
		} else if err != nil {
			return false, "", err
		}
		return true, fmt.Sprintf("a new comment (%s) by the %s you named", util.UUIDToString(commentID), c.CommentBy.Type), nil
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback(ctx)
	met, fingerprint, observed, err := (&IssueWakeupService{Tasks: s.Tasks}).evaluateCondition(ctx, tx, s.Tasks.Queries.WithTx(tx),
		db.IssueWakeup{IssueID: issueID, WorkspaceID: w.WorkspaceID, Condition: c.Check})
	if err != nil {
		if conditionDiagnostic(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if !met || (c.Baseline != "" && fingerprint == c.Baseline) {
		return false, "", nil
	}
	facts, _ := json.Marshal(observed)
	return true, string(bytes.TrimSpace(facts)), nil
}

// channelBound is true when an external channel delivers this DM's replies.
// A failed lookup counts as bound: the wakeup is refused rather than risked.
func (s *ChatWakeupService) channelBound(ctx context.Context, q *db.Queries, sessionID pgtype.UUID) bool {
	_, err := q.GetChannelChatSessionBindingBySessionAny(ctx, sessionID)
	return !errors.Is(err, pgx.ErrNoRows)
}

// Tick fires every wakeup whose deadline passed or whose condition now holds.
// A failure on one is logged and retried next tick; it never blocks the others.
func (s *ChatWakeupService) Tick(ctx context.Context) error {
	q := s.Tasks.Queries
	due, err := q.ListDueChatWakeups(ctx, chatWakeupTickBatch)
	if err != nil {
		return fmt.Errorf("list due chat wakeups: %w", err)
	}
	waiting, err := q.ListWaitingConditionalChatWakeups(ctx, chatWakeupTickBatch)
	if err != nil {
		return fmt.Errorf("list waiting chat wakeups: %w", err)
	}
	for _, w := range due {
		// At the deadline the condition still gets one look, so the turn says
		// which of the two ended the wait.
		reason := ""
		if len(w.Condition) > 0 {
			reason = "The deadline you set was reached and the condition had not held."
			if met, observed, err := s.conditionMet(ctx, w); err == nil && met {
				reason = conditionReason(w, observed)
			}
		}
		s.fireLogged(ctx, w, reason)
	}
	for _, w := range waiting {
		met, observed, err := s.conditionMet(ctx, w)
		if err != nil {
			slog.Warn("chat wakeup condition not evaluated", "wakeup_id", util.UUIDToString(w.ID), "error", err)
			continue
		}
		if met {
			s.fireLogged(ctx, w, conditionReason(w, observed))
		}
	}
	return nil
}

func (s *ChatWakeupService) fireLogged(ctx context.Context, w db.ChatWakeup, reason string) {
	if err := s.fire(ctx, w, reason); err != nil && !errors.Is(err, errChatWakeupRetry) {
		slog.Warn("chat wakeup not fired", "wakeup_id", util.UUIDToString(w.ID), "error", err)
	}
}

func conditionReason(w db.ChatWakeup, observed string) string {
	var c chatWakeupCondition
	_ = json.Unmarshal(w.Condition, &c)
	return fmt.Sprintf("The condition you set held (%s): %s.", c.Identifier, observed)
}

// fire starts one wakeup's turn in the DM's current session. Preflight reads
// (continuation chain, agent, attribution) run before the transaction; the
// transaction re-checks them under the session lock.
func (s *ChatWakeupService) fire(ctx context.Context, w db.ChatWakeup, reason string) error {
	q := s.Tasks.Queries
	expire := func() error {
		return q.SettleChatWakeup(ctx, db.SettleChatWakeupParams{ID: w.ID, Status: "expired"})
	}
	sessionID := w.ChatSessionID
	var session db.ChatSession
	for hop := 0; ; hop++ {
		var err error
		if session, err = q.GetChatSession(ctx, sessionID); errors.Is(err, pgx.ErrNoRows) {
			return expire()
		} else if err != nil {
			return err
		}
		if !session.ContinuedIntoSessionID.Valid {
			break
		}
		if hop == chatWakeupMaxHops {
			return fmt.Errorf("continuation chain longer than %d", chatWakeupMaxHops)
		}
		sessionID = session.ContinuedIntoSessionID
	}
	if session.Status != "active" || s.channelBound(ctx, q, session.ID) {
		return expire()
	}
	agent, err := q.GetAgent(ctx, session.AgentID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (agent.ArchivedAt.Valid || !agent.RuntimeID.Valid)) {
		return expire()
	} else if err != nil {
		return err
	}
	plan, err := s.Tasks.PlanDirectChatSend(ctx, session.ID, agent, session.CreatorID)
	if err != nil {
		return err
	}

	var started *db.AgentTaskQueue
	err = s.Tasks.runInTx(ctx, func(qtx *db.Queries) error {
		// Session first, then the wakeup row: DM deletion takes them in that
		// order, and the opposite order deadlocks with it.
		current, err := qtx.LockChatSessionForEnqueue(ctx, session.ID)
		if err != nil {
			return err
		}
		if current.Status != "active" || current.ContinuedIntoSessionID.Valid {
			return errChatWakeupRetry // the DM moved since the preflight
		}
		locked, err := qtx.LockPendingChatWakeup(ctx, w.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // cancelled or taken by another tick
		} else if err != nil {
			return err
		}
		if locked.ChatSessionID != current.ID {
			if err := qtx.MoveChatWakeupToSession(ctx, db.MoveChatWakeupToSessionParams{ID: locked.ID, ChatSessionID: current.ID}); err != nil {
				return err
			}
		}
		sent, err := s.Tasks.sendDirectChatMessageTx(ctx, qtx, plan, current, current.CreatorID, chatWakeupPrompt(locked, reason), nil, "", pgtype.UUID{}, "system", true)
		if err != nil {
			return err
		}
		started = &sent.Task
		return qtx.SettleChatWakeup(ctx, db.SettleChatWakeupParams{ID: locked.ID, Status: "fired", TaskID: sent.Task.ID})
	})
	switch {
	case errors.Is(err, ErrChatCompacting):
		return errChatWakeupRetry // the compaction continues the DM; the next tick follows it
	case errors.Is(err, ErrChatSessionArchived), errors.Is(err, ErrChatTaskAgentArchived), errors.Is(err, ErrChatTaskAgentNoRuntime):
		return expire()
	case err != nil:
		return err
	}
	if started != nil {
		s.Tasks.NotifyDirectChatSent(ctx, *started)
	}
	return nil
}

// chatWakeupPrompt is the hidden input of a wakeup turn: the agent's own
// instruction, framed so it is not mistaken for something the person typed.
// reason says why the turn starts now when a condition was set.
func chatWakeupPrompt(w db.ChatWakeup, reason string) string {
	if reason != "" {
		reason += " "
	}
	return fmt.Sprintf("Scheduled wakeup, not a message from the person. You set it earlier in this DM for %s. %s"+
		"Carry out your instruction, then reply to the person here.\n\nYour instruction:\n\n%s",
		w.FireAt.Time.UTC().Format(time.RFC3339), reason, w.Instruction)
}
