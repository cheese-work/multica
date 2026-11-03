package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

const askRepairBatch = 20

// askRepairFault lets a test stop an admission just before its commit.
var askRepairFault func(point string) error

func askRepairFaultAt(point string) error {
	if askRepairFault == nil {
		return nil
	}
	return askRepairFault(point)
}

// Why a root failed. The agent that produced the event owns the failure.
const (
	askRepairAgentUnavailable = "agent_unavailable"
	askRepairRefused          = "admission_refused"
	askRepairCreatedAsk       = "repair_created_ask"
	askRepairNoRequest        = "no_matching_request"
)

var askRepairFailureText = map[string]string{
	askRepairAgentUnavailable: "the agent that asked is archived or missing, so no repair run could start",
	askRepairRefused:          "a repair run for the agent was refused by the run gates",
	askRepairCreatedAsk:       "the repair run produced another ask that did not reach a person",
	askRepairNoRequest:        "the repair run ended without the decision request",
}

// repairAsks gives each event that cannot be sent as a notice one repair run
// for its agent, then closes every root whose repair run has ended.
func (handler *Handler) repairAsks(ctx context.Context) error {
	return errors.Join(handler.admitAskRepairs(ctx), handler.settleAskRepairs(ctx))
}

// admitAskRepairs admits at most one repair run per recovery root. One failing
// event does not hold back the others.
func (handler *Handler) admitAskRepairs(ctx context.Context) error {
	events, err := handler.Queries.ListAskRepairEvents(ctx, askRepairBatch)
	if err != nil {
		return err
	}
	var failures []error
	for _, event := range events {
		if err := handler.admitAskRepair(ctx, event); err != nil {
			slog.Warn("ask repair admission failed", "event_id", uuidToString(event.ID), "error", err)
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// admitAskRepair writes the root row and enqueues the repair run in one
// transaction, so a restart finds both or neither. A refusal rolls back only
// the enqueue and records the failed root.
func (handler *Handler) admitAskRepair(ctx context.Context, listed db.AskGuardEvent) error {
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	event, err := queries.LockAskGuardEvent(ctx, db.LockAskGuardEventParams{ID: listed.ID, WorkspaceID: listed.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if event.ClearedAt.Valid || event.RepairHandledAt.Valid || !event.RepairReason.Valid {
		return nil
	}
	handled := func() error {
		if err := queries.SetAskGuardEventRepairHandled(ctx, db.SetAskGuardEventRepairHandledParams{ID: event.ID, WorkspaceID: event.WorkspaceID}); err != nil {
			return err
		}
		return transaction.Commit(ctx)
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: event.WorkspaceID, ID: event.IssueID})
	if errors.Is(err, pgx.ErrNoRows) {
		return handled()
	}
	if err != nil {
		return err
	}
	task, err := queries.GetAgentTask(ctx, event.TaskID)
	if err != nil {
		return err
	}
	rootID := event.ID
	if event.RootID.Valid {
		rootID = event.RootID
	}
	root, err := queries.InsertAskRepairRoot(ctx, db.InsertAskRepairRootParams{ID: rootID, WorkspaceID: event.WorkspaceID, IssueID: event.IssueID, AgentID: task.AgentID})
	if errors.Is(err, pgx.ErrNoRows) {
		// The root has its repair run. An open ask beyond it is not repaired twice.
		if root, err = queries.LockAskRepairRoot(ctx, db.LockAskRepairRootParams{ID: rootID, WorkspaceID: event.WorkspaceID}); err != nil {
			return err
		}
		if err := handler.failAskRepairRoot(ctx, queries, root, askRepairCreatedAsk); err != nil {
			return err
		}
		return handled()
	}
	if err != nil {
		return err
	}
	agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: event.WorkspaceID, ID: task.AgentID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) || agent.ArchivedAt.Valid {
		if err := handler.failAskRepairRoot(ctx, queries, root, askRepairAgentUnavailable); err != nil {
			return err
		}
		return handled()
	}
	// A savepoint keeps the root row when the normal enqueue gates (project
	// stop, run budget, role and approval) refuse the run.
	admission, err := transaction.Begin(ctx)
	if err != nil {
		return err
	}
	defer admission.Rollback(ctx)
	repair, enqueueErr := handler.TaskService.EnqueueAskRepairTaskInTx(ctx, queries.WithTx(admission), issue, agent.ID, askRepairMessage(event, issue))
	if enqueueErr != nil {
		if err := admission.Rollback(ctx); err != nil {
			return errors.Join(enqueueErr, err)
		}
		slog.Warn("ask repair run refused", "event_id", uuidToString(event.ID), "error", enqueueErr)
		if err := handler.failAskRepairRoot(ctx, queries, root, askRepairRefused); err != nil {
			return err
		}
		return handled()
	}
	if err := queries.SetAskRepairRootTask(ctx, db.SetAskRepairRootTaskParams{ID: root.ID, WorkspaceID: root.WorkspaceID, RepairTaskID: repair.ID}); err != nil {
		return err
	}
	if err := admission.Commit(ctx); err != nil {
		return err
	}
	if err := queries.SetAskGuardEventRepairHandled(ctx, db.SetAskGuardEventRepairHandledParams{ID: event.ID, WorkspaceID: event.WorkspaceID}); err != nil {
		return err
	}
	if err := askRepairFaultAt("before_commit"); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return err
	}
	handler.TaskService.PublishTaskEnqueued(ctx, repair)
	return nil
}

// askRepairMessage is the repair run's message: the exact request command and
// why the ask did not reach the person. It carries no source text, so a run for
// an event whose recipient lost access learns nothing the recipient could not read.
func askRepairMessage(event db.AskGuardEvent, issue db.Issue) string {
	person := "<person-user-id>"
	if event.MemberID.Valid {
		person = uuidToString(event.MemberID)
	}
	return fmt.Sprintf("Ask recovery: an ask from your earlier run on issue %s did not reach a person (reason: %s). "+
		"Ask the person now with one decision request, one question in a file: "+
		"`multica decision request --issue %s --for %s --class <reversible|spend|deploy|publish|irreversible|product_direction> --recovers %s --ask-file <ask.json>`. "+
		"If you make no matching request, the ask counts as failed and the failure stays with you. "+
		"This message grants no decision, merge or deploy authority.",
		uuidToString(issue.ID), event.RepairReason.String, uuidToString(issue.ID), person, uuidToString(event.ID))
}

// settleAskRepairs closes each root whose repair run has ended: settled when
// the event was answered by a request and nothing new is open, failed otherwise.
func (handler *Handler) settleAskRepairs(ctx context.Context) error {
	roots, err := handler.Queries.ListEndedAskRepairRoots(ctx, askRepairBatch)
	if err != nil {
		return err
	}
	var failures []error
	for _, root := range roots {
		if err := handler.settleAskRepair(ctx, root); err != nil {
			slog.Warn("ask repair settle failed", "root_id", uuidToString(root.ID), "error", err)
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (handler *Handler) settleAskRepair(ctx context.Context, listed db.AskRepairRoot) error {
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	root, err := queries.LockAskRepairRoot(ctx, db.LockAskRepairRootParams{ID: listed.ID, WorkspaceID: listed.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if root.State != "admitted" {
		return nil
	}
	reason := ""
	original, err := queries.GetAskGuardEventInWorkspace(ctx, db.GetAskGuardEventInWorkspaceParams{ID: root.ID, WorkspaceID: root.WorkspaceID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	open, err := queries.CountOpenAskRepairRootEvents(ctx, db.CountOpenAskRepairRootEventsParams{RootID: root.ID, WorkspaceID: root.WorkspaceID})
	if err != nil {
		return err
	}
	switch {
	case open > 0:
		reason = askRepairCreatedAsk
	case errors.Is(err, pgx.ErrNoRows) || !original.ClearedAt.Valid:
		reason = askRepairNoRequest
	}
	if reason == "" {
		if err := queries.SettleAskRepairRoot(ctx, db.SettleAskRepairRootParams{ID: root.ID, WorkspaceID: root.WorkspaceID}); err != nil {
			return err
		}
	} else if err := handler.failAskRepairRoot(ctx, queries, root, reason); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

// failAskRepairRoot sets an open root to failed and writes the one system
// comment, in the caller's transaction. The comment names the owning agent in
// plain text and mentions nobody.
func (handler *Handler) failAskRepairRoot(ctx context.Context, queries *db.Queries, root db.AskRepairRoot, reason string) error {
	commentID := dbid.NewV7()
	failed, err := queries.FailAskRepairRoot(ctx, db.FailAskRepairRootParams{ID: root.ID, WorkspaceID: root.WorkspaceID, FailureReason: pgtype.Text{String: reason, Valid: true}, FailureCommentID: commentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	name := uuidToString(failed.AgentID)
	if agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: failed.WorkspaceID, ID: failed.AgentID}); err == nil {
		name = agent.Name
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	content := fmt.Sprintf("Ask recovery failed for event %s: %s. Owner: agent %s. The failure is listed in the decision brief.", uuidToString(failed.ID), askRepairFailureText[reason], plainName(name))
	_, err = queries.CreateComment(ctx, db.CreateCommentParams{ID: commentID, IssueID: failed.IssueID, WorkspaceID: failed.WorkspaceID, AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Content: content, Type: "system"})
	return err
}

// plainName drops the characters that could turn a name into a mention link.
func plainName(name string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune("[]()<>@`", r) {
			return -1
		}
		return r
	}, name)
}
