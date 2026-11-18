package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/designguard"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// errCannotDoOwnerOnly is saveWorkspace's refusal of an owner-only setting
// change by a caller who is not a workspace owner (CHE-5902 F2, CHE-7358).
var errCannotDoOwnerOnly = errors.New("only a workspace owner may change agent_cannot_do, decision_server_actions or standing_answers")

// ownerOnlySettings are the keys the route admits admins to save but only an owner may change.
var ownerOnlySettings = []string{decisiontx.SettingCannotDo, decisiontx.SettingServerActions, decisiontx.SettingStandingAnswers}

// requireOwnerForCannotDo refuses an incoming settings blob that adds, drops or
// changes an owner-only setting unless the caller owns the workspace. The
// route admits admins; these keys are the owner's.
func requireOwnerForCannotDo(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, userID string, stored, incoming map[string]any) error {
	moved := false
	for _, key := range ownerOnlySettings {
		sv, sok := stored[key]
		iv, iok := incoming[key]
		moved = moved || sok != iok || !reflect.DeepEqual(sv, iv)
	}
	if !moved {
		return nil
	}
	member, err := workspaceMemberVia(ctx, q, userID, uuidToString(workspaceID))
	if err != nil || member.Role != "owner" {
		return errCannotDoOwnerOnly
	}
	return nil
}

// checkAgentCannotDo runs the CHE-5902 check on a decision card with the
// workspace settings. It writes the response and returns false when the card
// is refused (400 with the one-line AskError) or the stored setting is unreadable.
func checkAgentCannotDo(writer http.ResponseWriter, ask decisiontx.Ask, settings []byte) (hints []string, ok bool) {
	mode, catalog, err := decisiontx.CannotDoFromSettings(settings)
	if err != nil {
		slog.Error("workspace settings unreadable for decision card check", "error", err)
		writeError(writer, http.StatusInternalServerError, "workspace settings are unreadable")
		return nil, false
	}
	hints, err = ask.CheckCannotDo(mode, catalog)
	var askErr *decisiontx.AskError
	if errors.As(err, &askErr) {
		writeError(writer, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return hints, true
}

// checkCardLint runs the CHE-5993 card lint with the workspace mode. It writes
// the response and returns false when the card is refused (400, one line with
// the reason code and field path) or the stored setting is unreadable.
func checkCardLint(writer http.ResponseWriter, ask decisiontx.Ask, class string, settings []byte) (hints []string, ok bool) {
	mode, err := decisiontx.CardLintModeFromSettings(settings)
	if err != nil {
		slog.Error("workspace settings unreadable for decision card check", "error", err)
		writeError(writer, http.StatusInternalServerError, "workspace settings are unreadable")
		return nil, false
	}
	hints, err = ask.CheckCard(mode, class)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return hints, true
}

// validateCardLintSetting checks decision_card_lint_mode of a workspace
// settings object with the reader the card lint uses. An absent key is valid; null is not.
func validateCardLintSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	mode, present := config[decisiontx.SettingCardLintMode]
	if !present {
		return nil
	}
	raw, err := json.Marshal(map[string]any{decisiontx.SettingCardLintMode: mode})
	if err != nil {
		return err
	}
	_, err = decisiontx.CardLintModeFromSettings(raw)
	return err
}

// validateDesignGuardSetting checks design_card_guard of a workspace settings
// object with the reader the design guard uses. An absent key is valid; null is not.
func validateDesignGuardSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	setting, present := config[designguard.SettingKey]
	if !present {
		return nil
	}
	raw, err := json.Marshal(map[string]any{designguard.SettingKey: setting})
	if err != nil {
		return err
	}
	_, err = designguard.ParseSettings(raw)
	return err
}

// validateAgentCannotDoSetting checks agent_cannot_do_mode and agent_cannot_do
// of a workspace settings object with the reader the decision check uses.
// Absent keys are valid; null is not.
func validateAgentCannotDoSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	only := map[string]any{}
	for _, key := range []string{decisiontx.SettingCannotDoMode, decisiontx.SettingCannotDo} {
		if v, present := config[key]; present {
			only[key] = v
		}
	}
	if len(only) == 0 {
		return nil
	}
	raw, err := json.Marshal(only)
	if err != nil {
		return err
	}
	_, _, err = decisiontx.CannotDoFromSettings(raw)
	return err
}

// validateServerActionSetting checks decision_server_actions and
// standing_answers of a workspace settings object. Absent keys are valid; null is not.
func validateServerActionSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return decisiontx.ValidateServerActionSettings(config)
}
