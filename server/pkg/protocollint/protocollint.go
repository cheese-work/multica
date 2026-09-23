// Package protocollint is the deterministic, mechanical replacement for the
// agent-facing "pre-exit protocol" prose in the CLAUDE.md/AGENTS.md brief
// (CHE-529 / CHE-513): reply-parent linkage, comment-authoring shape, and
// self-reported completion evidence must never be trusted from an agent's own
// narration — they are checked here against whatever the server itself
// already persisted for the run, and Check fails loudly, naming exactly which
// assertion tripped, instead of the agent grading its own compliance.
//
// This package holds NO I/O and NO second workflow-tracking model. Every
// field on Input is something a caller already has in hand after loading the
// completing db.AgentTaskQueue row and the db.Comment rows the platform
// persisted around it (see server/internal/handler/daemon.go's CompleteTask,
// which is the one normal pre-exit path every agent turn's completion runs
// through). Where a prose assertion has no server-persisted counterpart at
// all, it is documented as NOT checked here rather than approximated from
// something else (see the package doc comment on the specific assertions
// below and the CHE-529 PR description for the full list).
package protocollint

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// PostedComment is the slice of a persisted db.Comment a caller extracts to
// describe one comment authored by the completing run (comment.source_task_id
// == the run's task id). Only the fields the checks below actually reason
// about are carried, so a caller building this from db.Comment never has to
// know which internal columns matter here.
type PostedComment struct {
	// ID is the comment's own id, used only to name it in a failure message.
	ID string
	// ParentID is comment.parent_id, empty for a top-level comment.
	ParentID string
	// Content is the comment body, scanned only for a self-claimed human
	// waiver (assertion 5) — see waiverClaimRe.
	Content string
}

// OtherComment is a comment on the same issue authored by someone other than
// the completing run, used only to look for a human waiver claim's supporting
// evidence (assertion 5 below).
type OtherComment struct {
	// AuthorType is comment.author_type: "member", "agent", or "system".
	AuthorType string
	// Content is the comment body, scanned only for the literal waiver
	// vocabulary a human reviewer would use — see waiverGrantRe.
	Content string
}

// Input is everything Check reasons about, assembled by the caller from
// already-persisted state. Every field is optional in the sense that a zero
// value means "this turn had none of that activity" — Check must pass cleanly
// on a no-op turn (no comment posted, no status change, no evidence claimed),
// not fail because there was nothing to check (CHE-529 step 3).
type Input struct {
	// RunID names the completing run/task in failure messages. Required only
	// for message quality; Check does not branch on it.
	RunID string

	// TriggerCommentID is the id of the comment that triggered this run, or
	// "" for an assignment-triggered / autopilot / chat run that has no
	// triggering comment (server: agent_task_queue.trigger_comment_id).
	TriggerCommentID string

	// CoalescedCommentIDs are additional comment ids this run's completion
	// covers alongside TriggerCommentID (server:
	// agent_task_queue.coalesced_comment_ids, MUL-4195's at-least-once
	// processing: a member comment that arrives mid-run is merged into the
	// same completing task instead of spawning a second one). A reply parented
	// under any of these is exactly as valid as one parented under
	// TriggerCommentID — this mirrors taskCoversReplyParent's own check
	// server-side (comment.go), which accepts both. Omitting this field would
	// make checkReplyParent flag every legitimate coalesced-reply completion
	// as a violation.
	CoalescedCommentIDs []string

	// PostedComments are the comments this run authored on the issue
	// (comment.source_task_id == this run's task id), in any order.
	PostedComments []PostedComment

	// StatusChanged reports whether this run's completion request changed the
	// issue's status column (a Before != After compare the caller already has
	// from loading the issue before and after the run's status-changing API
	// calls, e.g. via issue.status at task start vs at completion).
	StatusChanged bool

	// StatusReadBack reports whether the run is known to have observed the
	// resulting status after changing it. There is no persisted "a GET
	// happened" record in this codebase (no request audit log, no
	// issue_status_history table — see the CHE-529 PR description), so a
	// caller can only ever set this true when it has independent evidence:
	// today, that is "the run's own completion payload later reports output
	// referencing the confirmed status" or an equivalent explicit signal the
	// caller constructs from data it trusts. Leaving this false is always
	// safe; Check treats false as "not verified" and fails when StatusChanged
	// is true. See CheckSkipsUnverifiableReadback in the test file and the
	// PR description for why this sub-check is intentionally conservative
	// rather than approximated.
	StatusReadBack bool

	// ClaimedEvidenceURL is a completion-evidence URL the run's own
	// completion payload reported (e.g. TaskCompletedPayload.PRURL /
	// TaskCompleteRequest.PRURL). Empty means no evidence was claimed.
	ClaimedEvidenceURL string

	// OtherComments are comments on the same issue authored by someone other
	// than this run, spanning at least the run's own lifetime. Used only to
	// look for a human-authored waiver grant backing a waiver claim in this
	// run's own PostedComments.
	OtherComments []OtherComment
}

// Violation is one failed assertion. Code identifies which assertion tripped
// (stable, for callers/tests to match on); Message is the loud, specific
// human-readable failure — never a bare boolean.
type Violation struct {
	Code    string
	Message string
}

func (v Violation) Error() string { return v.Message }

// Violation codes. Stable identifiers so a caller (or a test) can assert on
// exactly which rule fired without string-matching the prose message.
const (
	// CodeReplyParentMismatch: this run's completing turn was triggered by a
	// specific comment, and this run posted a reply that is neither that
	// trigger comment nor top-level-under-nothing — its parent points
	// somewhere the trigger does not cover.
	//
	// NOTE: this exact rule is ALSO already enforced synchronously, at
	// write-time, by the server's CreateComment handler
	// (server/internal/handler/comment.go: taskCoversReplyParent) — a
	// mismatched reply is rejected with HTTP 409 before it can ever be
	// persisted. Checking it again here, against whatever WAS persisted, is
	// deliberate defense in depth for CHE-529's "make it mechanical, don't
	// trust the agent" goal: it re-verifies the invariant from data instead
	// of assuming the write-time gate never regresses, and it is the one
	// assertion of the five that has a complete, independently-persisted
	// ground truth to check against.
	CodeReplyParentMismatch = "reply_parent_mismatch"

	// CodeStatusChangeNotReadBack: the run changed the issue's status but
	// Input carries no evidence the run observed the result.
	CodeStatusChangeNotReadBack = "status_change_not_read_back"

	// CodeEvidenceURLMalformed: the run's completion payload claims a
	// completion-evidence URL that is not even a well-formed reference to the
	// kind of resource it claims to be (currently: a GitHub pull request
	// URL). This is a syntactic floor, not proof the PR exists or is linked
	// to this issue — see the package doc and the PR description for why
	// deeper verification is not implemented here.
	CodeEvidenceURLMalformed = "evidence_url_malformed"

	// CodeUnsupportedWaiver: a comment this run posted claims a step was
	// explicitly waived by a human, but no comment from a "member" author on
	// this issue actually grants one.
	CodeUnsupportedWaiver = "unsupported_waiver"
)

// Check runs every assertion Input's data supports and returns every
// violation found (nil when none). It never panics on a zero-value Input —
// a turn with no comment, no status change, and no evidence claim is exactly
// the valid no_action / non-issue-run shape CHE-529 requires to pass cleanly.
func Check(in Input) []Violation {
	var violations []Violation

	if v, ok := checkReplyParent(in); !ok {
		violations = append(violations, v)
	}
	if v, ok := checkStatusReadback(in); !ok {
		violations = append(violations, v)
	}
	if v, ok := checkEvidenceURL(in); !ok {
		violations = append(violations, v)
	}
	violations = append(violations, checkUnsupportedWaivers(in)...)

	return violations
}

// checkReplyParent is assertion 2: every comment this run posted must be
// covered by the run's trigger comment, or one of its coalesced comments,
// when the run has one. A run with no TriggerCommentID (assignment/autopilot/
// chat trigger) has no parent constraint to enforce here — the same "no
// trigger, nothing to check" shape taskCoversReplyParent itself uses
// server-side. This mirrors taskCoversReplyParent (comment.go) exactly: both
// TriggerCommentID and every id in CoalescedCommentIDs are valid parents.
func checkReplyParent(in Input) (Violation, bool) {
	if in.TriggerCommentID == "" {
		return Violation{}, true
	}
	for _, c := range in.PostedComments {
		if c.ParentID == "" {
			return Violation{
				Code: CodeReplyParentMismatch,
				Message: fmt.Sprintf(
					"protocol violation: run %s posted top-level comment %s but was triggered by comment %s; reply must be parented under the trigger",
					label(in.RunID), c.ID, in.TriggerCommentID,
				),
			}, false
		}
		if c.ParentID == in.TriggerCommentID {
			continue
		}
		if slices.Contains(in.CoalescedCommentIDs, c.ParentID) {
			continue
		}
		return Violation{
			Code: CodeReplyParentMismatch,
			Message: fmt.Sprintf(
				"protocol violation: run %s posted comment %s with parent %s, but its trigger comment was %s (coalesced: %v)",
				label(in.RunID), c.ID, c.ParentID, in.TriggerCommentID, in.CoalescedCommentIDs,
			),
		}, false
	}
	return Violation{}, true
}

// checkStatusReadback is assertion 3: a status change must be followed by a
// confirmed readback. See Input.StatusReadBack's doc for why this is the one
// check whose "true" branch a caller can only ever set from evidence it
// independently trusts — there is no persisted "a GET happened" fact in this
// schema to check against.
func checkStatusReadback(in Input) (Violation, bool) {
	if !in.StatusChanged {
		return Violation{}, true
	}
	if in.StatusReadBack {
		return Violation{}, true
	}
	return Violation{
		Code: CodeStatusChangeNotReadBack,
		Message: fmt.Sprintf(
			"protocol violation: run %s changed issue status but recorded no readback confirming the resulting status",
			label(in.RunID),
		),
	}, false
}

// githubPRURLPattern is byte-for-byte the same pattern the server's own
// GitHub integration requires to resolve a PR URL back to a tracked pull
// request (server/internal/handler/github_merge_announcement.go:
// githubPRURLRe / parseGitHubPRURL). Reproduced rather than imported:
// internal/handler importing this package is the allowed direction, not the
// reverse, so the two copies are pinned together by
// TestEvidenceURLPatternMatchesGitHubHandlerPattern instead — keep both
// literals in lock-step by hand if either changes.
var githubPRURLPattern = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/(\d+)/?$`)

// checkEvidenceURL is the observable slice of assertion 4: when a run's
// completion payload claims a PR URL as its evidence, the URL must at least
// be a well-formed reference to a real GitHub PR location. This does NOT
// confirm the PR exists, is merged, or is linked to this issue — the server
// has real tables for that (github_pull_request, issue_vcs_pull_request), but
// nothing in the /complete request path cross-checks a claimed pr_url against
// them today, and wiring that lookup up is future work called out in the
// CHE-529 PR description, not attempted here.
func checkEvidenceURL(in Input) (Violation, bool) {
	if in.ClaimedEvidenceURL == "" {
		return Violation{}, true
	}
	if githubPRURLPattern.MatchString(in.ClaimedEvidenceURL) {
		return Violation{}, true
	}
	return Violation{
		Code: CodeEvidenceURLMalformed,
		Message: fmt.Sprintf(
			"protocol violation: run %s claimed completion evidence URL %q which is not a recognizable GitHub pull request URL",
			label(in.RunID), in.ClaimedEvidenceURL,
		),
	}, false
}

// workflowStepRe names the Multica workflow steps this package owns. A
// waiver claim or grant only counts when its verb applies to one of these
// (see waiverClaimRe / waiverGrantRe).
//
// Generic nouns ("review", "verification", "evidence", "status") are
// ordinary engineering vocabulary, so alone they are not a step: "the ABI
// review was waived" and "skip review of the ABI check" must not count. They
// qualify only as a workflow compound ("protocol review", "independent
// review", "status readback"), with an ordinal label ("Review #2",
// "verification B"), or as "<noun> step(s)" directly after a determiner
// ("the verification and review steps"). The determiner is what binds the
// noun to the step: "the ABI verification steps" has a foreign qualifier in
// between, so it stays out. A bare "step(s)" likewise needs a determiner, so
// "the ABI step" stays out.
const workflowStepRe = `(?:` +
	`protocol(?:\s+(?:review|check|steps?))?` +
	`|workflow\s+(?:review|steps?)` +
	`|(?:independent|exact-SHA|PR|code)\s+review` +
	`|status\s+(?:change|update|read-?back)` +
	`|comment\s+scan|CI\s+gate` +
	`|` + stepDeterminerRe + `\s+(?:` + genericStepNounRe + `(?:\s*(?:,|and|or|&)\s*` + genericStepNounRe + `)*\s+)?steps?` +
	`|(?:review|verification|evidence)\s+(?:(?-i:[A-Z])|#?\d+)` +
	`)`

const stepDeterminerRe = `(?:this|that|these|those|the|each|every|both|all(?:\s+the)?)`

const genericStepNounRe = `(?:review|verification|read-?back|evidence|status)`

// stepRefRe is a workflow step with an optional determiner before it and an
// optional ordinal label after it ("protocol review A"). The label is
// deliberately narrow: "status check" must not become a step reference.
const stepRefRe = `(?:(?:the|this|that)\s+)?` + workflowStepRe + `(?:\s+(?:(?-i:[A-Z])|#?\d+))?`

// waiverClaimRe matches this run's own comment text claiming a human waived a
// Multica workflow step, in the vocabulary the CLAUDE.md brief warns against
// fabricating: "<step> was (explicitly) waived", "<step> was skipped with
// approval", "waived <step>", "skipped <step> per waiver".
//
// CHE-681: the waiver verb must apply to the step, not merely share a
// sentence with it. The previous bare "waiver" match (and then sentence-wide
// co-occurrence) fired on CI/ABI governance ("The ABI check was waived by
// release policy. Status ...") and on discussion of this very check.
var waiverClaimRe = regexp.MustCompile(`(?i)\b(?:` +
	stepRefRe + `\s+(?:(?:was|were|is|are|has\s+been|have\s+been|got)\s+)?(?:(?:\w+ly|also|already)\s+)*(?:waived|skipped\s+(?:with|per)\s+(?:approval|waiver))` +
	`|waived\s+` + stepRefRe +
	`|skipp(?:ed|ing)\s+` + stepRefRe + `\s+(?:with|per)\s+(?:approval|waiver)` +
	`|` + stepWaiverGrantedRe +
	`)\b`)

// stepWaiverGrantedRe is "<step> waiver (was) granted" or "waiver (was)
// granted for <step>". An agent writing it is a claim; a member writing it is
// a grant, so both regexes share it.
const stepWaiverGrantedRe = stepRefRe + `\s+waiver\s+` + grantedRe +
	`|waiver\s+` + grantedRe + `\s+for\s+` + stepRefRe

const grantedRe = `(?:(?:was|has\s+been|is)\s+)?(?:granted|approved)`

// waiverGrantRe matches a human actually granting one, in a comment authored
// by a workspace member (never an agent or system narration). Like a claim,
// the grant must apply to a workflow step, so an unrelated grant ("ABI waiver
// granted") cannot mask a protocol violation.
var waiverGrantRe = regexp.MustCompile(`(?i)\b(?:` +
	`(?:i\s+waive|you\s+(?:can|may)\s+skip|(?:approved?|ok(?:ay)?)\s+to\s+skip)\s+` + stepRefRe +
	`|` + stepWaiverGrantedRe +
	`)\b`)

// quotedRe finds code spans and double-quoted text. A quoted span that
// itself holds waiver wording is a mention (e.g. discussing what this check
// flags), not a claim or grant, so it is dropped. Any other quoted span is
// just formatting ("The `protocol review` was waived."), so its text is kept.
var quotedRe = regexp.MustCompile("`[^`]*`|\"[^\"\n]*\"|\u201c[^\u201d\n]*\u201d")

var waiverWordRe = regexp.MustCompile(`(?i)waiv|skip|grant`)

// checkUnsupportedWaivers is assertion 5: a run must not claim, in its own
// posted comments, that a human waived some step unless a member's comment on
// this issue actually grants one. Every posted comment claiming a waiver
// without supporting cover is reported (not just the first), since each is an
// independent fabricated claim.
func checkUnsupportedWaivers(in Input) []Violation {
	granted := false
	for _, oc := range in.OtherComments {
		if oc.AuthorType == "member" && waiverGrantRe.MatchString(unquoted(oc.Content)) {
			granted = true
			break
		}
	}

	var violations []Violation
	for _, c := range in.PostedComments {
		if !waiverClaimRe.MatchString(unquoted(c.Content)) {
			continue
		}
		if granted {
			continue
		}
		violations = append(violations, Violation{
			Code: CodeUnsupportedWaiver,
			Message: fmt.Sprintf(
				"protocol violation: run %s comment %s claims a human waiver, but no member comment on this issue grants one",
				label(in.RunID), c.ID,
			),
		})
	}
	return violations
}

func unquoted(content string) string {
	return quotedRe.ReplaceAllStringFunc(content, func(span string) string {
		if waiverWordRe.MatchString(span) {
			return " "
		}
		return strings.Trim(span, "`\"\u201c\u201d")
	})
}

func label(runID string) string {
	if strings.TrimSpace(runID) == "" {
		return "<unknown>"
	}
	return runID
}
