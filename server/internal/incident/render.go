package incident

import (
	"fmt"
	"strings"
	"time"
)

var ict = time.FixedZone("ICT", 7*60*60)

func fmtTime(t time.Time) string { return t.In(ict).Format("02/01/2006 15:04 ICT") }

// Names resolves ids to display text; a missing id renders as itself.
type Names func(kind, id string) string

func (n Names) list(kind string, ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		v := id
		if n != nil {
			if s := n(kind, id); s != "" {
				v = s
			}
		}
		out = append(out, v)
	}
	return strings.Join(out, ", ")
}

func Title(c Cluster) string {
	return fmt.Sprintf("incident(provider): %s, %d failed runs on %d issues", c.Class, len(c.Runs), len(c.Issues))
}

// Body is the incident issue description.
func Body(c Cluster, n Names) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## What broke\n\n%d runs failed with provider error class `%s` between %s and %s.\n\n", len(c.Runs), c.Class, fmtTime(c.First), fmtTime(c.Last))
	if e := c.Runs[len(c.Runs)-1].Error; e != "" {
		fmt.Fprintf(&b, "Last error:\n\n```\n%s\n```\n\n", truncate(e, 400))
	}
	fmt.Fprintf(&b, "## Affected\n\n- Agents: %s\n- Runtimes: %s\n- Issues: %s\n\n", n.list("agent", c.Agents), n.list("runtime", c.Runtimes), n.list("issue", c.Issues))
	b.WriteString("## What happens next\n\nLater failures of this class are added here as comments; no second incident opens for it. The incident closes after 30 minutes with no new failure of this class.\n")
	return b.String()
}

// CommentBody lists the failures added since the last record.
func CommentBody(runs []Run, n Names) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d more failed runs, last at %s.\n\n", len(runs), fmtTime(runs[len(runs)-1].FailedAt))
	for _, r := range runs {
		fmt.Fprintf(&b, "- %s: %s on %s\n", fmtTime(r.FailedAt), n.list("agent", []string{r.AgentID}), n.list("issue", []string{r.IssueID}))
	}
	return b.String()
}

// CloseBody is the closing comment.
func CloseBody(lastFailure time.Time, retry int) string {
	return fmt.Sprintf("No failure of this class since %s (30 minutes). Closing. %d failed runs were recorded on this incident; retrying them once is a separate executor and has not run from here.", fmtTime(lastFailure), retry)
}

// NoticeTitle and NoticeBody are the one notification per incident.
func NoticeTitle(c Class) string { return "Provider incident: " + string(c) }

func NoticeBody(c Cluster, n Names) string {
	return fmt.Sprintf("%d runs of %s failed with %s since %s. Affected issues: %s. One incident issue tracks it; you get no further notice until it closes or needs you.",
		len(c.Runs), n.list("agent", c.Agents), c.Class, fmtTime(c.First), n.list("issue", c.Issues))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
