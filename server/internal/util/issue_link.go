package util

import "net/url"

func WorkspaceIssuePath(workspaceSlug, identifier string) string {
	if workspaceSlug == "" || identifier == "" {
		return ""
	}
	return "/" + url.PathEscape(workspaceSlug) + "/issues/" + url.PathEscape(identifier)
}
