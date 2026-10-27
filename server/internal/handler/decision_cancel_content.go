package handler

import (
	"fmt"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const decisionCancelAskRunes = 100

// decisionCancelIssue is what a cancellation message says about the closed issue.
type decisionCancelIssue struct {
	ref      string
	title    string
	status   string
	closedAt time.Time
}

func decisionCancelIssueFor(issue db.Issue, workspace db.Workspace, closedAt time.Time) decisionCancelIssue {
	identifier := fmt.Sprintf("%s-%d", issuePrefixForWorkspace(workspace), issue.Number)
	ref := identifier
	if link := channel.IssueWebLink(resolveFrontendAppURL(), workspace.Slug, identifier); link != "" {
		ref = "[" + identifier + "](" + link + ")"
	}
	// The viewer's timezone is not stored on the server, so the close time stays ISO UTC.
	return decisionCancelIssue{ref: ref, title: issue.Title, status: issue.Status, closedAt: closedAt.UTC()}
}

func (cancelled decisionCancelIssue) subject() string {
	return cancelled.ref + ` "` + cancelled.title + `"`
}

func (cancelled decisionCancelIssue) closed() string {
	return fmt.Sprintf("%s closed as %s at %s", cancelled.subject(), cancelled.status, cancelled.closedAt.Format(time.RFC3339))
}

// decisionCancelLine names one request: class, asked time and the first line of its ask.
func decisionCancelLine(request db.DecisionRequest) string {
	return fmt.Sprintf("%s, asked %s: %s", request.EscalationClass, request.CreatedAt.Time.UTC().Format(time.RFC3339), decisionAskPreview(request.Ask))
}

func decisionAskPreview(ask string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(ask), "\n")
	runes := []rune(strings.TrimSpace(first))
	if len(runes) == 0 {
		return "ask text unavailable"
	}
	if len(runes) > decisionCancelAskRunes {
		return string(runes[:decisionCancelAskRunes-1]) + "…"
	}
	return string(runes)
}

// decisionCancelComment is the per-request issue comment.
func decisionCancelComment(reason string, cancelled decisionCancelIssue, request db.DecisionRequest) string {
	line := "\n- " + decisionCancelLine(request)
	if reason != "issue_closed" {
		return decisionCancelOtherContent(reason, cancelled) + line
	}
	if request.Status == "answered" {
		return cancelled.closed() + ", after the answer was recorded." + line + "\nNo action needed."
	}
	return "Decision request cancelled: " + cancelled.closed() + "." + line + "\nNo action needed."
}

// decisionCancelNote is the one DM message for unanswered requests that the same close ends in a session.
func decisionCancelNote(reason string, cancelled decisionCancelIssue, requests []db.DecisionRequest) string {
	if reason != "issue_closed" {
		return decisionCancelOtherContent(reason, cancelled) + "\n- " + decisionCancelLine(requests[0])
	}
	var note strings.Builder
	note.WriteString("Decision requests ended: " + cancelled.closed() + ".")
	for _, request := range requests {
		note.WriteString("\n- " + decisionCancelLine(request) + " (cancelled)")
	}
	return note.String() + "\nNo action needed."
}

func decisionCancelOtherContent(reason string, cancelled decisionCancelIssue) string {
	content := fmt.Sprintf("Decision request cancelled: %s. Issue: %s, status %s.", reason, cancelled.subject(), cancelled.status)
	if reason == "human_removed" {
		content += " The person asked is no longer a workspace member. Ask again with a new decision request."
	}
	return content
}
