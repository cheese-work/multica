package channel

import (
	"net/url"

	"github.com/multica-ai/multica/server/internal/weblink"
)

// IssueWebLink builds the Multica web deep link for a channel issue result.
// Web routes are workspace-scoped — /{workspaceSlug}/issues/{identifier} — so a
// link missing the slug cannot resolve to the issue and is not worth sending;
// any empty part yields an empty string and the caller replies without a link.
// That is deliberate for the adapters that will reuse this helper: an adapter
// that forgets to plumb the slug through loses its link loudly, instead of
// silently shipping the unroutable legacy /issues/{identifier} form again.
func IssueWebLink(appURL, workspaceSlug, identifier string) string {
	return weblink.Build(appURL, weblink.Issue(workspaceSlug, identifier))
}

// ReferenceWebLink builds detail links for the navigable mention types.
// Member references address users, not workspace membership rows.
func ReferenceWebLink(appURL, workspaceSlug, kind, id string) string {
	if kind == "issue" {
		return IssueWebLink(appURL, workspaceSlug, id)
	}
	route := map[string]string{
		"project": "projects", "agent": "agents",
		"squad": "squads", "member": "members",
	}[kind]
	if workspaceSlug == "" || id == "" || route == "" {
		return ""
	}
	return weblink.Build(appURL, "/"+url.PathEscape(workspaceSlug)+"/"+route+"/"+url.PathEscape(id))
}
