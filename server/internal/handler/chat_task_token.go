package handler

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

// chatWakeupTaskID returns the chat task the request runs as. It is an
// agent-facing surface: the task token names the DM, so no session id is
// passed. It lives here, not in chat_wakeup.go, because PostChatOutcome (fork)
// needs it and chat_wakeup.go is upstream's file whole (CHE-7278 rule 9).
func chatWakeupTaskID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeError(w, http.StatusForbidden, "only a running chat task can manage DM wakeups")
		return pgtype.UUID{}, false
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		writeError(w, http.StatusForbidden, "a running chat task is required")
		return pgtype.UUID{}, false
	}
	return taskID, true
}
