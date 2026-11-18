package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/multica-ai/multica/server/internal/decisiontx"
)

// checkHeldSettingsAsk runs the CHE-5951 check on a decision or action card with
// the workspace settings. It writes the response and returns false when the card
// is refused (400 with the one-line AskError) or the stored setting is unreadable.
func checkHeldSettingsAsk(writer http.ResponseWriter, ask decisiontx.Ask, settings []byte) (hints []string, ok bool) {
	mode, err := decisiontx.HeldModeFromSettings(settings)
	if err != nil {
		slog.Error("workspace settings unreadable for decision card check", "error", err)
		writeError(writer, http.StatusInternalServerError, "workspace settings are unreadable")
		return nil, false
	}
	hints, err = ask.CheckHeldSettings(mode, settings)
	var askErr *decisiontx.AskError
	if errors.As(err, &askErr) {
		writeError(writer, http.StatusBadRequest, err.Error())
		return nil, false
	}
	if err != nil {
		slog.Error("workspace settings unreadable for decision card check", "error", err)
		writeError(writer, http.StatusInternalServerError, "workspace settings are unreadable")
		return nil, false
	}
	return hints, true
}

// validateHeldSettingModeSetting checks held_setting_ask_mode of a workspace
// settings object with the reader the ask check uses. An absent key is valid;
// null is not.
func validateHeldSettingModeSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	v, present := config[decisiontx.SettingHeldMode]
	if !present {
		return nil
	}
	raw, err := json.Marshal(map[string]any{decisiontx.SettingHeldMode: v})
	if err != nil {
		return err
	}
	_, err = decisiontx.HeldModeFromSettings(raw)
	return err
}
