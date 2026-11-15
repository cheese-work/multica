package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// A person edits the workspace guard policy on the Guard settings page with a
// passkey (CHE-5207). The edit is signed over the workspace, the digest of the
// policy it edits (base) and the digest of the policy it writes (after), so a
// stale page cannot overwrite a newer policy: the base no longer matches and
// the server answers 409 with the current version.

const guardPolicyAuditKind = "workspace.guard_policy.update"

func init() {
	auditKinds[guardPolicyAuditKind] = auditKindSpec{TargetType: "workspace", Allow: []string{"guard_policy_sha256"}}
}

type guardPolicyBody struct {
	Policy     json.RawMessage `json:"policy"`
	BaseSHA256 string          `json:"base_sha256"`
	Assertion  json.RawMessage `json:"assertion"`
}

// guardPolicyStale is the 409: the policy changed since the page read it.
type guardPolicyStale struct{ current guardPolicyVersion }

func (guardPolicyStale) Error() string {
	return "stale_base: the guard policy changed since you opened it"
}

// guardPolicyVersion is the stored policy and its digest. Policy is null when
// the workspace stores none.
type guardPolicyVersion struct {
	Policy json.RawMessage `json:"policy"`
	SHA256 string          `json:"sha256"`
}

func guardPolicyDigest(workspaceID pgtype.UUID, policy json.RawMessage) (string, error) {
	return decisionaction.SettingsDigest(uuidToString(workspaceID), map[string]json.RawMessage{"guard_policy": policy})
}

func storedGuardPolicy(workspace db.Workspace) (guardPolicyVersion, error) {
	stored, err := decodeStoredSettings(workspace.Settings)
	if err != nil {
		return guardPolicyVersion{}, err
	}
	policy := stored["guard_policy"]
	if len(policy) == 0 {
		policy = json.RawMessage("null")
	}
	digest, err := guardPolicyDigest(workspace.ID, policy)
	return guardPolicyVersion{Policy: policy, SHA256: digest}, err
}

func writeGuardPolicyStale(writer http.ResponseWriter, stale guardPolicyStale) {
	writeJSON(writer, http.StatusConflict, map[string]any{
		"error": stale.Error(), "code": "stale_base", "current_sha256": stale.current.SHA256, "policy": stale.current.Policy,
	})
}

// GetGuardPolicy reads the stored guard policy and its digest, the base an
// edit names. Any workspace member reads it.
func (handler *Handler) GetGuardPolicy(writer http.ResponseWriter, request *http.Request) {
	id := workspaceIDFromURL(request, "id")
	if _, ok := handler.requireWorkspaceMember(writer, request, id, "workspace not found"); !ok {
		return
	}
	workspaceID, ok := parseUUIDOrBadRequest(writer, id, "workspace id")
	if !ok {
		return
	}
	workspace, err := handler.Queries.GetWorkspace(request.Context(), workspaceID)
	if err != nil {
		writeError(writer, http.StatusNotFound, "workspace not found")
		return
	}
	version, err := storedGuardPolicy(workspace)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to read the guard policy")
		return
	}
	writeJSON(writer, http.StatusOK, version)
}

// readGuardPolicyEdit decodes the body and refuses a policy the daemon's guard
// would refuse to load, or one that is not an object (the page never clears it).
func readGuardPolicyEdit(writer http.ResponseWriter, request *http.Request, workspaceID pgtype.UUID) (guardPolicyBody, string, bool) {
	var body guardPolicyBody
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, enrollBodyLimit)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return body, "", false
	}
	var value map[string]any
	if json.Unmarshal(body.Policy, &value) != nil || value == nil {
		writeError(writer, http.StatusBadRequest, "invalid_policy: policy must be a JSON object")
		return body, "", false
	}
	if err := validateGuardPolicySetting(map[string]any{"guard_policy": value}); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_policy: "+err.Error())
		return body, "", false
	}
	after, err := guardPolicyDigest(workspaceID, body.Policy)
	if err != nil || len(body.BaseSHA256) != 64 {
		writeError(writer, http.StatusBadRequest, "invalid_policy: base_sha256 must be the digest the page read")
		return body, "", false
	}
	return body, after, true
}

// ChallengeGuardPolicy opens the owner's assertion challenge for one edit. A
// base that is no longer current is refused here, so no assertion is spent on it.
func (handler *Handler) ChallengeGuardPolicy(writer http.ResponseWriter, request *http.Request) {
	userID, workspaceID, ok := handler.gateCaller(writer, request) // owner, interactive session, passkeys configured
	if !ok {
		return
	}
	body, after, ok := readGuardPolicyEdit(writer, request, workspaceID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	workspace, err := handler.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		writeError(writer, http.StatusNotFound, "workspace not found")
		return
	}
	current, err := storedGuardPolicy(workspace)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to read the guard policy")
		return
	}
	if current.SHA256 != body.BaseSHA256 {
		writeGuardPolicyStale(writer, guardPolicyStale{current})
		return
	}
	issued, err := handler.Passkeys.IssueGuardPolicyChange(ctx, userID, workspaceID, body.BaseSHA256, after)
	if err != nil {
		writeRevokeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, challengeResponse{
		ChallengeID: uuidToString(issued.ChallengeID), Options: issued.Options, ExpiresAt: issued.ExpiresAt,
	})
}

// SetGuardPolicy writes the policy once the owner signed that exact edit.
func (handler *Handler) SetGuardPolicy(writer http.ResponseWriter, request *http.Request) {
	userID, workspaceID, ok := handler.gateCaller(writer, request)
	if !ok {
		return
	}
	body, after, ok := readGuardPolicyEdit(writer, request, workspaceID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	var workspace db.Workspace
	check := func(ctx context.Context, queries *db.Queries) error {
		var err error
		if workspace, err = queries.GetWorkspaceForUpdate(ctx, workspaceID); err != nil {
			return err
		}
		current, err := storedGuardPolicy(workspace)
		if err != nil {
			return err
		}
		if current.SHA256 != body.BaseSHA256 {
			return guardPolicyStale{current}
		}
		return nil
	}
	write := func(ctx context.Context, queries *db.Queries, _ pgtype.UUID) error {
		stored, err := decodeStoredSettings(workspace.Settings)
		if err != nil {
			return err
		}
		stored["guard_policy"] = body.Policy
		encoded, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		if workspace, err = queries.UpdateWorkspace(ctx, db.UpdateWorkspaceParams{ID: workspaceID, Settings: encoded}); err != nil {
			return err
		}
		if err := handler.writeAudit(ctx, queries, request, auditInput{
			Kind: guardPolicyAuditKind, WorkspaceID: workspaceID, TargetID: workspaceID,
			Before: map[string]any{"guard_policy_sha256": body.BaseSHA256}, After: map[string]any{"guard_policy_sha256": after},
		}); err != nil {
			return auditWriteError{err}
		}
		return nil
	}
	err := handler.Passkeys.SetGuardPolicy(ctx, userID, workspaceID, body.BaseSHA256, after, body.Assertion, check, write)
	if stale, isStale := err.(guardPolicyStale); isStale {
		writeGuardPolicyStale(writer, stale)
		return
	}
	if err != nil {
		if failedAudit(writer, request, err, guardPolicyAuditKind) {
			return
		}
		writeRevokeError(writer, err)
		return
	}
	handler.publishGuardPolicy(ctx, workspace, userID)
	writeJSON(writer, http.StatusOK, guardPolicyVersion{Policy: body.Policy, SHA256: after})
}

// publishGuardPolicy announces the committed settings to clients and wakes
// every member's daemons, which cache workspace settings.
func (handler *Handler) publishGuardPolicy(ctx context.Context, workspace db.Workspace, userID pgtype.UUID) {
	handler.publish(protocol.EventWorkspaceUpdated, uuidToString(workspace.ID), "member", uuidToString(userID), map[string]any{"workspace": handler.workspaceToResponse(workspace)})
	members, err := handler.Queries.ListMembers(ctx, workspace.ID)
	if err != nil {
		return
	}
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, uuidToString(member.UserID))
	}
	handler.notifyDaemonWorkspacesChanged(ids...)
}
