package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// upstream_checked (CHE-5583, CHE-5574 step 6): a create of a feat/plan issue
// into a project the gate lists needs the property "Upstream check" to hold a
// valid value. An untyped title is refused whatever the property holds: the
// type is what tells the gate to ask. Warn mode lets the create pass and
// logs the gap; off mode skips the gate.

const upstreamCheckPropertyName = "Upstream check"

// upstreamCheckValue is the text the issue holds in the Upstream check
// property; "" when the property is undefined or unset. A read failure is an
// error, so the create is refused rather than let through uncited.
func upstreamCheckValue(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, properties []byte) (string, error) {
	defs, err := q.ListIssueProperties(ctx, db.ListIssuePropertiesParams{WorkspaceID: workspaceID})
	if err != nil {
		return "", err
	}
	for _, def := range defs {
		if !strings.EqualFold(def.Name, upstreamCheckPropertyName) {
			continue
		}
		var values map[string]json.RawMessage
		if len(properties) > 0 {
			if err := json.Unmarshal(properties, &values); err != nil {
				return "", err
			}
		}
		var text string
		if json.Unmarshal(values[uuidToString(def.ID)], &text) != nil {
			return "", nil // unset (null or absent)
		}
		return text, nil
	}
	return "", nil
}

func (h *Handler) upstreamCreateGate(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, def *issueworkflow.Definition, properties []byte, projectID pgtype.UUID, title string) error {
	mode := issueworkflow.UpstreamGateMode(def, uuidToString(projectID))
	if mode == "" {
		return nil
	}
	gated, untyped := issueworkflow.UpstreamGatedTitle(title)
	if !gated {
		return nil
	}
	if !untyped {
		value, err := upstreamCheckValue(ctx, q, workspaceID, properties)
		if err != nil {
			return err
		}
		if issueworkflow.UpstreamCheckValid(value) {
			return nil
		}
	}
	if mode != issueworkflow.EntryModeBlock {
		slog.Warn("upstream_checked: create without a valid Upstream check", "project_id", uuidToString(projectID), "title", title)
		return nil
	}
	name := uuidToString(projectID)
	if project, err := q.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: workspaceID}); err == nil {
		name = project.Title
	}
	failure := issueworkflow.EntryFailure{Guard: issueworkflow.GuardUpstreamChecked, Mode: mode}
	if untyped {
		failure.Fact = fmt.Sprintf("upstream check missing: title has no type prefix in project %s", name)
		failure.Fix = "start the title with feat(...), plan(...), chore(...) or fix(...) and retry"
	} else {
		failure.Fact = fmt.Sprintf(`upstream check missing: feat/plan in project %s needs property "Upstream check" = a github.com/multica-ai/multica link, "none found: <query>", or "not multica: <repo>"`, name)
		failure.Fix = "add --property 'Upstream check=<value>' and retry"
	}
	return &EntryGateError{Step: "create", Failed: []issueworkflow.EntryFailure{failure}}
}
