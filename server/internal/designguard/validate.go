package designguard

import (
	"context"
	"errors"

	"github.com/multica-ai/multica/server/internal/doopread"
)

// Refusal codes of Validate (plan D1.3) that the URL grammar and independence do not own.
const (
	CodeDesignOptionMissing      = "design_option_missing"
	CodeDesignCanvasMismatch     = "design_canvas_mismatch"
	CodeDesignFrameOutsidePage   = "design_frame_outside_page"
	CodeDesignPageNotBound       = "design_page_not_bound"
	CodeDesignAttestationMissing = "design_attestation_not_found"
	CodeDesignAttestationPartial = "design_attestation_incomplete"
	CodeDesignAttestationStale   = "design_attestation_stale"
	CodeDesignFrameNotAttested   = "design_frame_not_attested"
	CodeDesignVerdictMissing     = "design_verdict_missing"
	CodeDesignDoopUnavailable    = "design_doop_unavailable"
	// CodeGuardUnavailable is a failure of Multica's own lookups (verdict or task rows), not of doop.
	CodeGuardUnavailable = "design_guard_unavailable"
)

// ClassProductDirection is the escalation class T1 to T3 apply to.
const ClassProductDirection = "product_direction"

// Option is the part of one card option Validate reads.
type Option struct {
	Label, WhatHappens string
	// Design is nil when the option cites no frame.
	Design *OptionDesign
}

// OptionDesign is the frame an option cites.
type OptionDesign struct{ FrameURL, AttestationID string }

// Input is one card as the create-decision handler sees it. Plain is true for a
// decision card with no action and no waiver topic: only those are guarded.
type Input struct {
	Settings        Settings
	IssueID         string
	IssueWorkflowID string
	DesignURL       string // the issue Design url property value, "" when unset
	Class           string
	AskingAgentID   string
	Plain           bool
	Options         []Option
}

// Verdict is the newest design_review_verdict of an issue for one attestation.
type Verdict struct {
	Verdict string // PASS or CHANGES
	AgentID string
	TaskID  string
}

// AttestationReader reads a doop attestation view; *doopread.Reader implements it.
type AttestationReader interface {
	Attestation(ctx context.Context, id string) (doopread.View, error)
}

// VerdictReader returns the newest verdict of issueID for attestationID.
type VerdictReader interface {
	NewestDesignVerdict(ctx context.Context, issueID, attestationID string) (Verdict, bool, error)
}

// ValidateDeps are the lookups Validate needs. Independence is the S9 lookup pair.
type ValidateDeps struct {
	Attestations AttestationReader
	Verdicts     VerdictReader
	Independence Deps
}

// Checked is the frame one option was checked against.
type Checked struct {
	FrameID        string
	CanvasID       string
	PageID         string
	AttestationID  string
	FrameUpdatedAt int64
}

// Result is what a passing card carries: one entry per option, in option order.
// Triggered is false when the guard does not apply to the card.
type Result struct {
	Triggered bool
	Options   []Checked
}

// Triggers reports whether the guard applies to a card (plan D1.2). Only a plain
// decision card in mode warn or block is guarded. T0: an option carries design,
// for any class. T1 to T3, class product_direction only: the issue Design url
// names a link on doop_origin; the issue is on the Design workflow; an option
// label or what_happens holds a link on doop_origin (a structural link match,
// never a word match, CHE-3345).
func Triggers(in Input) bool {
	if !in.Plain || in.Settings.Mode != ModeWarn && in.Settings.Mode != ModeBlock {
		return false
	}
	for _, option := range in.Options {
		if option.Design != nil {
			return true
		}
	}
	if in.Class != ClassProductDirection {
		return false
	}
	origin := in.Settings.DoopOrigin
	if HasDoopLink(origin, in.DesignURL) || in.Settings.DesignWorkflowID != "" && in.IssueWorkflowID == in.Settings.DesignWorkflowID {
		return true
	}
	for _, option := range in.Options {
		if HasDoopLink(origin, option.Label) || HasDoopLink(origin, option.WhatHappens) {
			return true
		}
	}
	return false
}

// Validate checks every option of a triggered card against C1 to C7 and returns
// the frame each option was checked against. A card the guard does not apply to
// returns an empty Result and no refusal, and reads nothing. The caller decides
// what a refusal means in its mode (warn or block).
func Validate(ctx context.Context, in Input, deps ValidateDeps) (Result, *Refusal) {
	if !Triggers(in) {
		return Result{}, nil
	}
	result := Result{Triggered: true, Options: make([]Checked, 0, len(in.Options))}
	origin := in.Settings.DoopOrigin
	views := map[string]doopread.View{}
	var order []string      // attestation ids in first-use order, so the refusal is deterministic
	var designURL *FrameRef // parsed once, at the first option that reaches C2
	for _, option := range in.Options {
		// C1: design is present and frame_url parses.
		if option.Design == nil || option.Design.AttestationID == "" {
			return Result{}, &Refusal{CodeDesignOptionMissing, "every option must cite a doop frame in options[].design"}
		}
		frame, err := ParseFrameURL(origin, option.Design.FrameURL)
		if err != nil {
			return Result{}, asRefusal(err)
		}
		// C2: the issue Design url is set and parses.
		if designURL == nil {
			parsed, err := ParseDesignURL(origin, in.DesignURL)
			if err != nil {
				return Result{}, asRefusal(err)
			}
			designURL = &parsed
		}
		id := option.Design.AttestationID
		view, seen := views[id]
		if !seen {
			var refusal *Refusal
			if view, refusal = readAttestation(ctx, deps.Attestations, id); refusal != nil {
				return Result{}, refusal
			}
			views[id] = view
			order = append(order, id)
		}
		checked, refusal := checkOption(in, *designURL, frame, id, view)
		if refusal != nil {
			return Result{}, refusal
		}
		result.Options = append(result.Options, checked)
	}
	// C6 and C7 once per attestation: the verdict and the independence rules do
	// not depend on the option.
	for _, id := range order {
		if refusal := checkVerdict(ctx, in, id, views[id], deps); refusal != nil {
			return Result{}, refusal
		}
	}
	return result, nil
}

func asRefusal(err error) *Refusal {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal
	}
	return &Refusal{CodeDesignFrameURLInvalid, err.Error()}
}

func readAttestation(ctx context.Context, reader AttestationReader, id string) (doopread.View, *Refusal) {
	if reader == nil {
		return doopread.View{}, &Refusal{CodeDesignDoopUnavailable, "doop is not configured"}
	}
	view, err := reader.Attestation(ctx, id)
	switch {
	case err == nil:
		return view, nil
	case errors.Is(err, doopread.ErrNotFound):
		return doopread.View{}, &Refusal{CodeDesignAttestationMissing, "doop has no attestation " + id}
	}
	return doopread.View{}, &Refusal{CodeDesignDoopUnavailable, "doop could not be read"}
}

// checkOption runs C3, C4, C4b and C5 for one option against its attestation.
func checkOption(in Input, designURL, frame FrameRef, id string, view doopread.View) (Checked, *Refusal) {
	// C3: option canvas = attestation canvas = Design url canvas.
	if frame.Canvas != view.CanvasID || designURL.Canvas != view.CanvasID {
		return Checked{}, &Refusal{CodeDesignCanvasMismatch, "the option, the Design url and the attestation must name the same canvas"}
	}
	// C4: every part the links name holds, and the option page, if present, is the attested page.
	if designURL.Page != "" && designURL.Page != view.PageID || designURL.Frame != "" && !hasFrame(view, designURL.Frame) ||
		frame.Page != "" && frame.Page != view.PageID {
		return Checked{}, &Refusal{CodeDesignFrameOutsidePage, "the cited frame is outside the page the Design url and the attestation name"}
	}
	// C4b: the attested page is bound to the card's issue.
	if view.PageIssueID == "" || view.PageIssueID != in.IssueID {
		return Checked{}, &Refusal{CodeDesignPageNotBound, "the attested page is not bound to this issue"}
	}
	// C5: complete, current, and the frame is in the attestation.
	switch {
	case !view.Complete:
		return Checked{}, &Refusal{CodeDesignAttestationPartial, "the attestation is incomplete"}
	case !view.Current:
		return Checked{}, &Refusal{CodeDesignAttestationStale, "the page changed after the attestation"}
	}
	for _, f := range view.Frames {
		if f.FrameID == frame.Frame {
			return Checked{FrameID: f.FrameID, CanvasID: view.CanvasID, PageID: view.PageID, AttestationID: id, FrameUpdatedAt: f.UpdatedAt}, nil
		}
	}
	return Checked{}, &Refusal{CodeDesignFrameNotAttested, "the cited frame is not in the attestation"}
}

func hasFrame(view doopread.View, frame string) bool {
	for _, f := range view.Frames {
		if f.FrameID == frame {
			return true
		}
	}
	return false
}

// checkVerdict runs C6 and C7: the newest verdict is PASS, and its reviewer is
// independent of every author of the attested page.
func checkVerdict(ctx context.Context, in Input, id string, view doopread.View, deps ValidateDeps) *Refusal {
	if deps.Verdicts == nil {
		return &Refusal{CodeGuardUnavailable, "the verdict record is not available"}
	}
	verdict, found, err := deps.Verdicts.NewestDesignVerdict(ctx, in.IssueID, id)
	switch {
	case err != nil:
		return &Refusal{CodeGuardUnavailable, "the verdict record could not be read"}
	case !found || verdict.Verdict != "PASS":
		return &Refusal{CodeDesignVerdictMissing, "the newest design verdict for this attestation is not PASS"}
	}
	authors := make([]Author, 0, len(view.Authors))
	for _, a := range view.Authors {
		authors = append(authors, Author{Kind: a.Kind, AgentID: deref(a.AgentID), TaskID: deref(a.TaskID)})
	}
	code, err := CheckIndependence(ctx, View{Authors: authors, ProvenanceComplete: view.ProvenanceComplete},
		Reviewer{AgentID: verdict.AgentID, TaskID: verdict.TaskID}, in.AskingAgentID, deps.Independence)
	switch {
	case err != nil:
		return &Refusal{CodeGuardUnavailable, "the task records could not be read"}
	case code != "":
		return &Refusal{code, "the design review is not independent of the page's authors"}
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
