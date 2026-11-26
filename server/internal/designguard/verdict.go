package designguard

import (
	"errors"
	"slices"
	"strings"
)

// Refusal codes of the conditions a PASS verdict meets at write (plan D1.3, C3 to C5)
// and of the endpoint's own refusals (plan D1.4, D1.7).
const (
	CodeCanvasMismatch        = "design_canvas_mismatch"
	CodeFrameOutsidePage      = "design_frame_outside_page"
	CodePageNotBound          = "design_page_not_bound"
	CodeAttestationNotFound   = "design_attestation_not_found"
	CodeAttestationIncomplete = "design_attestation_incomplete"
	CodeAttestationStale      = "design_attestation_stale"
	CodeDoopUnavailable       = "design_doop_unavailable"
	CodeGuardMisconfigured    = "design_guard_misconfigured"
)

// Attestation is the part of a doop attestation view that C3 to C5 read.
type Attestation struct {
	CanvasID    string
	PageID      string
	PageIssueID string // empty when the page is bound to no issue
	Complete    bool
	Current     bool
	Frames      []string
}

// CheckVerdictTarget is C2 to C5 for a verdict on issueID: the issue Design url
// and the attestation agree, the page is bound to the issue, and the
// attestation is complete and current. It returns nil when all hold.
func CheckVerdictTarget(origin, designURL, issueID string, a Attestation) *Refusal {
	ref, err := ParseDesignURL(origin, designURL)
	if err != nil {
		var r *Refusal
		if errors.As(err, &r) {
			return r
		}
		return &Refusal{CodeDesignURLMissing, err.Error()}
	}
	if ref.Canvas != a.CanvasID {
		return &Refusal{CodeCanvasMismatch, "the issue Design url names another canvas than the attestation"}
	}
	if ref.Page != "" && ref.Page != a.PageID {
		return &Refusal{CodeFrameOutsidePage, "the issue Design url names another page than the attestation"}
	}
	if ref.Frame != "" && !slices.Contains(a.Frames, ref.Frame) {
		return &Refusal{CodeFrameOutsidePage, "the issue Design url frame is not in the attestation"}
	}
	if a.PageIssueID == "" || !strings.EqualFold(a.PageIssueID, issueID) {
		return &Refusal{CodePageNotBound, "the attested page is not bound to this issue"}
	}
	if !a.Complete {
		return &Refusal{CodeAttestationIncomplete, "the attestation is not complete"}
	}
	if !a.Current {
		return &Refusal{CodeAttestationStale, "the page changed after the attestation"}
	}
	return nil
}
