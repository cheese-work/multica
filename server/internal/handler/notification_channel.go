package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Channel switches live in their own table so the legacy replace-all
// notification-preferences PUT cannot erase them. A missing row means the
// default: inbox in_app on; every other switch off. DM switches need an
// explicit per-user opt-in (CHE-1848), as does push.
var (
	notifChannelSources  = []string{"inbox", "dm"}
	notifChannelChannels = []string{"in_app", "push"}
)

func notifChannelDefault(source, channel string) bool {
	return source == "inbox" && channel == "in_app"
}

func (h *Handler) writeNotificationChannels(w http.ResponseWriter, r *http.Request, workspaceID, userID string) {
	rows, err := h.Queries.ListNotificationChannelPreferences(r.Context(), db.ListNotificationChannelPreferencesParams{
		WorkspaceID: parseUUID(workspaceID),
		UserID:      parseUUID(userID),
	})
	if err != nil {
		slog.Warn("ListNotificationChannelPreferences failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to get notification channels")
		return
	}

	channels := map[string]map[string]bool{}
	for _, source := range notifChannelSources {
		channels[source] = map[string]bool{}
		for _, channel := range notifChannelChannels {
			channels[source][channel] = notifChannelDefault(source, channel)
		}
	}
	for _, row := range rows {
		if sw, ok := channels[row.Source]; ok && contains(notifChannelChannels, row.Channel) {
			sw[row.Channel] = row.Enabled
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace_id": workspaceID,
		"channels":     channels,
	})
}

func (h *Handler) GetNotificationChannels(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	h.writeNotificationChannels(w, r, ctxWorkspaceID(r.Context()), userID)
}

type patchNotifChannelRequest struct {
	Source  string `json:"source"`
	Channel string `json:"channel"`
	Enabled *bool  `json:"enabled"`
}

// PatchNotificationChannel sets exactly one (source, channel) switch.
func (h *Handler) PatchNotificationChannel(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())

	var req patchNotifChannelRequest
	dec := json.NewDecoder(r.Body)
	// A second Decode must hit EOF: trailing JSON or garbage is a malformed body.
	if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !contains(notifChannelSources, req.Source) {
		writeError(w, http.StatusBadRequest, "invalid source: "+req.Source)
		return
	}
	if !contains(notifChannelChannels, req.Channel) {
		writeError(w, http.StatusBadRequest, "invalid channel: "+req.Channel)
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled field is required")
		return
	}

	if err := h.Queries.SetNotificationChannelPreference(r.Context(), db.SetNotificationChannelPreferenceParams{
		WorkspaceID: parseUUID(workspaceID),
		UserID:      parseUUID(userID),
		Source:      req.Source,
		Channel:     req.Channel,
		Enabled:     *req.Enabled,
	}); err != nil {
		slog.Warn("SetNotificationChannelPreference failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to update notification channels")
		return
	}
	h.writeNotificationChannels(w, r, workspaceID, userID)
}
