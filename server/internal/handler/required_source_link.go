package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/sourcelink"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Server-side enforcement of required source links (CHE-408).
//
// The defect: an agent writes "read the authoritative 01-22 plan" and ships it.
// The reader cannot follow a sentence, so the obligation was discharged in
// appearance only (CHE-153). A repository-side or CLI-side validator only binds
// writers that cooperate — a raw API client bypasses it entirely — so the check
// has to live here, on the write path itself.
//
// Three properties this guard is built to have, each load-bearing:
//
//  1. It runs before persistence on every entrypoint it covers. A rejected
//     request must leave no issue, no comment, no description change, and must
//     dispatch no agent run. Dispatch is synchronous and strictly downstream of
//     persistence in all four handlers, so "before persistence" also buys
//     "before dispatch" — but only if the call site is placed correctly, which
//     is why each one carries a comment saying what it must stay ahead of.
//  2. The obligation is declared out of band. It lives in a url-typed issue
//     property, written through SetIssueProperty — a different endpoint from the
//     one writing the body. An in-description marker would have let the author
//     of the untrusted text delete its own obligation.
//  3. It binds machine actors only. A human writing prose about a plan is
//     making an editorial choice; an agent doing it is evading a contract. Human
//     writes are out of scope here by design, not by oversight.
const requiredSourcePropertyName = "required source"

// requiredSourceLinkEnabled reports whether this workspace opted in.
//
// Default OFF when unset (Cheese, CHE-408): enforcement changes what agent
// writes are accepted, so it is opt-in per workspace rather than something that
// silently binds every existing workspace on deploy.
//
// A MALFORMED blob fails closed (enforcement ON). Unparseable settings mean we
// cannot prove the workspace did not opt in, and for a security control the
// safe reading of "cannot tell" is to enforce. This deliberately differs from
// the github_* readers above, which default permissive because they gate a
// convenience feature rather than a contract.
func requiredSourceLinkEnabled(ws db.Workspace) bool {
	if len(ws.Settings) == 0 {
		return false
	}
	var s struct {
		RequireSourceLink *bool `json:"require_source_link"`
	}
	if err := json.Unmarshal(ws.Settings, &s); err != nil {
		return true
	}
	return s.RequireSourceLink != nil && *s.RequireSourceLink
}

func (h *Handler) requiredSourceLinkEnabled(ctx context.Context, workspaceID pgtype.UUID) bool {
	ws, err := h.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		// Fail closed, for the same reason a malformed blob does: we cannot
		// show the workspace opted out.
		return true
	}
	return requiredSourceLinkEnabled(ws)
}

// declaredRequiredSource returns the source this issue requires its agent-
// authored content to cite, or "" when none is declared.
//
// The declaration is a url-typed property whose value the property endpoint has
// already validated as an http(s) URL with a host, so this reads a value that
// is well-formed by construction rather than re-deriving trust in it.
func (h *Handler) declaredRequiredSource(ctx context.Context, issue db.Issue) string {
	if len(issue.Properties) == 0 {
		return ""
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(issue.Properties, &values); err != nil {
		return ""
	}
	if len(values) == 0 {
		return ""
	}

	defs, err := h.Queries.ListIssueProperties(ctx, db.ListIssuePropertiesParams{
		WorkspaceID: issue.WorkspaceID,
		// Archived definitions still count: archiving a property stops NEW
		// values being set, it does not retract an obligation already recorded
		// on this issue. Dropping it here would turn "archive the property"
		// into a way to clear the requirement.
		IncludeArchived: true,
	})
	if err != nil {
		return ""
	}

	for _, def := range defs {
		if def.Type != "url" || !strings.EqualFold(strings.TrimSpace(def.Name), requiredSourcePropertyName) {
			continue
		}
		raw, ok := values[uuidToString(def.ID)]
		if !ok {
			continue
		}
		var source string
		if err := json.Unmarshal(raw, &source); err != nil {
			continue
		}
		if s := strings.TrimSpace(source); s != "" {
			return s
		}
	}
	return ""
}

// rejectMissingRequiredSourceLink writes the error response and returns true
// when content must be refused. The caller must return immediately on true —
// same contract as rejectGovernedFieldForAgentActor, and for the same reason:
// the response is already written.
//
// contentWillBeWritten must be computed from the decoded struct field the write
// path actually branches on, never from a raw map lookup, because encoding/json
// matches keys case-insensitively and a case-sensitive presence check is how
// CHE-455's guard was bypassed on PR #29.
func (h *Handler) rejectMissingRequiredSourceLink(
	w http.ResponseWriter,
	r *http.Request,
	actorType string,
	contentWillBeWritten bool,
	issue db.Issue,
	content string,
	field string,
) bool {
	if !contentWillBeWritten || !isGovernedFieldMachineActor(r, actorType) {
		return false
	}
	if !h.requiredSourceLinkEnabled(r.Context(), issue.WorkspaceID) {
		return false
	}
	required := h.declaredRequiredSource(r.Context(), issue)
	if required == "" {
		return false
	}
	verdict := sourcelink.Check(content, required)
	if verdict.OK {
		return false
	}

	// Structured, not a bare sentence: the writer is a machine that has to
	// decide what to change. `code` lets it branch, `required_source` tells it
	// exactly which URL to link, and `field` says which part of its request was
	// at fault. writeError's flat {"error": string} could carry none of that.
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":           verdict.Reason,
		"code":            "required_source_link_missing",
		"field":           field,
		"required_source": verdict.RequiredSource,
	})
	return true
}
