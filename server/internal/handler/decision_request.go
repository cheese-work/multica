package handler

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// askIdentity is the caller's ask identity. A structured ask leaves out the
// time its state was read, which a re-ask changes (CHE-7007).
func (input decisionRequestInput) askIdentity() string {
	if input.Parsed != nil {
		return input.Parsed.IdentityWithoutReadTime()
	}
	return decisiontx.Identity(input.Ask)
}

type decisionRequestInput struct {
	SessionID   pgtype.UUID
	NewDM       bool
	HumanUserID pgtype.UUID
	Ask         string
	Footer      string
	// CardLintMode is the workspace decision_card_lint_mode for a lint-eligible
	// ask; off or empty leaves the comment as written. Out of the fingerprint.
	CardLintMode    string `json:"-"`
	ParentID        pgtype.UUID
	EscalationClass string
	SourceCommentID pgtype.UUID
	// Recovers is the ask guard event this request answers; the request clears
	// it, for the person it asks, in its own transaction. It stays out of the
	// legacy fingerprint, which is pinned.
	Recovers    pgtype.UUID `json:"-"`
	Fingerprint string
	// Kind and ExpiresInSeconds are set for an action request only; they stay out of the legacy fingerprint.
	// Covers are the open cards this batch card answers for (CHE-4453).
	Covers           []pgtype.UUID `json:"-"`
	Kind             string        `json:"-"`
	ExpiresInSeconds float64       `json:"-"`
	// Parsed is the decoded caller ask. When set, the request is fingerprinted
	// from it and enriched once, for a new request. Nil means finished text.
	Parsed *decisiontx.Ask `json:"-"`
	Slug   string          `json:"-"`
	// LegacyFingerprint is the pre-D1d key, still looked up so a retry that
	// crosses the deploy replays its stored row.
	LegacyFingerprint string               `json:"-"`
	Action            *decisionActionDraft `json:",omitempty"`
	// Options are the card's answer buttons; the flat Ask carries their labels.
	Options []decisiontx.AskOption
	// AskJSON is the decoded structured Ask, stored whole for the card to render.
	// It stays out of the legacy fingerprint, which is pinned.
	AskJSON []byte `json:"-"`
	// Proposal is set only by the action proposal route.
	Proposal *proposalRequest `json:"-"`
	// DMSession is the calling chat task's session: the card renders there, for
	// the session's creator only. Invalid for an issue task.
	DMSession pgtype.UUID `json:"-"`
	// Batch is set only by the batch proposal route (CHE-5412).
	Batch *batchCard `json:"-"`
	// Wait asks to park the issue while the request is open under
	// decision_ask_park=on_wait (CHE-6460). Out of the fingerprint.
	Wait bool `json:"-"`
	// OwnerScript is the owner-script guard's finished check (CHE-7138 S7a); its
	// event is written with the message. Nil when the guard is off or no agent
	// text is read.
	OwnerScript *ownerScriptCheck `json:"-"`
	// ReaskOf is the earlier card this ask continues (--reask-of, CHE-6433) and
	// NewTopic says the ask is a different matter (--new-topic). Out of the
	// fingerprint: a re-ask is told apart by its parent, the root of that card's thread.
	ReaskOf  pgtype.UUID `json:"-"`
	NewTopic bool        `json:"-"`
	// ReaskSession is the chat session of the card ReaskOf names, read before the
	// transaction so the lock order can place it among the sessions.
	ReaskSession pgtype.UUID `json:"-"`
	// AskRaw is the ask as the caller sent it; the re-ask gate keys its guard row on it.
	AskRaw []byte `json:"-"`
	// Reask is the card ReaskOf names, locked in the request transaction. insertDecisionAsk
	// withdraws it when it is still open.
	Reask *db.DecisionRequest `json:"-"`
}

// batchCard ties the requests of one batch to one shared card: they share the
// batch id and the card's chat message. The lead request writes the message
// and records the one ping; the others point at it.
type batchCard struct {
	ID, MessageID pgtype.UUID
	Ask           string
	Lead          bool
}

func (card *batchCard) batchID() pgtype.UUID {
	if card == nil {
		return pgtype.UUID{}
	}
	return card.ID
}

// writesCard is true for a request that is not part of a batch, or leads one.
func (card *batchCard) writesCard() bool { return card == nil || card.Lead }

// dmSession is the chat session a request from a DM renders in: set by a
// plain request or a proposal made from a chat task, invalid otherwise.
func (input decisionRequestInput) dmSession() pgtype.UUID {
	if input.DMSession.Valid {
		return input.DMSession
	}
	return input.Proposal.chatSession()
}

type decisionRequestResult struct {
	Request          db.DecisionRequest
	Comment          db.Comment
	AttachedToIntent bool
	Message          db.ChatMessage
	ClearedComments  []db.Comment
	CreatedSession   *db.ChatSession
	// BackingLane is true when the request sits in a hidden decision backing lane: nothing about
	// the lane is announced to the member. Warnings go back to the caller as hints.
	BackingLane bool
	Warnings    []string
	Replayed    bool
	// Withdrawn is the card a re-ask replaced; its events go out after the commit.
	Withdrawn *decisionRequestResult
	Action    *db.DecisionAction
	// PingItem is set only when the ping was recorded inside the request
	// transaction (single-call path); PingRecorded distinguishes "recorded,
	// no item for a departed recipient" from "not yet recorded".
	PingItem     *db.InboxItem
	PingRecorded bool
	// ParkedActivity is the status row of the move to the waiting status, nil
	// when this request made none; ParkedIssue is the issue as written.
	ParkedIssue    db.Issue
	ParkedActivity *db.ActivityLog
	// AskStub is the system reply a card placed in a new thread (decision_card_thread)
	// left in its source thread; nil when the card has no source.
	AskStub *db.CreateCommentRow
}

// createDecisionRequest creates the request, or finds the same-task retry of
// it. A new request clears --recovers inside its own transaction; a replay never
// reaches that code, so it validates and clears here. Clearing is idempotent.
func (handler *Handler) createDecisionRequest(request *http.Request, issue db.Issue, input decisionRequestInput) (decisionRequestResult, error) {
	result, err := handler.createDecisionRequestOnce(request, issue, input)
	if err != nil || !result.Replayed {
		return result, err
	}
	taskID, _ := util.ParseUUID(request.Header.Get("X-Task-ID")) // createDecisionRequestOnce already required it
	return result, clearAskGuardEvent(request.Context(), handler.Queries, issue, input.Recovers, result.Request.HumanUserID, result.Request.ID, taskID)
}

func (handler *Handler) createDecisionRequestOnce(request *http.Request, issue db.Issue, input decisionRequestInput) (decisionRequestResult, error) {
	empty := decisionRequestResult{}
	deny := decisionIntentRefusal{http.StatusForbidden, "decision request requires this issue's running task"}
	if request.Header.Get("X-Actor-Source") != "task_token" {
		return empty, deny
	}
	taskID, err := util.ParseUUID(request.Header.Get("X-Task-ID"))
	if err != nil {
		return empty, deny
	}
	agentID, err := util.ParseUUID(request.Header.Get("X-Agent-ID"))
	if err != nil {
		return empty, deny
	}
	if input.NewDM && (input.SessionID.Valid || !input.HumanUserID.Valid) || strings.TrimSpace(input.Ask) == "" {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "a listed lane, a new DM or a named person, and a nonempty ask, are required"}
	}
	switch input.EscalationClass {
	case "reversible", "spend", "deploy", "publish", "irreversible", "product_direction":
	default:
		return empty, decisionIntentRefusal{http.StatusBadRequest, "invalid escalation class"}
	}
	if err := handler.refuseReversibleAsk(request.Context(), issue, input); err != nil {
		return empty, err
	}
	if input.HumanUserID.Valid {
		var err error
		if input.HumanUserID, err = handler.resolveDecisionRecipient(request.Context(), issue, input.HumanUserID); err != nil {
			return empty, err
		}
	}
	if input, err = handler.resolveReask(request.Context(), issue, input); err != nil {
		return empty, err
	}
	if input.SourceCommentID.Valid {
		task, err := handler.Queries.GetAgentTask(request.Context(), taskID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !runsDecisionRequest(task, agentID, issue, input.dmSession(), input.EscalationClass) {
			return empty, deny
		}
		if err != nil {
			return empty, err
		}
		if input.Fingerprint, input.LegacyFingerprint, err = input.fingerprints(); err != nil {
			return empty, err
		}
		if prior, err := findDecisionRequestRetry(request.Context(), handler.Queries, issue, agentID, taskID, input); err != nil || prior.Request.ID.Valid {
			return prior, err
		}
	}
	if input.OwnerScript, err = handler.checkOwnerScript(request.Context(), issue, agentID, taskID, input); err != nil {
		return empty, err
	}
	intent, err := handler.Queries.GetDecisionIntentForTask(request.Context(), db.GetDecisionIntentForTaskParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, TaskID: taskID, AgentID: agentID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	finish := func(run func() (decisionRequestResult, error)) (decisionRequestResult, error) {
		result, err := retryDecisionTx(request, run)
		if err != nil && input.Fingerprint != "" {
			prior, lookupErr := findDecisionRequestRetry(request.Context(), handler.Queries, issue, agentID, taskID, input)
			if lookupErr == nil && prior.Request.ID.Valid {
				return prior, nil
			}
		}
		return result, err
	}
	// A pending intent comment keeps the two-step flow; otherwise this call
	// opens, uses and seals its own window.
	if pending := err == nil && !intent.ConsumedAt.Valid && !intent.ClosedAt.Valid && !intent.SealedAt.Valid; pending {
		if input.ReaskOf.Valid {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "reask_of cannot go with an open intent comment; send the card without the intent"}
		}
		if input.SessionID.Valid == input.NewDM || input.ParentID.Valid {
			return empty, decisionIntentRefusal{http.StatusBadRequest, "a listed lane or an explicit new DM, and no parent, are required with an open intent"}
		}
		return finish(func() (decisionRequestResult, error) {
			return handler.createDecisionRequestTx(request, issue, input, intent)
		})
	}
	if !input.HumanUserID.Valid {
		if prior, err := findDecisionRequestRetry(request.Context(), handler.Queries, issue, agentID, taskID, input); err != nil || prior.Request.ID.Valid {
			return prior, err
		}
		return empty, decisionIntentRefusal{http.StatusForbidden, "decision request requires a named person (for) or an open intent naming the lane's person"}
	}
	return finish(func() (decisionRequestResult, error) {
		return handler.createSingleCallDecisionTx(request, issue, input, agentID)
	})
}

// runsDecisionRequest reports whether task is the agent's running task for this
// request: the issue's own task, the chat task of dmSession when the request is
// made from a DM (a chat task has no issue of its own), or the task that created
// the issue when the class is an escalation (createdIssueCard).
func runsDecisionRequest(task db.AgentTaskQueue, agentID pgtype.UUID, issue db.Issue, dmSession pgtype.UUID, class string) bool {
	if task.AgentID != agentID || task.Status != "running" || task.ChatSessionID != dmSession {
		return false
	}
	return dmSession.Valid || task.IssueID == issue.ID || createdIssueCard(task, issue, class)
}

// createdIssueCard lets the running task that created a new root issue open an
// escalation card on it (CHE-6911). A held new issue gets no run, so without
// this the five escalation classes could not start at once. Narrow by design:
// a class other than reversible, and an issue whose server-stamped origin is
// this very task. The caller still proves the task is running and the agent's.
func createdIssueCard(task db.AgentTaskQueue, issue db.Issue, class string) bool {
	return class != "" && class != "reversible" && issue.OriginType.String == "agent_create" && issue.OriginID == task.ID
}

// resolveDecisionRecipient maps the named id to the active member's user id.
func (handler *Handler) resolveDecisionRecipient(ctx context.Context, issue db.Issue, id pgtype.UUID) (pgtype.UUID, error) {
	member, err := handler.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: id, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		member, err = handler.Queries.GetMember(ctx, id)
	}
	if errors.Is(err, pgx.ErrNoRows) || err == nil && member.WorkspaceID != issue.WorkspaceID {
		return id, decisionIntentRefusal{http.StatusBadRequest, "named person is not an active workspace member: " + uuidToString(id)}
	}
	return member.UserID, err
}

// fingerprints returns the retry key of this call and the pre-D1d key. A caller
// ask is keyed by its decoded input alone, so agent renames, comment edits and
// the rendered text never change it. Finished text keeps the old key only.
func (input decisionRequestInput) fingerprints() (current, legacy string, err error) {
	hash := func(value any) (string, error) {
		encoded, err := json.Marshal(value)
		digest := sha256.Sum256(encoded)
		return hex.EncodeToString(digest[:]), err
	}
	if legacy, err = hash(input); err != nil || input.Parsed == nil {
		return legacy, "", err
	}
	current, err = hash(struct {
		Ask                                      decisiontx.Ask
		Footer, EscalationClass                  string
		NewDM                                    bool
		SessionID, HumanUserID, ParentID, Source pgtype.UUID
		ExpiresInSeconds                         float64 `json:",omitempty"`
	}{*input.Parsed, input.Footer, input.EscalationClass, input.NewDM, input.SessionID, input.HumanUserID, input.ParentID, input.SourceCommentID, input.ExpiresInSeconds})
	return "v2:" + current, legacy, err
}

func findDecisionRequestRetry(ctx context.Context, queries *db.Queries, issue db.Issue, agentID, taskID pgtype.UUID, input decisionRequestInput) (decisionRequestResult, error) {
	for _, fingerprint := range []string{input.Fingerprint, input.LegacyFingerprint} {
		if fingerprint == "" {
			continue
		}
		prior, err := queries.FindDecisionRequestRetry(ctx, db.FindDecisionRequestRetryParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: agentID, TaskID: taskID, Fingerprint: fingerprint})
		if err == nil {
			return decisionRequestResult{Request: prior, Replayed: true}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return decisionRequestResult{}, err
		}
	}
	return decisionRequestResult{}, nil
}

// enrichDecisionAsk renders the stored ask of a new request: the asking agent's
// name and comment excerpts are read once, here, and stay a snapshot.
func enrichDecisionAsk(ctx context.Context, queries *db.Queries, issue db.Issue, agentID pgtype.UUID, input decisionRequestInput) (decisionRequestInput, error) {
	if input.Parsed == nil {
		return input, nil
	}
	agent, err := queries.GetAgent(ctx, agentID)
	if err != nil {
		return input, err
	}
	ask := *input.Parsed
	ask.AskedBy = agent.Name
	ask, err = ask.WithExcerpts(func(commentID string) (string, error) {
		id, err := util.ParseUUID(commentID)
		if err != nil {
			return "", nil
		}
		comment, err := queries.GetComment(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (comment.WorkspaceID != issue.WorkspaceID || comment.DeletedAt.Valid) {
			return "", nil
		}
		return comment.Content, err
	})
	if err != nil {
		return input, err
	}
	input.Ask = ask.Content(input.Slug)
	if len(decisionAskContent(input)) > maxCommentContentBytes {
		return input, decisionIntentRefusal{http.StatusBadRequest, "ask is too long"}
	}
	return input, nil
}

func retryDecisionTx(request *http.Request, run func() (decisionRequestResult, error)) (decisionRequestResult, error) {
	deadlockRetried := false
	for attempt := 0; ; attempt++ {
		result, err := run()
		if err == nil || !decisiontx.IsRetryable(err) || attempt == 2 {
			return result, err
		}
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "40P01" {
			if deadlockRetried {
				return decisionRequestResult{}, err
			}
			deadlockRetried = true
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-request.Context().Done():
			return decisionRequestResult{}, request.Context().Err()
		}
	}
}

// decisionAskContent is the ask comment body; the DM message carries the ask only.
func decisionAskContent(input decisionRequestInput) string {
	if strings.TrimSpace(input.Footer) == "" {
		return input.Ask
	}
	return input.Ask + "\n\n" + input.Footer
}

// lockDecisionLane locks an existing DM and proves it is a listed decision lane
// of the agent or of the topic.
func lockDecisionLane(ctx context.Context, queries *db.Queries, issue db.Issue, topic decisionTopic, sessionID, agentID pgtype.UUID) (db.ChatSession, error) {
	workspaceID := issue.WorkspaceID
	deny := decisionIntentRefusal{http.StatusForbidden, "decision request window or lane is unavailable"}
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: workspaceID, ID: sessionID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ChatSession{}, deny
		}
		return db.ChatSession{}, err
	}
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{ID: sessionID, WorkspaceID: workspaceID})
	if err != nil {
		return db.ChatSession{}, err
	}
	if !session.DecisionTopicID.Valid && session.AgentID != agentID || session.Status != "active" || session.ChannelOriginAt.Valid || session.IsAgentIntro {
		return db.ChatSession{}, deny
	}
	lanes, err := queries.ListDecisionLanes(ctx, db.ListDecisionLanesParams{WorkspaceID: workspaceID, AgentID: agentID, TopicID: topic.ID, CreatorID: session.CreatorID, SessionID: session.ID})
	if err != nil {
		return db.ChatSession{}, err
	}
	if len(lanes) == 0 {
		return db.ChatSession{}, deny
	}
	if _, err := queries.GetChannelChatSessionBindingBySessionAny(ctx, session.ID); err == nil {
		return db.ChatSession{}, deny
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.ChatSession{}, err
	}
	return session, nil
}

func (handler *Handler) createDecisionRequestTx(request *http.Request, issue db.Issue, input decisionRequestInput, located db.DecisionIntent) (decisionRequestResult, error) {
	ctx := request.Context()
	empty := decisionRequestResult{}
	deny := decisionIntentRefusal{http.StatusForbidden, "decision request window or lane is unavailable"}
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	var createdSession *db.ChatSession
	var session db.ChatSession
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, issue.WorkspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, deny
		}
		return empty, err
	}
	if prior, err := findDecisionRequestRetry(ctx, queries, issue, located.AgentID, located.TaskID, input); err != nil || prior.Request.ID.Valid {
		return prior, err
	}
	topic, err := resolveDecisionTopic(ctx, queries, issue)
	if err != nil {
		return empty, err
	}
	backing, err := handler.issueAskGoesToBackingLane(ctx, queries, issue, input.dmSession().Valid)
	if err != nil {
		return empty, err
	}
	newDM := input.NewDM
	if backing {
		// session_id and new_dm only name the person: the ask goes to that person's backing lane.
		humanID := input.HumanUserID
		if !humanID.Valid {
			named, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{ID: input.SessionID, WorkspaceID: issue.WorkspaceID})
			if errors.Is(err, pgx.ErrNoRows) {
				return empty, deny
			}
			if err != nil {
				return empty, err
			}
			humanID = named.CreatorID
		}
		agent, targetID, err := handler.decisionLaneWindow(request, queries, issue, humanID)
		if err != nil {
			return empty, err
		}
		var found bool
		if session, found, err = backingLane(ctx, queries, issue, topic, targetID); err != nil {
			return empty, err
		}
		if newDM = !found; newDM {
			steward, err := handler.decisionSteward(ctx, queries, topic, agent, targetID)
			if err != nil {
				return empty, err
			}
			session = db.ChatSession{AgentID: steward.ID, CreatorID: targetID, Title: "Decisions: " + topic.Title, DecisionTopicID: topic.ID}
		}
	} else if newDM {
		agent, targetID, err := handler.decisionLaneWindow(request, queries, issue, input.HumanUserID)
		if err != nil {
			return empty, err
		}
		var found bool
		if session, found, err = topicLane(ctx, queries, issue, topic, targetID, located.AgentID); err != nil {
			return empty, err
		}
		if newDM = !found; newDM {
			steward, err := handler.decisionSteward(ctx, queries, topic, agent, targetID)
			if err != nil {
				return empty, err
			}
			session = db.ChatSession{AgentID: steward.ID, CreatorID: targetID, Title: "Decisions: " + topic.Title, DecisionTopicID: topic.ID}
		}
	} else {
		session, err = lockDecisionLane(ctx, queries, issue, topic, input.SessionID, located.AgentID)
		if err != nil {
			return empty, err
		}
		if input.HumanUserID.Valid && session.CreatorID != input.HumanUserID {
			return empty, deny
		}
		if err := requireLaneInTopic(ctx, queries, issue, topic, session); err != nil {
			return empty, err
		}
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, issue.WorkspaceID, located.CommentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, deny
		}
		return empty, err
	}
	issue, err = queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID})
	if err != nil {
		return empty, err
	}
	if prior, err := findDecisionRequestRetry(ctx, queries, issue, located.AgentID, located.TaskID, input); err != nil || prior.Request.ID.Valid {
		return prior, err
	}
	category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, issue.Status)
	if err != nil {
		return empty, err
	}
	if category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return empty, decisionIntentRefusal{http.StatusForbidden, "decision request refuses a terminal issue"}
	}
	if err := requireNoRecentReask(ctx, queries, issue, located.AgentID, session.CreatorID, input.askIdentity()); err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionCallingTask(ctx, db.LockDecisionCallingTaskParams{WorkspaceID: issue.WorkspaceID, ID: located.TaskID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: issue.WorkspaceID, UserID: session.CreatorID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, deny
		}
		return empty, err
	}
	if err := requireDecisionAuthority(ctx, queries, issue, session.CreatorID, input.EscalationClass, http.StatusConflict); err != nil {
		return empty, err
	}
	intent, err := queries.LockDecisionIntent(ctx, db.LockDecisionIntentParams{WorkspaceID: issue.WorkspaceID, ID: located.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, deny
	}
	if err != nil {
		return empty, err
	}
	if intent.IssueID != issue.ID || intent.TaskID != located.TaskID || intent.AgentID != located.AgentID || intent.CommentID != located.CommentID {
		return empty, deny
	}
	agent, _, windowError := handler.decisionLaneWindow(request, queries, issue, session.CreatorID)
	if err := decisiontx.ValidateIntent(ctx, transaction, intent); err != nil {
		if errors.Is(err, decisiontx.ErrIntentClosed) {
			handler.DecisionWorker.Notify()
			return empty, decisionIntentRefusal{http.StatusForbidden, err.Error()}
		}
		return empty, err
	}
	if windowError != nil {
		return empty, windowError
	}
	if root == nil || agent.Kind == "system" {
		return empty, deny
	}
	tag, err := queries.GetComment(ctx, intent.CommentID)
	if err != nil {
		return empty, err
	}
	if tag.WorkspaceID != issue.WorkspaceID || tag.IssueID != issue.ID || tag.AuthorType != "agent" || tag.AuthorID != intent.AgentID || tag.SourceTaskID != intent.TaskID || tag.DecisionIntentID != intent.ID {
		return empty, deny
	}
	target, err := queries.LockDecisionIntentTarget(ctx, db.LockDecisionIntentTargetParams{WorkspaceID: issue.WorkspaceID, DecisionIntentID: intent.ID, HumanUserID: session.CreatorID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && target.ConsumedAt.Valid) {
		return empty, deny
	}
	if err != nil {
		return empty, err
	}
	if newDM {
		session, err = createDecisionDM(ctx, queries, issue, session)
		if err != nil {
			return empty, err
		}
		createdSession = &session
		if backing {
			if err := queries.CreateDecisionBackingLane(ctx, db.CreateDecisionBackingLaneParams{SessionID: session.ID, WorkspaceID: issue.WorkspaceID, CreatorID: session.CreatorID, TopicID: topic.ID}); err != nil {
				return empty, err
			}
		}
	}
	result, err := insertDecisionAsk(ctx, queries, issue, input, intent, session, createdSession != nil, root, intent.CommentID, dbid.NewV7(), &tag)
	if err != nil {
		return empty, err
	}
	if backing {
		result.BackingLane = true
		result.Warnings = append(result.Warnings, "decision_screen is on: session_id and new_dm are ignored; the ask goes to the member's decision backing lane")
	}
	if result.ParkedIssue, result.ParkedActivity, err = parkForDecisionRequest(ctx, queries, issue, input.Wait); err != nil {
		return empty, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	result.CreatedSession = createdSession
	return result, nil
}

func createDecisionDM(ctx context.Context, queries *db.Queries, issue db.Issue, lane db.ChatSession) (db.ChatSession, error) {
	session, err := queries.CreateChatSession(ctx, db.CreateChatSessionParams{ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, AgentID: lane.AgentID, CreatorID: lane.CreatorID, Title: lane.Title, DecisionTopicID: lane.DecisionTopicID})
	if err != nil {
		return session, err
	}
	_, err = queries.MarkChatSessionExplicitlyCreated(ctx, session.ID)
	return session, err
}

// insertDecisionAsk writes the request, ask comment, DM message and consumes
// the intent target; the caller commits. With tag set (two-step path) an ask
// identical to the intent comment is that comment: it is reused instead of
// posting a second copy, unless a footer must be added. A comment carries one
// request, so later targets of a group intent get their own ask comment. With
// tag nil (single-call path) the ask comment is the intent's own anchor.
func insertDecisionAsk(ctx context.Context, queries *db.Queries, issue db.Issue, input decisionRequestInput, intent db.DecisionIntent, session db.ChatSession, newSession bool, root *db.Comment, parentID, commentID pgtype.UUID, tag *db.Comment) (decisionRequestResult, error) {
	empty := decisionRequestResult{}
	var sourceSnapshot string
	if input.SourceCommentID.Valid {
		source, err := queries.LockDecisionSourceComment(ctx, db.LockDecisionSourceCommentParams{WorkspaceID: issue.WorkspaceID, ID: input.SourceCommentID})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && source.IssueID != issue.ID {
			return empty, decisionIntentRefusal{http.StatusNotFound, "ask source comment not found on this issue"}
		}
		if err != nil {
			return empty, err
		}
		sourceSnapshot = source.Content
	}
	attach := tag != nil && !tag.DecisionRequestID.Valid && strings.TrimSpace(input.Footer) == "" && strings.TrimSpace(tag.Content) == strings.TrimSpace(input.Ask)
	if attach {
		commentID = tag.ID
	}
	askIdentity := input.askIdentity()
	input, err := enrichDecisionAsk(ctx, queries, issue, intent.AgentID, input)
	if err != nil {
		return empty, err
	}
	options, err := json.Marshal(append([]decisiontx.AskOption{}, input.Options...))
	if err != nil {
		return empty, err
	}
	messageID := dbid.NewV7()
	if input.Batch != nil {
		messageID = input.Batch.MessageID
	}
	created, err := queries.CreateDecisionRequest(ctx, db.CreateDecisionRequestParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AskingAgentID: intent.AgentID, HumanUserID: session.CreatorID, ChatSessionID: session.ID, PingMessageID: messageID, IssueCommentID: commentID, Ask: input.Ask, Options: options, AskJson: input.AskJSON, EscalationClass: input.EscalationClass, Origin: "asked", DecisionIntentID: intent.ID, SourceCommentID: input.SourceCommentID, SourceContentSnapshot: sourceSnapshot, RequestFingerprint: input.Fingerprint, AskIdentity: askIdentity, ProjectID: issue.ProjectID, Kind: pgtype.Text{String: input.Kind, Valid: input.Kind != ""}, ExpiresInSeconds: pgtype.Float8{Float64: input.ExpiresInSeconds, Valid: input.ExpiresInSeconds > 0}, ProposalFingerprint: input.Proposal.fingerprintText(), BatchID: input.Batch.batchID(), ReaskOfRequestID: input.ReaskOf})
	if errors.Is(err, pgx.ErrNoRows) && input.Proposal != nil {
		return empty, errProposalRaced
	}
	if err != nil {
		return empty, err
	}
	if created, err = coverBatchRequests(ctx, queries, created, input.Covers); err != nil {
		return empty, err
	}
	if err := clearAskGuardEvent(ctx, queries, issue, input.Recovers, session.CreatorID, created.ID, intent.TaskID); err != nil {
		return empty, err
	}
	if err := serveAskGuardEvents(ctx, queries, issue, session.CreatorID, intent.TaskID); err != nil {
		return empty, err
	}
	if err := recordDecisionObservation(ctx, queries, issue, created, input.Parsed); err != nil {
		return empty, err
	}
	var action *db.DecisionAction
	if input.Action != nil {
		stored, err := createDecisionAction(ctx, queries, issue, created, intent.TaskID, input.Action)
		if err != nil {
			return empty, err
		}
		action = &stored
	}
	// decision_card_thread=on (T3): the card is its own thread and mentions the asked
	// person. A DM agent's ask stays in its DM, and an ask that attaches to an intent
	// comment keeps that comment's place.
	cardThread := false
	if !input.dmSession().Valid {
		workspace, err := queries.GetWorkspace(ctx, issue.WorkspaceID)
		if err != nil {
			return empty, err
		}
		if cardThread, err = decisiontx.CardThreadOn(workspace.Settings); err != nil {
			return empty, err
		}
	}
	var withdrawn *decisionRequestResult
	if input.Reask != nil {
		// The card the re-ask replaces closes in this transaction, as the asking agent.
		result, err := withdrawOpenDecision(ctx, queries, *input.Reask, root, reaskSupersededPrefix+uuidToString(created.ID), "agent", intent.AgentID, intent.TaskID, pgtype.UUID{})
		if err != nil {
			return empty, err
		}
		withdrawn = &result
	}
	var askSource pgtype.UUID
	if cardThread && tag == nil && !input.ReaskOf.Valid {
		// A new root: the parent only names the source thread, so the reply rules
		// (resolution clear, parent link) do not apply.
		if root != nil {
			askSource = root.ID
		}
		root, parentID = nil, pgtype.UUID{}
	}
	var cleared []db.Comment
	if root != nil {
		cleared, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{ThreadRootID: root.ID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return empty, err
		}
	}
	content := decisionAskContent(input)
	if input.CardLintMode != "" && input.CardLintMode != "off" || cardThread {
		human, err := queries.GetUser(ctx, session.CreatorID)
		if err != nil {
			return empty, err
		}
		var added bool
		if content, added = decisiontx.MentionHuman(content, human.Name, uuidToString(session.CreatorID)); added {
			slog.Info("decision card lint", "code", "mention_missing", "issue_id", uuidToString(issue.ID), "request_id", uuidToString(created.ID))
		}
	}
	var comment db.Comment
	if attach {
		comment, err = queries.AttachDecisionAskToComment(ctx, db.AttachDecisionAskToCommentParams{WorkspaceID: issue.WorkspaceID, CommentID: tag.ID, DecisionRequestID: created.ID, Content: content})
	} else {
		commentParams := db.CreateCommentParams{ID: commentID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "agent", AuthorID: intent.AgentID, SourceTaskID: intent.TaskID, ParentID: parentID, Content: content, Type: "comment", DecisionRequestID: created.ID, DecisionCommentKind: pgtype.Text{String: "ask", Valid: true}, AskSourceRootID: askSource}
		if tag == nil {
			commentParams.DecisionIntentID = intent.ID
		}
		var row db.CreateCommentRow
		row, err = queries.CreateComment(ctx, commentParams)
		comment = row.Comment()
	}
	if err != nil {
		return empty, err
	}
	var askStub *db.CreateCommentRow
	var warnings []string
	if askSource.Valid {
		stub, err := queries.CreateComment(ctx, askSourceStubParams(issue, askSource, comment.ID, content))
		if err != nil {
			return empty, err
		}
		askStub = &stub
		warnings = append(warnings, fmt.Sprintf("Card placed in a new thread %s; thread %s got a link.", uuidToString(comment.ID), uuidToString(askSource)))
	}
	var message db.ChatMessage
	if input.Batch.writesCard() {
		cardAsk := input.Ask
		if input.Batch != nil {
			cardAsk = input.Batch.Ask
		}
		message, err = queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: messageID, ChatSessionID: session.ID, Role: "assistant", Content: cardAsk, MessageKind: pgtype.Text{String: "decision_request", Valid: true}, SentVia: pgtype.Text{String: "task_token", Valid: true}})
		if err != nil {
			return empty, err
		}
		if newSession {
			if err := queries.MarkChatSessionReadBefore(ctx, db.MarkChatSessionReadBeforeParams{ID: session.ID, MessageCreatedAt: message.CreatedAt}); err != nil {
				return empty, err
			}
		}
		if err := queries.TouchChatSession(ctx, session.ID); err != nil {
			return empty, err
		}
	}
	consumed, err := queries.ConsumeDecisionIntentTarget(ctx, db.ConsumeDecisionIntentTargetParams{WorkspaceID: issue.WorkspaceID, DecisionIntentID: intent.ID, HumanUserID: session.CreatorID, RequestID: created.ID})
	if err != nil {
		return empty, err
	}
	if consumed != 1 {
		return empty, decisionIntentRefusal{http.StatusForbidden, "decision request window or lane is unavailable"}
	}
	if err := queries.CompleteDecisionIntent(ctx, db.CompleteDecisionIntentParams{WorkspaceID: issue.WorkspaceID, ID: intent.ID, RequestID: created.ID}); err != nil {
		return empty, err
	}
	if input.OwnerScript != nil {
		if err := input.OwnerScript.writeDelivered(ctx, queries, issue, created, messageID, comment.ID); err != nil {
			return empty, err
		}
	}
	return decisionRequestResult{Request: created, Comment: comment, AttachedToIntent: attach, Message: message, ClearedComments: cleared, Action: action, AskStub: askStub, Warnings: warnings, Withdrawn: withdrawn}, nil
}

// createSingleCallDecisionTx is the request path without an intent comment: it
// runs the intent comment's checks inline against the named person, anchors a
// fresh intent on the ask comment itself, and consumes and seals it.
func (handler *Handler) createSingleCallDecisionTx(request *http.Request, issue db.Issue, input decisionRequestInput, agentID pgtype.UUID) (decisionRequestResult, error) {
	ctx := request.Context()
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return decisionRequestResult{}, err
	}
	defer transaction.Rollback(ctx)
	result, err := handler.singleCallDecisionIn(request, handler.Queries.WithTx(transaction), issue, input, agentID)
	if err != nil {
		var gate reaskGateError
		if errors.As(err, &gate) {
			// The refusal rolls the request back; its row is written on its own.
			transaction.Rollback(ctx)
			handler.recordReaskRefused(ctx, issue.WorkspaceID, gate)
		}
		return decisionRequestResult{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return decisionRequestResult{}, err
	}
	handler.DecisionWorker.Notify()
	return result, nil
}

// singleCallDecisionIn is the body of the single-call path inside the
// caller's transaction; the caller commits. The batch route runs it once per
// item in one transaction.
func (handler *Handler) singleCallDecisionIn(request *http.Request, queries *db.Queries, issue db.Issue, input decisionRequestInput, agentID pgtype.UUID) (decisionRequestResult, error) {
	ctx := request.Context()
	empty := decisionRequestResult{}
	deny := decisionIntentRefusal{http.StatusForbidden, "decision request window or lane is unavailable"}
	typed := input.Parsed != nil && len(input.Parsed.TypedOptionPaths()) > 0
	typedSendHook("before_fence")
	if typed {
		// First lock of the send: a settings write that turns a fenced key off waits for this
		// transaction, and a send that waits here holds no other lock.
		if err := queries.LockTypedSettingsFenceShared(ctx, issue.WorkspaceID); err != nil {
			return empty, err
		}
	}
	typedSendHook("after_fence")
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, issue.WorkspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, deny
		}
		return empty, err
	}
	typedSendHook("after_row_lock")
	if input.ReaskSession.Valid {
		// Lock order: workspace, session (this one and the lane's, lower id first), thread,
		// issue, task, member, request. A re-ask names no lane of its own, so the card's
		// session is the one it can lock first; a lane resolved below with a lower id than
		// the card's session is the one residual cycle (see the PR).
		if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: issue.WorkspaceID, ID: input.ReaskSession}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return empty, decisionCodedRefusal{http.StatusConflict, "reask_lane_gone", "the lane of the card to re-ask is gone; use --new-topic"}
			}
			return empty, err
		}
	}
	if typed {
		// The authoritative read of decision_server_actions: inside the transaction, after the locks.
		workspace, err := queries.GetWorkspace(ctx, issue.WorkspaceID)
		if err != nil {
			return empty, err
		}
		if !decisiontx.ServerActionsOn(workspace.Settings) {
			return empty, decisionIntentRefusal{http.StatusBadRequest, decisiontx.ServerActionsOffCode + ": " + input.Parsed.TypedOptionPaths()[0] + " needs the workspace setting decision_server_actions to be on; it is off"}
		}
		typedSendHook("after_setting_read")
	}
	topic, err := resolveDecisionTopic(ctx, queries, issue)
	if err != nil {
		return empty, err
	}
	var session db.ChatSession
	reuse := input.SessionID.Valid
	dmSession := input.dmSession()
	inDM := dmSession.Valid
	backing, err := handler.issueAskGoesToBackingLane(ctx, queries, issue, inDM)
	if err != nil {
		return empty, err
	}
	var warnings []string
	if backing {
		if input.SessionID.Valid || input.NewDM {
			warnings = append(warnings, "decision_screen is on: session_id and new_dm are ignored; the ask goes to the member's decision backing lane")
		}
		if session, reuse, err = backingLane(ctx, queries, issue, topic, input.HumanUserID); err != nil {
			return empty, err
		}
	} else if inDM {
		reuse = true
		if session, err = lockProposalDM(ctx, queries, issue, dmSession, agentID); err != nil {
			return empty, input.Proposal.laneError(err)
		}
		if session.CreatorID != input.HumanUserID {
			return empty, deny
		}
	} else if reuse {
		if session, err = lockDecisionLane(ctx, queries, issue, topic, input.SessionID, agentID); err != nil {
			return empty, err
		}
		if session.CreatorID != input.HumanUserID {
			return empty, deny
		}
		if err := requireLaneInTopic(ctx, queries, issue, topic, session); err != nil {
			return empty, err
		}
	} else if dmID, found, dmErr := latestDM(ctx, queries, issue, input, agentID); dmErr != nil {
		return empty, dmErr
	} else if found {
		// An issue task asks in the DM the person already has with the agent.
		reuse = true
		if session, err = lockProposalDM(ctx, queries, issue, dmID, agentID); err != nil {
			return empty, input.Proposal.laneError(err)
		}
	} else if session, reuse, err = topicLane(ctx, queries, issue, topic, input.HumanUserID, agentID); err != nil {
		return empty, input.Proposal.laneError(err)
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, issue.WorkspaceID, input.ParentID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && root != nil && root.IssueID != issue.ID {
		return empty, decisionIntentRefusal{http.StatusBadRequest, "invalid parent comment"}
	}
	if err != nil {
		return empty, err
	}
	taskIssue, agent, task, err := handler.authorizeDecisionTaskIn(request, queries, issue, "agent", agentID, dmSession, input.Proposal != nil && input.Action != nil && input.Action.Type == runBudgetRaiseType, input.EscalationClass)
	if input.Proposal != nil && (err != nil || input.Proposal.RecipientErr != nil || !reuse) {
		// A stopped task, a removed recipient or a lane that is gone cannot ask
		// again, but an open proposal of this identity is reported as
		// ineligible, not refused and not asked a second time.
		held, holdErr := holdsOpenProposal(ctx, queries, issue, agentID, input)
		if holdErr != nil {
			return empty, holdErr
		}
		if held {
			return empty, proposalIneligible(cmp.Or[error](err, input.Proposal.RecipientErr, errProposalLaneGone))
		}
	}
	if err != nil {
		return empty, err
	}
	if input.Proposal != nil && input.Proposal.RecipientErr != nil {
		return empty, input.Proposal.RecipientErr
	}
	issue = taskIssue
	if prior, err := findDecisionRequestRetry(ctx, queries, issue, agentID, task.ID, input); err != nil || prior.Request.ID.Valid {
		return prior, err
	}
	if input.Proposal != nil && reuse {
		// A new DM has no open proposal yet. Replay comes first: re-asking
		// guards would refuse the identical open ask.
		if prior, err := handler.replayProposal(request, queries, issue, agent, input, session); err != nil || prior.Request.ID.Valid {
			return prior, err
		}
	}
	if agent.Kind == "system" {
		return empty, deny
	}
	authorityStatus := http.StatusConflict
	if input.Proposal != nil {
		authorityStatus = http.StatusForbidden // D3's guard owns the re-ask rule
	} else if err := requireNoRecentReaskExcept(ctx, queries, issue, agentID, input.HumanUserID, input.askIdentity(), input.ReaskOf); err != nil {
		return empty, err
	}
	var gateHint string
	if input.Proposal == nil {
		if gateHint, err = reaskGate(ctx, queries, issue, input, task); err != nil {
			return empty, err
		}
	}
	if err := handler.checkDecisionTarget(request, queries, issue, agent, input.HumanUserID); err != nil {
		return empty, err
	}
	if err := requireDecisionAuthority(ctx, queries, issue, input.HumanUserID, input.EscalationClass, authorityStatus); err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionMember(ctx, db.LockDecisionMemberParams{WorkspaceID: issue.WorkspaceID, UserID: input.HumanUserID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, deny
		}
		return empty, err
	}
	if input, err = lockReask(ctx, queries, issue, input); err != nil {
		return empty, err
	}
	var createdSession *db.ChatSession
	if !reuse {
		steward, err := handler.decisionSteward(ctx, queries, topic, agent, input.HumanUserID)
		if err != nil {
			return empty, err
		}
		lane := db.ChatSession{AgentID: steward.ID, CreatorID: input.HumanUserID, Title: "Decisions: " + topic.Title, DecisionTopicID: topic.ID}
		if session, err = createDecisionDM(ctx, queries, issue, lane); err != nil {
			return empty, err
		}
		createdSession = &session
		if backing {
			if err := queries.CreateDecisionBackingLane(ctx, db.CreateDecisionBackingLaneParams{SessionID: session.ID, WorkspaceID: issue.WorkspaceID, CreatorID: session.CreatorID, TopicID: topic.ID}); err != nil {
				return empty, err
			}
		}
	}
	if input.Proposal != nil {
		if input.Proposal, err = handler.admitProposal(ctx, queries, issue, agent, session, input); err != nil {
			return empty, err
		}
	}
	if err := supersedeDecisionIntent(ctx, queries, issue, task.ID); err != nil {
		return empty, err
	}
	commentID := dbid.NewV7()
	intent, err := queries.CreateDecisionIntent(ctx, db.CreateDecisionIntentParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, TaskID: task.ID, AgentID: agent.ID, CommentID: commentID})
	if err != nil {
		return empty, err
	}
	if _, err := queries.CreateDecisionIntentTarget(ctx, db.CreateDecisionIntentTargetParams{DecisionIntentID: intent.ID, HumanUserID: input.HumanUserID}); err != nil {
		return empty, err
	}
	result, err := insertDecisionAsk(ctx, queries, issue, input, intent, session, createdSession != nil, root, input.ParentID, commentID, nil)
	if err != nil {
		return empty, err
	}
	if input.Action != nil && isRunBudgetActionType(input.Action.Type) {
		window, err := runBudgetWindowOf(input.Action.Params)
		if err != nil {
			return empty, err
		}
		if err := recordRunBudgetAsk(ctx, queries, issue, "agent", agent.ID, input.Action.Type, window); err != nil {
			return empty, err
		}
	}
	// The ping commits with the request: a ping failure refuses the whole call.
	// A batch pings once, for its lead; the others are recorded as handled.
	if input.Batch.writesCard() {
		result.PingItem, err = decisiontx.NotifyPingTx(ctx, queries, issue.WorkspaceID, result.Request.ID)
	} else {
		_, err = queries.MarkDecisionPingNotified(ctx, db.MarkDecisionPingNotifiedParams{WorkspaceID: issue.WorkspaceID, ID: result.Request.ID})
	}
	if err != nil {
		return empty, err
	}
	result.PingRecorded = true
	if !inDM { // the DM's task is not the issue's: the anchor stays as it is
		if result.ParkedIssue, result.ParkedActivity, err = parkForDecisionRequest(ctx, queries, issue, input.Wait); err != nil {
			return empty, err
		}
	}
	result.CreatedSession = createdSession
	result.BackingLane = backing
	result.Warnings = append(warnings, result.Warnings...)
	if gateHint != "" {
		result.Warnings = append(result.Warnings, gateHint)
	}
	return result, nil
}

// issueAskGoesToBackingLane is true for an issue task's ask while decision_screen is on. A DM
// task's ask stays in its DM (plan r4 3.7). The setting is read inside the request transaction.
func (handler *Handler) issueAskGoesToBackingLane(ctx context.Context, queries *db.Queries, issue db.Issue, inDM bool) (bool, error) {
	if inDM {
		return false, nil
	}
	workspace, err := queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return false, err
	}
	return decisiontx.DecisionScreenOn(workspace.Settings), nil
}

// backingLane finds and locks the member's backing lane for the topic. found is false when there
// is none yet; the caller creates it and registers it in the same transaction. The topic-lane
// advisory lock serialises two asks that would both create it.
func backingLane(ctx context.Context, queries *db.Queries, issue db.Issue, topic decisionTopic, humanID pgtype.UUID) (session db.ChatSession, found bool, err error) {
	if err := queries.LockDecisionTopicLane(ctx, db.LockDecisionTopicLaneParams{WorkspaceID: issue.WorkspaceID, CreatorID: humanID, TopicID: topic.ID}); err != nil {
		return db.ChatSession{}, false, err
	}
	id, err := queries.GetDecisionBackingLane(ctx, db.GetDecisionBackingLaneParams{WorkspaceID: issue.WorkspaceID, CreatorID: humanID, TopicID: topic.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ChatSession{}, false, nil
	}
	if err != nil {
		return db.ChatSession{}, false, err
	}
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: issue.WorkspaceID, ID: id}); err != nil {
		return db.ChatSession{}, false, err
	}
	session, err = queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{ID: id, WorkspaceID: issue.WorkspaceID})
	return session, err == nil, err
}

// latestDM finds the recipient's most recently active plain DM with the agent
// for a proposal made from an issue task (CHE-5253).
func latestDM(ctx context.Context, queries *db.Queries, issue db.Issue, input decisionRequestInput, agentID pgtype.UUID) (pgtype.UUID, bool, error) {
	if input.Proposal == nil {
		return pgtype.UUID{}, false, nil
	}
	id, err := queries.GetLatestPlainDM(ctx, db.GetLatestPlainDMParams{WorkspaceID: issue.WorkspaceID, CreatorID: input.HumanUserID, AgentID: agentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, false, nil
	}
	return id, err == nil, err
}
