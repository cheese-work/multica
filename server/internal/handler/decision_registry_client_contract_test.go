package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

// previewExempt names the passkey-required action types that need no card
// preview, each with the reason. An empty map is the goal: a type listed here
// shows "Details unavailable" and its card cannot be approved (CHE-6242).
var previewExempt = map[string]string{}

// Every approvable action type must render its card: a config change (the
// client shows the stored diff) or a server preview that the web client also
// vouches for. A type with neither registers fine but its cards are never
// approvable (CHE-6247).
func TestEveryApprovableActionTypeRendersItsCard(t *testing.T) {
	preview, previewed := serverPreviewTypes(t), clientTypeList(t, "PREVIEWED_ACTION_TYPES")
	config := clientTypeList(t, "CONFIG_CHANGE_ACTION_TYPES")
	for _, definition := range decisionaction.Definitions() {
		if !definition.RequiresPasskey() {
			continue
		}
		typ := definition.Type
		if reason, ok := previewExempt[typ]; ok {
			if reason == "" {
				t.Errorf("%s: exemption needs a reason", typ)
			}
			continue
		}
		switch {
		case config[typ]:
		case preview[typ] && previewed[typ]:
		default:
			t.Errorf("%s: no card preview (config change in web client: %t, server preview: %t, web PREVIEWED_ACTION_TYPES: %t); its cards show \"Details unavailable\"", typ, config[typ], preview[typ], previewed[typ])
		}
	}
}
