package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/weblink"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Read filter for elevated runs (CHE-2216 plan 3.7). A run holding an elevated
// token must not read text an external source may have written, so a comment or
// description marked external_read, or carrying taint, comes back as a stub:
// author, id, hash and link. Members and restricted runs read full text. The
// approve and downgrade exceptions are B8b-2.

const elevationRestrictedFinal = "restricted_final"

// elevatedReader reports whether r comes from a run whose reads must be
// filtered. Only a trusted task token counts: Auth strips client-supplied
// X-Actor-Source. A task with no elevation row or in restricted_final reads
// full text (T16). A lookup error or an unreadable task id returns true: plan
// 3.4 requires a stub, never full text, when the server cannot tell.
func (h *Handler) elevatedReader(r *http.Request) bool {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		return false
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		return true
	}
	te, err := h.Queries.GetTaskElevation(r.Context(), taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	return err != nil || te.State != elevationRestrictedFinal
}

// readFilter answers "stub this body?" for one request. It looks the elevation
// up once, and only when a marked body appears.
type readFilter struct {
	h    *Handler
	r    *http.Request
	once sync.Once
	stub bool
}

func (h *Handler) newReadFilter(r *http.Request) *readFilter { return &readFilter{h: h, r: r} }

func (f *readFilter) withholds(marked bool) bool {
	if !marked {
		return false
	}
	f.once.Do(func() { f.stub = f.h.elevatedReader(f.r) })
	return f.stub
}

// withheldStub replaces a body. The hash covers the exact bytes withheld, so a
// B3 approval of that hash (B8b-2) can name it.
func withheldStub(kind, id, authorType, authorID, body, link string) string {
	stub := fmt.Sprintf("[withheld: untrusted %s; id=%s author=%s:%s sha256=%s", kind, id, authorType, authorID, sha256Hex(body))
	if link != "" {
		stub += " link=" + link
	}
	return stub + "]"
}

func commentWithheld(c db.Comment) bool { return c.ExternalRead || len(c.TaintIds) > 0 }

// commentStub builds the stub for c; the link is empty when the web URL is unset.
func (h *Handler) commentStub(ctx context.Context, c db.Comment) string {
	id := uuidToString(c.ID)
	link := ""
	if issue, err := h.Queries.GetIssue(ctx, c.IssueID); err == nil {
		link = h.workspaceWebURL(ctx, c.WorkspaceID, func(ws db.Workspace) string {
			return weblink.Comment(ws.Slug, issuePrefixForWorkspace(ws)+"-"+strconv.Itoa(int(issue.Number)), id)
		})
	}
	return withheldStub("comment", id, c.AuthorType, uuidToString(c.AuthorID), c.Content, link)
}

// descriptionStub builds the stub for an issue description.
func (h *Handler) descriptionStub(i db.Issue, resp IssueResponse) string {
	body := ""
	if i.Description.Valid {
		body = i.Description.String
	}
	return withheldStub("issue description", resp.ID, i.CreatorType, uuidToString(i.CreatorID), body, resp.WebURL)
}

// stubDescription replaces resp.Description with a stub when issue's description
// is marked and the reader is elevated. Every route that returns a description
// calls it, so no representation (list, search, write response) hands the text
// back.
func (f *readFilter) stubDescription(h *Handler, i db.Issue, resp *IssueResponse) {
	if f.withholds(i.DescriptionExternalRead) {
		stub := h.descriptionStub(i, *resp)
		resp.Description, resp.DescriptionWithheld = &stub, true
	}
}

// denyWithheldAttachment answers 403 for an elevated reader asking for the
// bytes of a marked attachment (plan 3.7: no representation of a marked body).
func (h *Handler) denyWithheldAttachment(w http.ResponseWriter, r *http.Request, att db.Attachment) bool {
	if att.ExternalRead && h.elevatedReader(r) {
		writeError(w, http.StatusForbidden, "attachment withheld: untrusted content is not readable by an elevated run")
		return true
	}
	return false
}
