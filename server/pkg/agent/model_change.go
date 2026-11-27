package agent

import (
	"encoding/json"
	"strings"
)

// MessageModelChange marks one runtime-reported change of the model serving a
// run, with the evidence the runtime gave (CHE-10140). Its Input is built by
// newModelChangeMessage. The daemon stores it as a run message; the server
// projects it to a DM notice. It never changes the configured agent model.
const MessageModelChange MessageType = "model_change"

// Model change phases. Only "confirmed" says a model actually served; the
// others report what the runtime said it was doing.
const (
	ModelChangePhaseAttempt   = "attempt"   // runtime is retrying on the target
	ModelChangePhaseSticky    = "sticky"    // runtime keeps the target for later turns
	ModelChangePhaseReturn    = "return"    // runtime went back to the primary
	ModelChangePhaseConfirmed = "confirmed" // a response from the target was seen
	ModelChangePhaseFailed    = "failed"    // the fallback did not serve
)

// modelChangeFields is the runtime-neutral shape of one transition. Empty
// strings are omitted from Input rather than recorded as unknown values.
type modelChangeFields struct {
	runtime, event, phase    string
	sourceModel, targetModel string
	reason, category         string
	direction, scope         string
	nativeEventID, requestID string
}

func newModelChangeMessage(f modelChangeFields) Message {
	in := map[string]any{}
	for k, v := range map[string]string{
		"runtime": f.runtime, "event": f.event, "phase": f.phase,
		"source_model": f.sourceModel, "target_model": f.targetModel,
		"reason": f.reason, "category": f.category,
		"direction": f.direction, "scope": f.scope,
		"native_event_id": f.nativeEventID, "request_id": f.requestID,
	} {
		if v = strings.TrimSpace(v); v != "" {
			in[k] = v
		}
	}
	return Message{Type: MessageModelChange, Input: in}
}

// claudeFallbackWatch tracks a main-thread fallback the runtime announced but
// no response has confirmed yet.
type claudeFallbackWatch struct {
	eventID, source, target, event string
}

// claudeModelChange maps one system frame to its model_change message and the
// watch state after it. ok is false for other frames and for frames that name
// no model pair, which carry no usable evidence.
func claudeModelChange(msg claudeSDKMessage, watch *claudeFallbackWatch) (Message, *claudeFallbackWatch, bool) {
	f := modelChangeFields{
		runtime: "claude", event: msg.Subtype, scope: "session",
		sourceModel: msg.OriginalModel, targetModel: msg.FallbackModel,
		reason: msg.Trigger, nativeEventID: msg.UUID,
		category: msg.APIRefusalCategory, requestID: msg.RequestID,
		direction: msg.Direction,
	}
	switch msg.Subtype {
	case "model_refusal_fallback":
		if msg.Scope == "local" {
			f.scope = "local"
		}
		switch msg.Direction {
		case "revert":
			f.phase = ModelChangePhaseReturn
		case "sticky":
			f.phase = ModelChangePhaseSticky
		default:
			f.phase = ModelChangePhaseAttempt
		}
	case "model_fallback":
		f.phase = ModelChangePhaseAttempt
	case "model_refusal_no_fallback":
		f.phase, f.targetModel = ModelChangePhaseFailed, ""
		if f.sourceModel == "" {
			return Message{}, watch, false
		}
		return newModelChangeMessage(f), watch, true
	default:
		return Message{}, watch, false
	}
	if f.sourceModel == "" || f.targetModel == "" {
		return Message{}, watch, false
	}
	switch {
	case f.scope != "session":
		// A subagent or side request fell back; the main thread did not.
	case f.phase == ModelChangePhaseReturn:
		watch = nil
	default:
		watch = &claudeFallbackWatch{eventID: msg.UUID, source: f.sourceModel, target: f.targetModel, event: msg.Subtype}
	}
	return newModelChangeMessage(f), watch, true
}

// claudeFallbackConfirmed reports the confirmation once a main-thread
// assistant frame came from the fallback target.
func claudeFallbackConfirmed(watch *claudeFallbackWatch, msg claudeSDKMessage) (Message, bool) {
	if watch == nil || msg.ParentToolUseID != "" || claudeAssistantModel(msg) != watch.target {
		return Message{}, false
	}
	return newModelChangeMessage(modelChangeFields{
		runtime: "claude", event: watch.event, phase: ModelChangePhaseConfirmed, scope: "session",
		sourceModel: watch.source, targetModel: watch.target,
		nativeEventID: watch.eventID + ":confirmed",
	}), true
}

// claudeFallbackFailed reports a fallback whose turn ended in error before any
// response from the target was seen.
func claudeFallbackFailed(watch *claudeFallbackWatch) (Message, bool) {
	if watch == nil {
		return Message{}, false
	}
	return newModelChangeMessage(modelChangeFields{
		runtime: "claude", event: watch.event, phase: ModelChangePhaseFailed, scope: "session",
		sourceModel: watch.source, targetModel: watch.target,
		nativeEventID: watch.eventID + ":failed",
	}), true
}

// claudeAssistantModel is the model an assistant frame says produced it, or ""
// when the frame is unreadable.
func claudeAssistantModel(msg claudeSDKMessage) string {
	var content claudeMessageContent
	if err := json.Unmarshal(msg.Message, &content); err != nil {
		return ""
	}
	return content.Model
}
