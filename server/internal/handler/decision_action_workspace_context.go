package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// workspace.context.set v1 (CHE-4485) replaces the whole workspace context. The
// agent proposes; the owner approves. The server reads the live context, builds
// the text diff and compares the agent's digests with its own.

const (
	workspaceContextField = "workspace.context"
	// workspaceContextCap is the longest context, in code points, a change may leave.
	workspaceContextCap = 10000
)

func init() {
	registerConfigChange("workspace.context.set", configChange{
		Fields:    []string{workspaceContextField},
		Authorize: authorizeWorkspaceContextProposer,
		Lock: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) error {
			_, err := loadWorkspaceTarget(ctx, queries, workspaceID, params, http.StatusConflict, true)
			return err
		},
		Read:  readWorkspaceContext,
		Apply: applyWorkspaceContext,
		Publish: func(ctx context.Context, handler *Handler, action db.DecisionAction) error {
			workspace, err := handler.Queries.GetWorkspace(ctx, action.WorkspaceID)
			if err != nil {
				return err
			}
			handler.publish(protocol.EventWorkspaceUpdated, uuidToString(action.WorkspaceID), "member", uuidToString(action.ApprovedByUserID), map[string]any{"workspace": handler.workspaceToResponse(workspace)})
			return nil
		},
	})
}

func readWorkspaceContext(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) (configChangeRead, error) {
	set, err := textSetParams(params, "workspace_id", "context")
	if err != nil {
		return configChangeRead{}, err
	}
	workspace, err := loadWorkspaceTarget(ctx, queries, workspaceID, params, http.StatusBadRequest, false)
	if err != nil {
		return configChangeRead{}, err
	}
	live := workspace.Context.String
	if err := checkWorkspaceContext(live, set.Content); err != nil {
		return configChangeRead{}, err
	}
	read := textConfigRead(workspaceContextField, uuidToString(workspace.ID), live, set.Content)
	read.HumanBlocksIntact = protectedBlocksUnchanged(live, set.Content)
	read.Meta["cap"] = workspaceContextCap
	return read, nil
}

// checkWorkspaceContext refuses a NUL byte and text over the cap, except a
// change that leaves an already oversized value no longer (the rule shape of
// checkAgentInstructionsSize).
func checkWorkspaceContext(live, after string) error {
	if strings.ContainsRune(after, 0) {
		return decisionIntentRefusal{http.StatusBadRequest, "context_has_nul: the context may not contain a NUL byte"}
	}
	chars := utf8.RuneCountInString(after)
	if chars <= workspaceContextCap {
		return nil
	}
	if liveChars := utf8.RuneCountInString(live); liveChars > workspaceContextCap && chars <= liveChars {
		return nil
	}
	return decisionIntentRefusal{http.StatusUnprocessableEntity, fmt.Sprintf("context_too_long: the context may hold at most %d code points (got %d)", workspaceContextCap, chars)}
}

// authorizeWorkspaceContextProposer lets only an agent holding the workspace's
// delegated-write grant for the context propose a change. The grant is read
// again at approval: a proposer who lost it meanwhile is revoked, not unknown.
func authorizeWorkspaceContextProposer(ctx context.Context, queries *db.Queries, workspaceID, proposer pgtype.UUID, _ json.RawMessage, _ configChangeRead) error {
	if _, ok := delegatedWriteAllowedQ(ctx, queries, workspaceID, uuidToString(proposer), workspaceContextField); ok {
		return nil
	}
	if configChangeAtApproval(ctx) {
		return decisionIntentRefusal{http.StatusConflict, "proposer_revoked: the proposing agent no longer holds the delegated write grant for " + workspaceContextField}
	}
	return decisionIntentRefusal{http.StatusForbidden, "not_permitted: the proposing agent does not hold the delegated write grant for " + workspaceContextField}
}

func applyWorkspaceContext(ctx context.Context, execution decisionActionExecution, _ configChangeRead) error {
	set, err := textSetParams(execution.Action.Params, "workspace_id", "context")
	if err != nil {
		return err
	}
	_, err = execution.Queries.UpdateWorkspace(ctx, db.UpdateWorkspaceParams{ID: execution.Action.WorkspaceID, Context: pgtype.Text{String: set.Content, Valid: true}})
	return err
}
