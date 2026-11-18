package handler

import (
	"fmt"
	"net/http"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// refuseTypedOptionsOn answers 400 with the field path of the first typed
// option (server_action or after_wait) when the card is of a kind that cannot
// carry one: a topic ask, --action-file, a batch card or a CI card (CHE-7358).
// It reports whether it refused.
func refuseTypedOptionsOn(writer http.ResponseWriter, ask decisiontx.Ask, why string) bool {
	paths := ask.TypedOptionPaths()
	if len(paths) == 0 {
		return false
	}
	writeErrorCode(writer, http.StatusBadRequest, "typed_option_refused", paths[0]+" is not allowed on "+why)
	return true
}

// checkTypedOptions gates the typed options of a decision request on the
// workspace setting decision_server_actions (a plain read) and writes the
// effect line of each server_action option into what_happens. It writes the
// response and returns false when the card is refused.
func (handler *Handler) checkTypedOptions(writer http.ResponseWriter, request *http.Request, ask *decisiontx.Ask, workspace db.Workspace, issue db.Issue, why string) bool {
	paths := ask.TypedOptionPaths()
	if len(paths) == 0 {
		return true
	}
	if why != "" {
		return !refuseTypedOptionsOn(writer, *ask, why)
	}
	if !decisiontx.ServerActionsOn(workspace.Settings) {
		writeErrorCode(writer, http.StatusBadRequest, decisiontx.ServerActionsOffCode, paths[0]+" needs the workspace setting decision_server_actions to be on; it is off")
		return false
	}
	identifier := fmt.Sprintf("%s-%d", workspace.IssuePrefix, issue.Number)
	for index := range ask.Options {
		option := &ask.Options[index]
		name := ""
		if option.Action != nil && option.Action.Type == decisiontx.ActionAssign && option.Action.Params.AssigneeType == "agent" {
			// A name only reads better; the legality of the target is T4's check.
			if id, err := util.ParseUUID(option.Action.Params.AssigneeID); err == nil {
				if agent, err := handler.Queries.GetAgentInWorkspace(request.Context(), db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: workspace.ID}); err == nil {
					name = agent.Name
				}
			}
		}
		option.WriteEffectLine(identifier, name)
	}
	return true
}
