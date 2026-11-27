package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/designguard"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// G4 at the create-decision API (CHE-7064 D1.7, S12). A plain decision card is
// validated against the doop attestation and the typed verdict record before it
// is stored. Mode off reads nothing; warn turns a refusal into a hint; block
// refuses with the code of the failed check.

// designVerdicts reads the newest design_review_verdict row (C6).
type designVerdicts struct{ queries *db.Queries }

func (reader designVerdicts) NewestDesignVerdict(ctx context.Context, issueID, attestationID string) (designguard.Verdict, bool, error) {
	issue, err := util.ParseUUID(issueID)
	if err != nil {
		return designguard.Verdict{}, false, err
	}
	row, err := reader.queries.GetNewestDesignVerdict(ctx, db.GetNewestDesignVerdictParams{IssueID: issue, AttestationID: attestationID})
	if errors.Is(err, pgx.ErrNoRows) {
		return designguard.Verdict{}, false, nil
	}
	if err != nil {
		return designguard.Verdict{}, false, err
	}
	var details struct {
		Verdict string `json:"verdict"`
		TaskID  string `json:"task_id"`
	}
	if err := json.Unmarshal(row.Details, &details); err != nil {
		return designguard.Verdict{}, false, err
	}
	return designguard.Verdict{Verdict: details.Verdict, AgentID: uuidToString(row.ActorID), TaskID: details.TaskID}, true, nil
}

// designRefusalStatus is the HTTP class of a refusal (plan D1.7): 400 for a
// card defect, 503 when doop, Multica's own lookups or the setting are
// unavailable, 409 for a binding, stale, verdict or independence failure.
func designRefusalStatus(code string) int {
	switch code {
	case designguard.CodeDesignOptionMissing, designguard.CodeDesignFrameURLInvalid, designguard.CodeDesignURLMissing,
		designguard.CodeDesignURLAmbiguous, designguard.CodeDesignAttestationMissing:
		return http.StatusBadRequest
	case designguard.CodeDesignDoopUnavailable, designguard.CodeGuardUnavailable, designguard.CodeGuardMisconfigured:
		return http.StatusServiceUnavailable
	}
	return http.StatusConflict
}

// guardDesignCard runs G4 on a parsed plain card. It returns ok=false after it
// wrote a refusal. On a pass it stamps frame_updated_at into parsed.Options and
// reports stamped=true, so the caller renders the ask again. In warn mode a
// refusal is a hint and the card is stored unstamped.
func (h *Handler) guardDesignCard(w http.ResponseWriter, r *http.Request, issue db.Issue, workspace db.Workspace, parsed *decisiontx.Ask, class string) (hints []string, stamped, ok bool) {
	settings, err := designguard.ParseSettings(workspace.Settings)
	if err != nil {
		writeErrorCode(w, http.StatusServiceUnavailable, designguard.CodeGuardMisconfigured, err.Error())
		return nil, false, false
	}
	if settings.Mode == designguard.ModeOff {
		return nil, false, true
	}
	input := designguard.Input{
		Settings:      settings,
		IssueID:       uuidToString(issue.ID),
		DesignURL:     designURLOf(issue, settings.DesignURLPropertyID),
		Class:         class,
		AskingAgentID: r.Header.Get("X-Agent-ID"),
		Plain:         true,
	}
	if issue.WorkflowID.Valid {
		input.IssueWorkflowID = uuidToString(issue.WorkflowID)
	}
	for _, option := range parsed.Options {
		guarded := designguard.Option{Label: option.Label, WhatHappens: option.WhatHappens}
		if option.Design != nil {
			guarded.Design = &designguard.OptionDesign{FrameURL: option.Design.FrameURL, AttestationID: option.Design.AttestationID}
		}
		input.Options = append(input.Options, guarded)
	}
	deps := designguard.ValidateDeps{
		Verdicts: designVerdicts{queries: h.Queries},
		Independence: designguard.Deps{
			Lab: voteLab,
			Tasks: func(ctx context.Context, ids []string) ([]designguard.Task, error) {
				return h.designTaskRows(ctx, ids)
			},
		},
	}
	if h.DoopReader != nil { // a nil *Reader must not become a non-nil interface
		deps.Attestations = h.DoopReader
	}
	result, refusal := designguard.Validate(r.Context(), input, deps)
	if refusal != nil {
		if settings.Mode == designguard.ModeWarn {
			return []string{designguard.SettingKey + " " + refusal.Code + ": " + refusal.Message}, false, true
		}
		writeErrorCode(w, designRefusalStatus(refusal.Code), refusal.Code, refusal.Message)
		return nil, false, false
	}
	if !result.Triggered {
		return nil, false, true
	}
	for index, checked := range result.Options {
		parsed.Options[index].Design.FrameUpdatedAt = checked.FrameUpdatedAt
	}
	return nil, true, true
}
