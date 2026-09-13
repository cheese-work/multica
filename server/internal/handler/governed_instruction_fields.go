package handler

import (
	"encoding/json"
	"net/http"
)

// rawFieldsHasKey reports whether fieldName was present as a JSON key in the
// client's request body, regardless of its value (including explicit null
// or empty string). rawFields is the map produced alongside the decoded
// struct by decodeJSONBodyWithRawFields or an equivalent manual
// io.ReadAll + json.Unmarshal(&rawFields) pair.
func rawFieldsHasKey(rawFields map[string]json.RawMessage, fieldName string) bool {
	_, ok := rawFields[fieldName]
	return ok
}

// rejectGovernedFieldForAgentActor enforces CHE-455: an agent actor —
// authenticated via an `mat_` task-scoped token, or via the in-process
// X-Agent-ID/X-Task-ID fallback resolveActor also trusts — may never
// persist a change to a "governed instruction field" (an agent's, squad's,
// or workspace's `instructions`/`context`/`description` prompt content, or
// a project's `description`), even when the acting agent's owning human
// holds an admin/owner workspace role.
//
// This is deliberately field-scoped, not route-scoped: the same endpoints
// carry other fields (status, max_concurrent_tasks, name, avatar_url, ...)
// that agent actors legitimately write today, so the whole route cannot be
// gated the way GetAgentEnv/UpdateAgentEnv (MUL-2600) or RequireHumanActor
// gate theirs. Only the governed field itself is rejected — with a 403, so
// a client that retries with the field omitted keeps working, rather than
// a 400 that would suggest the request was malformed.
//
// Callers must have already decoded the body into a `map[string]json.RawMessage`
// of exactly the keys the client sent (see decodeJSONBodyWithRawFields /
// the manual io.ReadAll + json.Unmarshal pattern UpdateProject and
// UpdateWorkspace use) — a present-but-empty-string value must still be
// caught, which a nil-pointer check on the decoded struct cannot do.
//
// actorType must come from h.resolveActor(r, userID, workspaceID); passing
// anything else defeats the guard.
func rejectGovernedFieldForAgentActor(w http.ResponseWriter, actorType string, rawFieldPresent bool, fieldName string) bool {
	if actorType != "agent" || !rawFieldPresent {
		return false
	}
	writeError(w, http.StatusForbidden, "agents may not modify "+fieldName+"; this field requires human or applier authorization")
	return true
}
