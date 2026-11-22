package handler

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/multica-ai/multica/server/internal/decisiontx"
)

// The typed settings fence (CHE-7361) orders a typed send against a write that
// changes a key the send's check reads. A send holds the fence shared as its
// first lock; the passkey approval of workspace.settings.patch only tries it
// and the owner's direct write waits for it, each before any row lock.
// Sends and capped moves share it, so they never wait for each other.

// typedFencedKeys are the settings keys a typed send's admission reads.
var typedFencedKeys = []string{decisiontx.SettingServerActions, decisiontx.SettingStandingAnswers}

// errTypedFenceNeeded asks saveWorkspace to run again with the fence held: the
// stored settings changed between its first read and its row lock.
var errTypedFenceNeeded = errors.New("typed settings fence needed")

// typedFenceKeysChange reports whether writing incoming over stored changes a
// fenced key. replace is true for a write that replaces the whole object, so
// a key it omits is dropped; false for a patch, where an omitted key stays.
// A JSON null reads as an absent key.
func typedFenceKeysChange(stored []byte, incoming map[string]any, replace bool) bool {
	var before map[string]any
	_ = json.Unmarshal(stored, &before) // stored settings that are not an object hold no fenced key
	for _, key := range typedFencedKeys {
		next, named := incoming[key]
		if !named && !replace {
			continue
		}
		if !reflect.DeepEqual(before[key], next) {
			return true
		}
	}
	return false
}

// typedFenceKeysChangeRaw is typedFenceKeysChange for a patch whose values are raw JSON.
func typedFenceKeysChangeRaw(stored []byte, patch map[string]json.RawMessage) bool {
	incoming := make(map[string]any, len(typedFencedKeys))
	for _, key := range typedFencedKeys {
		raw, named := patch[key]
		if !named {
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return true // an invalid value is refused later; take the fence meanwhile
		}
		incoming[key] = value
	}
	return typedFenceKeysChange(stored, incoming, false)
}
