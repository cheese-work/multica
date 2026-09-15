package handler

import (
	"context"
	"strings"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// statusQueryKind is the closed vocabulary of deterministic status questions
// the web issue-detail comment composer answers without admitting an agent
// (CHE-487 Unit D1). A comment matching one of these exactly (after trimming
// outer whitespace and case folding) never reaches triggerTasksForComment.
type statusQueryKind string

const (
	statusQueryStatus     statusQueryKind = "status"
	statusQueryETA        statusQueryKind = "eta"
	statusQueryActiveRuns statusQueryKind = "active_runs"
	statusQueryCI         statusQueryKind = "ci"
	statusQueryPRHead     statusQueryKind = "pr_head"
)

// statusQueryPhrases maps the exact, case-insensitive comment text to its
// statusQueryKind. Deliberately a closed map, not a regex or prefix match:
// "status?", "status please", and "" must all miss.
var statusQueryPhrases = map[string]statusQueryKind{
	"status":      statusQueryStatus,
	"eta":         statusQueryETA,
	"active runs": statusQueryActiveRuns,
	"ci":          statusQueryCI,
	"pr head":     statusQueryPRHead,
}

// parseStatusQuery reports whether content, trimmed of outer whitespace and
// compared case-insensitively, equals exactly one of the five reserved status
// phrases. Any additional word, punctuation, or instruction ("status and also
// fix the tests", "status?") is NOT a match and falls through to the normal
// trigger path unchanged.
func parseStatusQuery(content string) (statusQueryKind, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "", false
	}
	kind, ok := statusQueryPhrases[strings.ToLower(trimmed)]
	return kind, ok
}

// statusAnswerAvailability reports how trustworthy a StatusAnswer's payload
// is. Every switch over it needs a default case (CLAUDE.md API Compatibility
// rule for server-driven enums).
type statusAnswerAvailability string

const (
	// availabilityCurrent means the payload was read fresh from the source of
	// truth for this request.
	availabilityCurrent statusAnswerAvailability = "current"
	// availabilityStale means the payload is last-known data older than the
	// source's own staleness threshold (e.g. an un-refreshed PR snapshot).
	availabilityStale statusAnswerAvailability = "stale"
	// availabilityUnavailable means there is nothing to report: a query
	// failed, no source exists (ETA), or nothing is linked (no PR).
	availabilityUnavailable statusAnswerAvailability = "unavailable"
	// availabilityUnauthorized means the caller/context could not read the
	// underlying source. Not currently produced by any of the five kinds
	// below (all five read data already scoped to the requesting issue), but
	// is part of the enum contract for future sources that DO gate on
	// caller-specific permissions.
	availabilityUnauthorized statusAnswerAvailability = "unauthorized"
)

// StatusAnswer is the deterministic, timestamped projection returned instead
// of agent admission when a comment's content exactly matches one of the five
// reserved status-query phrases. Every field is sourced from structured data
// already read for other handlers — never inferred, never guessed, never
// fabricated as a success when the source is missing or errored.
type StatusAnswer struct {
	Kind         statusQueryKind          `json:"kind"`
	AnsweredAt   string                   `json:"answered_at"`
	Availability statusAnswerAvailability `json:"availability"`
	// Detail is the human-readable deterministic line rendered in the
	// composer/timeline. Always populated, including on unavailable.
	Detail string `json:"detail"`
	// IssueStatus is populated only for kind=="status".
	IssueStatus *string `json:"issue_status,omitempty"`
	// ActiveRuns is populated only for kind=="active_runs".
	ActiveRuns []StatusAnswerActiveRun `json:"active_runs,omitempty"`
	// PullRequest is populated only for kind=="ci" or kind=="pr_head" when a
	// PR is linked and readable.
	PullRequest *StatusAnswerPullRequest `json:"pull_request,omitempty"`
}

// StatusAnswerActiveRun is one row of the "active runs" answer: the same
// agent/status shape the CLI's `multica issue runs --active` surface reads
// from ListActiveTasksByIssue, projected down to what a status answer needs.
type StatusAnswerActiveRun struct {
	AgentID string `json:"agent_id"`
	Status  string `json:"status"`
}

// StatusAnswerPullRequest is the "ci" / "pr head" answer payload. Both kinds
// share this shape; "ci" callers read ChecksRollup, "pr head" callers read
// HeadSHA and HTMLURL. Never render ChecksRollup == nil as passed — nil means
// no checks have reported (or no snapshot exists), never success.
type StatusAnswerPullRequest struct {
	PullRequestID string `json:"pull_request_id"`
	HTMLURL       string `json:"html_url"`
	HeadSHA       string `json:"head_sha"`
	// ChecksRollup mirrors GitHubPullRequestResponse.ChecksRollup: lowercased
	// "success" | "failure" | "pending" | "error" | "expected", or nil when no
	// snapshot / no checks have reported.
	ChecksRollup *string `json:"checks_rollup"`
	// SnapshotFetchedAt is when the API snapshot backing ChecksRollup was last
	// fetched (RFC3339), or nil when no snapshot has ever landed.
	SnapshotFetchedAt *string `json:"snapshot_fetched_at"`
}

// buildStatusAnswer answers one of the five reserved status-query kinds from
// the same structured sources the CLI D2-D4 surfaces read. It never enqueues
// anything and never falls back to a model: a read failure or absent source
// always resolves to availabilityUnavailable with a stated reason, never a
// guess.
func (h *Handler) buildStatusAnswer(ctx context.Context, issue db.Issue, kind statusQueryKind) StatusAnswer {
	now := time.Now().UTC()
	answer := StatusAnswer{
		Kind:       kind,
		AnsweredAt: now.Format(time.RFC3339),
	}

	switch kind {
	case statusQueryStatus:
		status := issue.Status
		answer.IssueStatus = &status
		answer.Availability = availabilityCurrent
		answer.Detail = "Issue status is " + status + "."

	case statusQueryETA:
		// There is no structured ETA field on an issue anywhere in this
		// codebase — only start_date / due_date exist, and due_date is a
		// planning target, not an estimate the platform computed or was
		// given. Reporting due_date here would silently invent an ETA the
		// system never actually tracked, so this kind is unconditionally
		// unavailable/unknown.
		answer.Availability = availabilityUnavailable
		answer.Detail = "No recorded estimate exists for this issue."

	case statusQueryActiveRuns:
		tasks, err := h.Queries.ListActiveTasksByIssue(ctx, issue.ID)
		if err != nil {
			answer.Availability = availabilityUnavailable
			answer.Detail = "Active runs could not be read right now."
			break
		}
		runs := make([]StatusAnswerActiveRun, len(tasks))
		for i, t := range tasks {
			runs[i] = StatusAnswerActiveRun{
				AgentID: uuidToString(t.AgentID),
				Status:  t.Status,
			}
		}
		answer.ActiveRuns = runs
		answer.Availability = availabilityCurrent
		answer.Detail = activeRunsDetail(len(runs))

	case statusQueryCI, statusQueryPRHead:
		h.fillStatusAnswerFromPullRequest(ctx, issue, kind, &answer)

	default:
		// Unreachable: parseStatusQuery only ever returns a key present in
		// statusQueryPhrases. Kept for the server-driven-enum default-case
		// rule (CLAUDE.md API Compatibility).
		answer.Availability = availabilityUnavailable
		answer.Detail = "Unknown status query."
	}

	return answer
}

func activeRunsDetail(count int) string {
	if count == 0 {
		return "No active runs on this issue."
	}
	if count == 1 {
		return "1 active run on this issue."
	}
	return "Multiple active runs on this issue."
}

// fillStatusAnswerFromPullRequest answers "ci" / "pr head" from the same
// ListPullRequestsByIssue query and issuePullRequestRowToResponse projection
// the issue detail PR list uses. It reads the most recently created linked PR
// (row [0]; ListPullRequestsByIssue orders pr_created_at DESC) — no new query
// is added since this already serves both kinds.
func (h *Handler) fillStatusAnswerFromPullRequest(ctx context.Context, issue db.Issue, kind statusQueryKind, answer *StatusAnswer) {
	rows, err := h.Queries.ListPullRequestsByIssue(ctx, issue.ID)
	if err != nil {
		answer.Availability = availabilityUnavailable
		answer.Detail = prUnavailableDetail(kind, "the linked pull request could not be read")
		return
	}
	if len(rows) == 0 {
		answer.Availability = availabilityUnavailable
		answer.Detail = prUnavailableDetail(kind, "no pull request is linked to this issue")
		return
	}

	applyStatusAnswerFromPullRequestRow(rows[0], h.PRRefresh.Enabled(), kind, answer)
}

// applyStatusAnswerFromPullRequestRow is the pure projection step of
// fillStatusAnswerFromPullRequest, split out so the availability matrix
// (current / stale / unavailable) is unit-testable without depending on
// h.PRRefresh's GitHub App private key configuration — the same split
// TestIssuePullRequestResponseHidesUnavailableSnapshot uses for
// issuePullRequestRowToResponse itself.
func applyStatusAnswerFromPullRequestRow(row db.ListPullRequestsByIssueRow, snapshotEnabled bool, kind statusQueryKind, answer *StatusAnswer) {
	resp := issuePullRequestRowToResponse(row, snapshotEnabled)

	pr := &StatusAnswerPullRequest{
		PullRequestID:     resp.ID,
		HTMLURL:           resp.HtmlURL,
		HeadSHA:           row.HeadSha,
		ChecksRollup:      resp.ChecksRollup,
		SnapshotFetchedAt: resp.SnapshotFetchedAt,
	}
	answer.PullRequest = pr

	switch {
	case resp.SnapshotAvailable == nil || !*resp.SnapshotAvailable:
		// No API snapshot has ever landed for this PR (feature disabled, or
		// no successful fetch yet): never render nil checks as passed.
		answer.Availability = availabilityUnavailable
		answer.Detail = prUnavailableDetail(kind, "no CI snapshot is available for the linked pull request yet")
	case resp.SnapshotStale:
		answer.Availability = availabilityStale
		answer.Detail = prStaleDetail(kind, pr)
	default:
		answer.Availability = availabilityCurrent
		answer.Detail = prCurrentDetail(kind, pr)
	}
}

func prUnavailableDetail(kind statusQueryKind, reason string) string {
	if kind == statusQueryPRHead {
		return "PR head is unavailable: " + reason + "."
	}
	return "CI status is unavailable: " + reason + "."
}

func prStaleDetail(kind statusQueryKind, pr *StatusAnswerPullRequest) string {
	if kind == statusQueryPRHead {
		return "PR head (last known, stale snapshot): " + pr.HeadSHA + "."
	}
	return "CI status (last known, stale snapshot): " + checksRollupLabel(pr.ChecksRollup) + "."
}

func prCurrentDetail(kind statusQueryKind, pr *StatusAnswerPullRequest) string {
	if kind == statusQueryPRHead {
		return "PR head is " + pr.HeadSHA + "."
	}
	return "CI status is " + checksRollupLabel(pr.ChecksRollup) + "."
}

func checksRollupLabel(rollup *string) string {
	if rollup == nil {
		return "no checks reported"
	}
	return *rollup
}
