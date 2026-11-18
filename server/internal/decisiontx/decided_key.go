package decisiontx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

// Decided-ask keys (CHE-5682 layer 1). A key is the SHA-256 of the RFC 8785
// form of one JSON object built from typed fields only: never text similarity.
// Every function here is pure. A key error means "cannot compare"; the caller
// treats it as no match and never refuses an ask on it.

// KeyVersion prefixes every key document. A change to the key rules bumps it,
// so keys of two versions never compare equal.
const KeyVersion = "v1"

// KeyExcludedAskFields names, by JSON field, the only Ask fields left out of
// the match key: the asker's explanation, checks and provenance. Another agent
// rewrites them on every re-ask and none changes what a choice does. A field
// added to Ask later is in the key by default, so the key gets stricter, never
// weaker. state_claims (CHE-5950) joined after the CHE-5682 plan: each claim
// carries a fresh observed_at, so it is the asker's evidence like probes.
var KeyExcludedAskFields = []string{
	"context_eli10", "agent_did", "stakes_if_wrong", "recommendation", "recommended_option", "confidence", "probes", "state_claims",
	"source_issue_id", "source_comment_id",
}

// keyExactStrings lists the string fields kept as exact bytes. Every other
// string is trimmed and its whitespace runs collapsed. Spacing inside a command
// can change what it does.
var keyExactStrings = map[string]bool{"AskOption.agent_commands": true, "HumanStep.command": true}

// ActionKeyInput is the action of a decision request or proposal: the new
// draft, or the stored decision_action row. Params is the raw JSON object.
// preconditions and expires_at are not inputs: the first is the server's
// snapshot of the target at ask time, the second a card lifetime.
type ActionKeyInput struct {
	Type        string
	Version     int
	Params      json.RawMessage
	SecretSlots []decisionaction.SecretSlot
}

// SubjectKey groups asks of one subject for supersession: kind, decision,
// subject and topic. Use ProposalKey for a decision proposal.
func SubjectKey(kind string, ask Ask) (string, error) {
	return keyOfDoc(map[string]any{
		"key_version": KeyVersion, "key": "subject", "kind": collapse(kind),
		"decision": collapse(ask.Decision), "subject": collapse(ask.Subject), "topic": collapse(ask.Topic),
	})
}

// MatchKey is the key two asks share only when they ask the same thing: key
// version, kind, escalation class, the ask content minus KeyExcludedAskFields,
// and the action (nil is a different key from any action).
func MatchKey(kind, escalationClass string, ask Ask, action *ActionKeyInput) (string, error) {
	doc, err := matchKeyValue(kind, escalationClass, ask, action)
	if err != nil {
		return "", err
	}
	return keyOfDoc(doc)
}

// ProposalKey is the subject key and the match key of a decision proposal: the
// action key alone.
func ProposalKey(action ActionKeyInput) (string, error) {
	value, err := actionKeyValue(&action)
	if err != nil {
		return "", err
	}
	return keyOfDoc(map[string]any{"key_version": KeyVersion, "key": "proposal", "action": value})
}

// matchKeyDoc is the canonical JSON the match key hashes.
func matchKeyDoc(kind, escalationClass string, ask Ask, action *ActionKeyInput) ([]byte, error) {
	doc, err := matchKeyValue(kind, escalationClass, ask, action)
	if err != nil {
		return nil, err
	}
	return canonicalDoc(doc)
}

func matchKeyValue(kind, escalationClass string, ask Ask, action *ActionKeyInput) (map[string]any, error) {
	content, err := keyValue(reflect.ValueOf(ask), false)
	if err != nil {
		return nil, err
	}
	actionValue, err := actionKeyValue(action)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"key_version": KeyVersion, "key": "match", "kind": collapse(kind), "escalation_class": collapse(escalationClass),
		"ask": content, "action": actionValue,
	}, nil
}

// actionKeyValue is the action key object, or nil for no action. params_sha256
// is the hex SHA-256 of the RFC 8785 form of params; secret slots carry names
// and targets only, never a value, and are sorted.
func actionKeyValue(action *ActionKeyInput) (any, error) {
	if action == nil {
		return nil, nil
	}
	params, err := decisionaction.Canonical(action.Params)
	if err != nil {
		return nil, fmt.Errorf("action params: %w", err)
	}
	paramsHash := sha256.Sum256(params)
	slots := make([]json.RawMessage, 0, len(action.SecretSlots))
	for _, slot := range action.SecretSlots {
		canonical, err := canonicalDoc(map[string]any{"slot": slot.Slot, "target": map[string]any{"agent_id": slot.Target.AgentID, "env_key": slot.Target.EnvKey}})
		if err != nil {
			return nil, err
		}
		slots = append(slots, canonical)
	}
	slices.SortFunc(slots, func(a, b json.RawMessage) int { return bytes.Compare(a, b) })
	return map[string]any{
		"action_type": action.Type, "action_version": action.Version, "params_sha256": hex.EncodeToString(paramsHash[:]), "secret_slots": slots,
	}, nil
}

// keyValue turns a value into plain JSON values by reflection, so every field
// of Ask, AskOption and the types inside them joins the key with no list to
// keep in step. Empty and nil lists are the same. options is order-free.
func keyValue(value reflect.Value, exact bool) (any, error) {
	switch value.Kind() {
	case reflect.String:
		if exact {
			return value.String(), nil
		}
		return collapse(value.String()), nil
	case reflect.Bool:
		return value.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int(), nil
	case reflect.Float32, reflect.Float64:
		return value.Float(), nil
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return nil, nil
		}
		return keyValue(value.Elem(), exact)
	case reflect.Slice, reflect.Array:
		items := make([]any, 0, value.Len())
		for i := range value.Len() {
			item, err := keyValue(value.Index(i), exact)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		return items, nil
	case reflect.Struct:
		return keyStruct(value, exact)
	}
	return nil, fmt.Errorf("key: unsupported kind %s", value.Kind())
}

func keyStruct(value reflect.Value, exact bool) (map[string]any, error) {
	structType := value.Type()
	out := map[string]any{}
	for i := range structType.NumField() {
		field := structType.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if !field.IsExported() || name == "-" || name == "" {
			continue
		}
		if structType == reflect.TypeFor[Ask]() && slices.Contains(KeyExcludedAskFields, name) {
			continue
		}
		item, err := keyValue(value.Field(i), exact || keyExactStrings[structType.Name()+"."+name])
		if err != nil {
			return nil, err
		}
		if structType == reflect.TypeFor[Ask]() && name == "options" {
			if item, err = sortedOptions(item); err != nil {
				return nil, err
			}
		}
		out[name] = item
	}
	return out, nil
}

// sortedOptions orders options by their canonical JSON: the server moves the
// recommended option first, so order is not identity.
func sortedOptions(options any) (any, error) {
	list, _ := options.([]any)
	encoded := make([]json.RawMessage, 0, len(list))
	for _, option := range list {
		canonical, err := canonicalDoc(option)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, canonical)
	}
	slices.SortFunc(encoded, func(a, b json.RawMessage) int { return bytes.Compare(a, b) })
	return encoded, nil
}

func collapse(text string) string { return strings.Join(strings.Fields(text), " ") }

func canonicalDoc(doc any) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	canonical, err := decisionaction.Canonical(raw)
	if err != nil {
		return nil, err
	}
	if len(canonical) == 0 {
		return nil, errors.New("key: empty document")
	}
	return canonical, nil
}

func keyOfDoc(doc any) (string, error) {
	canonical, err := canonicalDoc(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
