package handler

import (
	"net/http"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func decisionLaneArchiveAllowed(writer http.ResponseWriter, request *http.Request, queries *db.Queries, session db.ChatSession) bool {
	present, err := queries.DecisionOpenLaneExists(request.Context(), db.DecisionOpenLaneExistsParams{WorkspaceID: session.WorkspaceID, SessionID: session.ID})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to check open decisions")
		return false
	}
	if present {
		writeError(writer, http.StatusConflict, "an open decision request requires this chat session")
		return false
	}
	return true
}
