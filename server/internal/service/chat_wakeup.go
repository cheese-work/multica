package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
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
	// chatWakeupConditionBudget bounds the condition pass of one tick; the job's run timeout is 45 s.
	chatWakeupConditionBudget = 10 * time.Second
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
// WakeupCondition, so the issue-wakeup evaluators decide status and merged
// facts. A checks_finished fact needs a NEW finished result, so it is tracked
// per pull request: Baseline is each linked pull request's "head:state:check-run identity" at
// creation (finished checks that already existed belong to an older push), and
// Armed lists the pull requests seen with checks still running since, so a
// same-head rerun counts when it finishes.
type chatWakeupCondition struct {
	IssueID    string               `json:"issue_id"`
	Identifier string               `json:"identifier"`
	Check      json.RawMessage      `json:"check,omitempty"`
	CommentBy  *ChatWakeupCommenter `json:"comment_by,omitempty"`
	Baseline   map[string]string    `json:"baseline,omitempty"`
	Armed      []string             `json:"armed,omitempty"`
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
			rows, err := s.Tasks.Queries.ListChatWakeupLinkedPullRequests(ctx, issue.ID)
			if err != nil {
				return nil, err
			}
			stored.Baseline = map[string]string{}
			for _, row := range rows {
				key := strconv.Itoa(int(row.PrNumber))
				if checksTerminal(row.HeadSha, row.Checks) {
					stored.Baseline[key] = row.HeadSha + ":" + row.Checks + ":" + row.ChecksIdentity
				} else if row.HeadSha != "" {
					stored.Armed = append(stored.Armed, key)
				}
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
	var check WakeupCondition
	if err := json.Unmarshal(c.Check, &check); err != nil {
		return false, "", err
	}
	if check.Type == "pull_request" && check.Event == "checks_finished" {
		return s.checksFinished(ctx, w, c, issueID)
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback(ctx)
	met, _, observed, err := (&IssueWakeupService{Tasks: s.Tasks}).evaluateCondition(ctx, tx, s.Tasks.Queries.WithTx(tx),
		db.IssueWakeup{IssueID: issueID, WorkspaceID: w.WorkspaceID, Condition: c.Check})
	if err != nil {
		if conditionDiagnostic(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if !met {
		return false, "", nil
	}
	facts, _ := json.Marshal(observed)
	return true, string(bytes.TrimSpace(facts)), nil
}

func checksTerminal(head, state string) bool {
	return head != "" && (state == "SUCCESS" || state == "FAILURE" || state == "ERROR")
}

// checksFinished holds when a linked pull request has a finished result that is
// new since the wakeup was set: a different head, result or check-run identity
// than the baseline, a pull request linked since, or a rerun seen running and
// now finished. The check-run identity (names, states and details URLs with job
// ids) changes on every rerun, so a rerun that starts and finishes between two
// ticks is still seen. Removing a link is never a new result. ponytail: a rerun
// whose check runs keep the same details URLs, such as a legacy status context
// with a fixed target, is seen only if a tick observes it running.
func (s *ChatWakeupService) checksFinished(ctx context.Context, w db.ChatWakeup, c chatWakeupCondition, issueID pgtype.UUID) (bool, string, error) {
	if !s.Tasks.PRSnapshotsEnabled {
		return false, "", nil
	}
	rows, err := s.Tasks.Queries.ListChatWakeupLinkedPullRequests(ctx, issueID)
	if err != nil {
		return false, "", err
	}
	armed := map[string]bool{}
	for _, key := range c.Armed {
		armed[key] = true
	}
	armedBefore := len(armed)
	var fresh []map[string]any
	for _, row := range rows {
		key := strconv.Itoa(int(row.PrNumber))
		if !checksTerminal(row.HeadSha, row.Checks) {
			if row.HeadSha != "" {
				armed[key] = true
			}
			continue
		}
		if base, known := c.Baseline[key]; !known || base != row.HeadSha+":"+row.Checks+":"+row.ChecksIdentity || armed[key] {
			fresh = append(fresh, map[string]any{"number": row.PrNumber, "head_sha": row.HeadSha, "checks": strings.ToLower(row.Checks)})
		}
	}
	if len(fresh) > 0 {
		facts, _ := json.Marshal(map[string]any{"pull_requests": fresh})
		return true, string(facts), nil
	}
	if len(armed) != armedBefore {
		c.Armed = c.Armed[:0]
		for key := range armed {
			c.Armed = append(c.Armed, key)
		}
		sort.Strings(c.Armed)
		updated, _ := json.Marshal(c)
		if err := s.Tasks.Queries.UpdateChatWakeupCondition(ctx, db.UpdateChatWakeupConditionParams{ID: w.ID, Condition: updated}); err != nil {
			return false, "", err
		}
	}
	return false, "", nil
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
	// Page through every waiting condition, least recently checked first, within
	// a time budget. A tick that runs out of budget resumes where it stopped on
	// the next one, because each check stamps the wakeup.
	var seen []pgtype.UUID
	budget := time.Now().Add(chatWakeupConditionBudget)
	for time.Now().Before(budget) {
		waiting, err := q.ListWaitingConditionalChatWakeups(ctx, db.ListWaitingConditionalChatWakeupsParams{Seen: seen, Batch: chatWakeupTickBatch})
		if err != nil {
			return fmt.Errorf("list waiting chat wakeups: %w", err)
		}
		if len(waiting) == 0 {
			break
		}
		for _, w := range waiting {
			seen = append(seen, w.ID)
			met, observed, err := s.conditionMet(ctx, w)
			if err != nil {
				slog.Warn("chat wakeup condition not evaluated", "wakeup_id", util.UUIDToString(w.ID), "error", err)
			}
			if err := q.TouchChatWakeupConditionChecked(ctx, w.ID); err != nil {
				slog.Warn("chat wakeup condition check not stamped", "wakeup_id", util.UUIDToString(w.ID), "error", err)
			}
			if err == nil && met {
				s.fireLogged(ctx, w, conditionReason(w, observed))
			}
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
	// Follow the continuation chain to the DM that is current now. It has no
	// fixed depth; a session met twice is a corrupt cycle, so the wakeup expires.
	sessionID := w.ChatSessionID
	var session db.ChatSession
	visited := map[pgtype.UUID]bool{}
	for {
		var err error
		if session, err = q.GetChatSession(ctx, sessionID); errors.Is(err, pgx.ErrNoRows) {
			return expire()
		} else if err != nil {
			return err
		}
		if !session.ContinuedIntoSessionID.Valid {
			break
		}
		if visited[session.ID] {
			return expire()
		}
		visited[session.ID] = true
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
