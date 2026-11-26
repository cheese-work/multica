package util

import "net/url"

// OpenIssueFooter is the closing line of a server-written chat notice. It is an
// issue mention so the web chat renders a chip, not a raw path (CHE-10217).
func OpenIssueFooter(identifier, issueID string) string {
	return "Open issue: [" + identifier + "](mention://issue/" + issueID + ")"
}

func WorkspaceIssuePath(workspaceSlug, identifier string) string {
	if workspaceSlug == "" || identifier == "" {
		return ""
	}
	return "/" + url.PathEscape(workspaceSlug) + "/issues/" + url.PathEscape(identifier)
}
