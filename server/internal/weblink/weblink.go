// Package weblink builds absolute Multica web URLs for CLI-visible responses.
// Every builder returns "" when any part is empty, so a caller without a
// configured app origin or workspace slug omits the link instead of shipping an
// unroutable one.
package weblink

import (
	"net/url"
	"strings"
)

// Build joins the app origin and a root-relative path.
func Build(appURL, path string) string {
	if appURL == "" || path == "" {
		return ""
	}
	return strings.TrimRight(appURL, "/") + path
}

func Issue(slug, identifier string) string {
	if slug == "" || identifier == "" {
		return ""
	}
	return "/" + url.PathEscape(slug) + "/issues/" + url.PathEscape(identifier)
}

func Comment(slug, identifier, commentID string) string {
	issue := Issue(slug, identifier)
	if issue == "" || commentID == "" {
		return ""
	}
	return issue + "#comment-" + commentID
}

func Chat(slug, sessionID string) string {
	if slug == "" || sessionID == "" {
		return ""
	}
	return "/" + url.PathEscape(slug) + "/chat?session=" + sessionID
}

func Attachment(slug, attachmentID string) string {
	if slug == "" || attachmentID == "" {
		return ""
	}
	return "/" + url.PathEscape(slug) + "/attachments/" + url.PathEscape(attachmentID) + "/preview"
}
