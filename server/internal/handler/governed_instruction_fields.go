package handler

import (
	"encoding/json"
	"net/http"
	"strings"
)

// isGovernedFieldMachineActor reports whether r was authenticated by a
// machine credential that CHE-455 must deny on governed instruction
// fields: an `mat_` agent task token (resolveActor's "agent"
// classification) OR an `mcn_` cloud-node PAT (X-Actor-Source: cloud_pat).
//
// resolveActor alone is not enough here. It deliberately does not
// classify cloud_pat as "agent" — see actor_guards.go's RequireHumanActor
// doc comment: resolveActor's job is workspace-scoped authorship
// attribution (issue creator, comment author), and cloud nodes don't
// author workspace-scoped resources, so folding cloud_pat into that
// classifier would be the wrong coupling and could change authorship
// attribution behavior far outside this guard's scope. But a cloud-node
// PAT is, for CHE-455's purposes, exactly the same threat as an mat_
// token: a machine credential acting as its owning human with no human
// having reviewed this specific write. isMachineCredentialActor
// (actor_guards.go) is the existing, tested, authoritative check for
// that — X-Actor-Source is server-set only, stripped of any
// client-supplied value before the auth middleware re-stamps it, so
// checking it here cannot be spoofed.
func isGovernedFieldMachineActor(r *http.Request, actorType string) bool {
	return actorType == "agent" || isMachineCredentialActor(r)
}

// rawFieldsHasKeyFold reports whether fieldName was present as a JSON
// object key in the client's request body, matched case-insensitively —
// mirroring how encoding/json itself matches object keys to struct fields
// when decoding into a Go struct (RFC 8259 doesn't require case-sensitive
// object keys, and encoding/json deliberately doesn't enforce it). Use
// this, never a plain `rawFields[fieldName]` lookup, anywhere the decoded
// struct alone cannot distinguish "field omitted" from "field explicitly
// set to null" (see UpdateProject's Description/Icon/LeadType/... pattern).
// A plain map lookup is case-sensitive and is exactly what let
// `{"Description": "..."}` (or any other case variant) bypass CHE-455's
// guard while still reaching the write path through the case-insensitively
// decoded struct field — see the CHE-455 review finding on PR #29.
func rawFieldsHasKeyFold(rawFields map[string]json.RawMessage, fieldName string) bool {
	for k := range rawFields {
		if strings.EqualFold(k, fieldName) {
			return true
		}
	}
	return false
}

// rejectGovernedFieldForAgentActor enforces CHE-455: a machine actor —
// an `mat_` agent task token (including the in-process X-Agent-ID/X-Task-ID
// fallback resolveActor also trusts) or an `mcn_` cloud-node PAT — may
// never persist a change to a "governed instruction field" (an agent's,
// squad's, or workspace's `instructions`/`context`/`description` prompt
// content, or a project's `description`), even when the credential's
// owning human holds an admin/owner workspace role.
//
// This is deliberately field-scoped, not route-scoped: the same endpoints
// carry other fields (status, max_concurrent_tasks, name, avatar_url, ...)
// that machine actors legitimately write today, so the whole route cannot
// be gated the way GetAgentEnv/UpdateAgentEnv (MUL-2600) or
// RequireHumanActor gate theirs. Only the governed field itself is
// rejected — with a 403, so a client that retries with the field omitted
// keeps working, rather than a 400 that would suggest the request was
// malformed.
//
// fieldWillBeWritten must be computed from the DECODED STRUCT FIELD that the
// handler is about to copy into its db.*Params — e.g. `req.Instructions !=
// nil` for an `*string` update field, or `req.Instructions != ""` for
// CreateAgent's plain `string` create field — never from a raw
// map[string]json.RawMessage key lookup. encoding/json matches object keys
// to struct fields case-insensitively (`{"Instructions":...}` populates
// `req.Instructions` exactly like `{"instructions":...}`), while a raw map
// key lookup is case-sensitive. A body of `{"Instructions":"..."}` used to
// slip through a `rawFields["instructions"]` presence check, get decoded
// into the struct anyway by the JSON decoder, and get written to the DB by
// the unconditional `if req.Instructions != nil { params.Instructions = ... }`
// below it — passing every negative test that only sent the canonical
// lowercase key. Gating on the struct field closes that: it is the exact
// value the write path branches on, so it cannot drift from what actually
// reaches the DB.
//
// actorType must come from h.resolveActor(r, userID, workspaceID); passing
// anything else weakens the mat_/in-process half of the guard. r supplies
// the cloud_pat half via isGovernedFieldMachineActor — pass the same *http.Request
// resolveActor was called with.
func rejectGovernedFieldForAgentActor(w http.ResponseWriter, r *http.Request, actorType string, fieldWillBeWritten bool, fieldName string) bool {
	if !isGovernedFieldMachineActor(r, actorType) || !fieldWillBeWritten {
		return false
	}
	writeError(w, http.StatusForbidden, "agents may not modify "+fieldName+"; this field requires human or applier authorization")
	return true
}
