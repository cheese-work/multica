package checkpoint

import (
	"fmt"
	"strings"
)

// Render produces the per-turn context block for a checkpoint, in the same
// spirit as daemon.perTurnContextBlocks' other builders: it is meant to be
// appended after the cached prompt prefix, not prepended, so a changing
// checkpoint costs only that turn's tokens (MUL-5377). It performs no
// summarization and drops nothing in Obligations, Blockers, or Evidence —
// those fields are written out in full regardless of length, per the CHE-482
// v2 stop condition.
//
// changedThreadIDs names threads the caller determined (via Diff) are stale
// and must be re-read in full; the block tells the agent which ones rather
// than making it recompute the diff.
func Render(cp Checkpoint, changedThreadIDs []string) string {
	if cp.IssueID == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Checkpoint from a prior run\n\n")
	fmt.Fprintf(&b, "Owner: agent %s. Issue revision at checkpoint time: %d. Candidate: %s.\n\n",
		cp.AgentID, cp.IssueRev, nonEmpty(cp.CandidateID, "(none recorded)"))

	if cp.NextPermittedAction != "" {
		fmt.Fprintf(&b, "**Next permitted action:** %s\n\n", cp.NextPermittedAction)
	}

	if len(cp.AcceptedDecisions) > 0 {
		b.WriteString("**Accepted decisions:**\n\n")
		for _, d := range cp.AcceptedDecisions {
			fmt.Fprintf(&b, "- %s\n", d)
		}
		b.WriteString("\n")
	}

	// Obligations are never truncated — CHE-482 v2 hard stop. Rendered in
	// full regardless of count.
	if len(cp.Obligations) > 0 {
		b.WriteString("**Unresolved obligations (do not drop any of these):**\n\n")
		for _, o := range cp.Obligations {
			label := ""
			switch o.Kind {
			case ObligationApproval:
				label = " [approval restriction]"
			case ObligationHumanBlock:
				label = " [human request]"
			}
			if o.SourceThreadID != "" {
				fmt.Fprintf(&b, "- %s%s (thread %s)\n", o.Description, label, o.SourceThreadID)
			} else {
				fmt.Fprintf(&b, "- %s%s\n", o.Description, label)
			}
		}
		b.WriteString("\n")
	}

	if len(cp.Blockers) > 0 {
		b.WriteString("**Blockers:**\n\n")
		for _, bl := range cp.Blockers {
			fmt.Fprintf(&b, "- %s\n", bl)
		}
		b.WriteString("\n")
	}

	if len(cp.Evidence) > 0 {
		b.WriteString("**Evidence:**\n\n")
		for _, e := range cp.Evidence {
			fmt.Fprintf(&b, "- %s: %s (%s)\n", e.Description, e.Ref, e.Identity)
		}
		b.WriteString("\n")
	}

	if len(cp.ResolvedThreads) > 0 {
		b.WriteString("**Resolved history omitted below — resolution recorded here:**\n\n")
		for _, r := range cp.ResolvedThreads {
			if r.EvidenceRef != "" {
				fmt.Fprintf(&b, "- thread %s: %s (evidence: %s)\n", r.ThreadRootID, r.Resolution, r.EvidenceRef)
			} else {
				fmt.Fprintf(&b, "- thread %s: %s\n", r.ThreadRootID, r.Resolution)
			}
		}
		b.WriteString("\n")
	}

	if len(changedThreadIDs) > 0 {
		b.WriteString("**Changed or uncovered since this checkpoint — expand these threads in full, do not rely on the summary above for them:**\n\n")
		for _, id := range changedThreadIDs {
			fmt.Fprintf(&b, "- %s\n", id)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("No thread has moved since this checkpoint. The resolved-history omissions above are safe to trust without re-reading.\n\n")
	}

	return b.String()
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
