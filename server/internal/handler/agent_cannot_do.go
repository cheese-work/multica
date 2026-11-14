package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/multica-ai/multica/server/internal/decisiontx"
)

// checkAgentCannotDo runs the CHE-5902 check on a decision card with the
// workspace settings. It writes the response and returns false when the card
// is refused (400 with the one-line AskError) or the stored setting is unreadable.
func checkAgentCannotDo(writer http.ResponseWriter, ask decisiontx.Ask, settings []byte) (hints []string, ok bool) {
	mode, catalog, err := decisiontx.CannotDoFromSettings(settings)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "workspace settings: "+err.Error())
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
