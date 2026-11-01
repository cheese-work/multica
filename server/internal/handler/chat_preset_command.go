package handler

import (
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/agentpreset"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// parsePresetCommand reports whether a DM message is the /preset command and
// returns its argument. Only a single-line message whose first word is exactly
// /preset is one, so `/presets are great` and a command with text after it
// stay ordinary messages.
func parsePresetCommand(content string) (arg string, ok bool) {
	content = strings.TrimSpace(content)
	if strings.Contains(content, "\n") {
		return "", false
	}
	words := strings.Fields(content)
	if len(words) == 0 || words[0] != "/preset" {
		return "", false
	}
	return strings.Join(words[1:], " "), true
}

// runPresetCommand answers /preset in the DM: no argument lists the choices,
// `reset` hands the choice back to the agent default, `base` forces the agent
// row, any other name selects that preset. The reply is a server-authored
// notice; the command itself is not stored, so it never reaches a prompt.
func (h *Handler) runPresetCommand(w http.ResponseWriter, r *http.Request, session db.ChatSession, agent db.Agent, workspaceID, userID, arg string) {
	ctx := r.Context()
	usable, err := h.usablePresetNames(ctx, agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list presets")
		return
	}
	choices := strings.Join(append([]string{agentpreset.BaseName, "reset"}, usable...), ", ")
	current := "none (the agent default applies)"
	if session.PresetName.Valid {
		current = session.PresetName.String
	}

	var reply string
	set := func(name *string) bool {
		stored := pgtype.Text{}
		if name != nil {
			stored = pgtype.Text{String: *name, Valid: true}
		}
		if _, err := h.Queries.SetChatSessionPresetName(ctx, db.SetChatSessionPresetNameParams{ID: session.ID, PresetName: stored}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to set the preset")
			return false
		}
		return true
	}
	switch {
	case arg == "":
		reply = "Preset for this DM: " + current + ". Choices: " + choices + "."
	case arg == "reset":
		if !set(nil) {
			return
		}
		reply = "Preset cleared. The agent default applies."
	case arg == agentpreset.BaseName || slices.Contains(usable, arg):
		if !set(&arg) {
			return
		}
		reply = "Preset set to " + arg + " for the next run."
	default:
		reply = "No usable preset named \"" + arg + "\". Choices: " + choices + "."
	}

	msg, err := h.Queries.CreateChatMessage(ctx, db.CreateChatMessageParams{
		ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant", Content: reply,
		MessageKind: pgtype.Text{String: protocol.ChatMessageKindCompactNotice, Valid: true},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write the reply")
		return
	}
	sessionID := util.UUIDToString(session.ID)
	h.publishChat(protocol.EventChatMessage, workspaceID, "member", userID, sessionID, protocol.ChatMessagePayload{
		ChatSessionID: sessionID, MessageID: util.UUIDToString(msg.ID), Role: "assistant", Content: reply,
		CreatedAt: timestampToString(msg.CreatedAt), MessageKind: protocol.ChatMessageKindCompactNotice,
	})
	writeJSON(w, http.StatusCreated, SendChatMessageResponse{MessageID: util.UUIDToString(msg.ID), CreatedAt: timestampToString(msg.CreatedAt)})
}
