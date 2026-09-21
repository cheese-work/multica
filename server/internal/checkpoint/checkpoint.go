// Package checkpoint implements checkpoint-based briefing (CHE-489): a
// per-issue, per-agent record of what a prior run already covered, so a later
// run can skip re-reading resolved history instead of re-scanning the whole
// comment tree from scratch.
//
// It deliberately invokes no model — a checkpoint is built and diffed with
// plain data, never summarized — and it never drops an unresolved obligation,
// approval restriction, or evidence reference to fit a token target
// (CHE-482 v2 stop conditions).
//
// The substrate is the existing bounded comment scan
// (ListRootCommentsForIssue / `comment list --roots-only --summary`), which
// already returns, per thread, exactly the tuple a checkpoint diffs against:
// root id, reply_count, and last_activity_at. This package reuses that
// projection; it does not introduce a new traversal.
package checkpoint

import "time"

// ThreadCoverage is the durable per-thread projection a checkpoint stores and
// diffs against. It mirrors the roots-only scan tuple exactly (root id,
// reply_count, last_activity_at) plus whether the thread carries a resolution,
// which the fold-aware readers (foldResolvedThreads) already compute.
//
// reply_count and last_activity_at are durable and permanent: they are
// derived from the comment table's own rows, not from any dedup identity tied
// to a task's lifecycle (agent_task_queue's pending-slot and rerun-lineage
// indexes are both scoped to non-terminal task status and are released on
// completion — neither survives past the run that used it). A checkpoint that
// needs identity outlasting a run's completion therefore keys off this tuple,
// not off task dedup state.
type ThreadCoverage struct {
	ThreadRootID   string
	ReplyCount     int
	LastActivityAt time.Time
	Resolved       bool
}

// Obligation is an unresolved ask the checkpoint must never truncate: a
// pending human request, a mandatory instruction, an approval restriction
// still in force. CHE-482 v2's hard stop condition is precisely that these
// are never dropped to meet a token target.
type Obligation struct {
	// Description is the obligation itself, stated concretely enough to act
	// on without re-reading the source thread (e.g. "wait for @sol review
	// before merge", "do not truncate unresolved obligations").
	Description string
	// SourceThreadID is the thread this obligation was raised in, so a reader
	// who wants the original wording can jump straight to it.
	SourceThreadID string
	// Kind distinguishes ordinary follow-ups from restrictions the workflow
	// must actively honor (an approval gate, a human-only decision).
	Kind ObligationKind
}

// ObligationKind classifies an Obligation for the renderer, so approval
// restrictions render with the weight the content contract requires and are
// never collapsed into an ordinary bullet.
type ObligationKind string

const (
	ObligationGeneral    ObligationKind = "general"
	ObligationApproval   ObligationKind = "approval_restriction"
	ObligationHumanBlock ObligationKind = "human_request"
)

// EvidenceLink is a check or reviewer receipt the checkpoint must keep
// retrievable — the content contract requires reviewer/check identity, not
// just a URL.
type EvidenceLink struct {
	Description string // what the evidence is (e.g. "native CI run", "Terra fixture review")
	Ref         string // URL, PR check name, or other retrievable pointer
	Identity    string // the check name or reviewer identity that produced it
}

// ResolvedNote records a thread the checkpoint has chosen to omit from full
// coverage. The content contract permits omitting resolved history ONLY when
// its resolution and evidence are recorded and retrievable — this is that
// record, kept even though the thread's own comments are not re-rendered.
type ResolvedNote struct {
	ThreadRootID string
	Resolution   string // the settled outcome, stated plainly
	EvidenceRef  string // where the resolving evidence lives, if any
}

// Checkpoint is the valid-checkpoint content contract from CHE-482 v2 (line
// 30): owner and revision, coverage cursor, current outcome/candidate,
// accepted decisions and approval references, unresolved obligations,
// blockers, next permitted action, and evidence links with check/reviewer
// identities. Resolved history may be omitted only when ResolvedThreads
// records its resolution and evidence.
type Checkpoint struct {
	// Owner/revision identity.
	IssueID     string
	AgentID     string
	IssueRev    int64  // issue revision this checkpoint was built against
	CandidateID string // current outcome/candidate identity (e.g. PR head SHA)

	// Coverage cursor: the per-thread tuple this checkpoint covered, keyed by
	// ThreadRootID. This is what Diff compares against a live scan.
	Coverage map[string]ThreadCoverage

	// Content contract fields. None of these are ever truncated to fit a
	// token budget (CHE-482 v2 stop condition).
	AcceptedDecisions   []string
	Obligations         []Obligation
	Blockers            []string
	NextPermittedAction string
	Evidence            []EvidenceLink
	ResolvedThreads     []ResolvedNote

	BuiltAt time.Time
}

// Diff compares a checkpoint's stored coverage against a live scan and
// reports which threads must be expanded because they are changed or
// uncovered, plus whether the checkpoint as a whole is stale.
//
// A thread is "changed" when its live reply_count, last_activity_at, or
// resolved state differs from what the checkpoint recorded — new substantive
// feedback invalidates that thread's coverage, per the content contract.
// A thread present live but absent from the checkpoint's coverage is
// "uncovered" and must also be expanded. A thread the checkpoint covered that
// no longer appears live is not reported — comments are never deleted, so
// this cannot happen against a real scan; if it does (a defensive read
// mismatch), it is silently ignored rather than treated as a false-positive
// staleness signal.
//
// The checkpoint's CandidateID and IssueRev are compared first: either
// mismatching against the live values passed in invalidates every thread
// unconditionally (a new candidate or a materially updated issue means the
// checkpoint's accepted-decisions/obligations fields themselves may be out of
// date, not just the thread coverage), and the changed-thread list becomes
// "all live threads" as documented at the call site (see BuildFromScan).
func Diff(cp Checkpoint, live []ThreadCoverage) (changed []string, stale bool) {
	changed = make([]string, 0, len(live))
	for _, lt := range live {
		prev, ok := cp.Coverage[lt.ThreadRootID]
		if !ok {
			changed = append(changed, lt.ThreadRootID)
			continue
		}
		if prev.ReplyCount != lt.ReplyCount ||
			!prev.LastActivityAt.Equal(lt.LastActivityAt) ||
			prev.Resolved != lt.Resolved {
			changed = append(changed, lt.ThreadRootID)
		}
	}
	stale = len(changed) > 0
	return changed, stale
}

// CandidateChanged reports whether the checkpoint's recorded candidate/issue
// revision identity differs from the live values — the coarse invalidation
// check that must run before the per-thread Diff, per the content contract's
// "compare live revisions, relevant event generations, and candidate
// identity" rule.
func CandidateChanged(cp Checkpoint, liveIssueRev int64, liveCandidateID string) bool {
	return cp.IssueRev != liveIssueRev || cp.CandidateID != liveCandidateID
}
