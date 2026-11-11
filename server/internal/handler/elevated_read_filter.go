package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/internal/weblink"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Read filter for elevated runs (CHE-2216 plan 3.7). A run holding an elevated
// token must not read text an external source may have written, so a comment or
// description marked external_read, or carrying taint, comes back as a stub:
// author, id, hash and link. Members and restricted runs read full text. Two
// exceptions return the body (B8b-2): a read_snapshot_approval of the exact body
// hash for the issue the run was elevated on (B3), and a completed downgrade, which leaves the task in
// restricted_final so it is no longer an elevated reader.

const elevationRestrictedFinal = "restricted_final"

// elevatedReader reports whether r comes from a run whose reads must be
// filtered. Only a trusted task token counts: Auth strips client-supplied
// X-Actor-Source. A task with no elevation row or in restricted_final reads
// full text (T16). A lookup error or an unreadable task id returns true: plan
// 3.4 requires a stub, never full text, when the server cannot tell.
func (h *Handler) elevatedReader(r *http.Request) bool {
	elevated, _ := h.elevatedReaderIssue(r)
	return elevated
}

// elevatedReaderIssue is elevatedReader plus the issue the run was elevated on,
// which a read approval must name. The issue is invalid when it is unknown.
func (h *Handler) elevatedReaderIssue(r *http.Request) (bool, pgtype.UUID) {
	if r.Header.Get("X-Actor-Source") != "task_token" {
		return false, pgtype.UUID{}
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		return true, pgtype.UUID{}
	}
	te, err := h.Queries.GetTaskElevation(r.Context(), taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, pgtype.UUID{}
	}
	if err != nil {
		return true, pgtype.UUID{}
	}
	return te.State != elevationRestrictedFinal, te.IssueID
}

// readFilter answers "stub this body?" for one request. It looks the elevation
// up once, and only when a marked body appears.
type readFilter struct {
	h    *Handler
	r    *http.Request
	once sync.Once
	stub bool
	// taskIssue is the issue the run was elevated on; invalid when unknown.
	taskIssue pgtype.UUID
}

func (h *Handler) newReadFilter(r *http.Request) *readFilter { return &readFilter{h: h, r: r} }

func (f *readFilter) withholds(marked bool) bool {
	if !marked {
		return false
	}
	f.once.Do(func() { f.stub, f.taskIssue = f.h.elevatedReaderIssue(f.r) })
	return f.stub
}

// withholdsApproved is withholds for a body an approver can name: a marked body
// still reads in full when a person approved these exact bytes (hash of the
// current text, so an edit voids the approval) for the issue the run was
// elevated on. The issue the text sits on does not count: an approval for issue B
// must not open B's text to a run working on issue A. A lookup error or an
// unknown task issue withholds (plan 3.4).
func (f *readFilter) withholdsApproved(marked bool, workspaceID pgtype.UUID, kind, objectID, body string) bool {
	if !f.withholds(marked) {
		return false
	}
	if !f.taskIssue.Valid {
		return true
	}
	ok, err := f.h.hasReadSnapshotApproval(f.r.Context(), workspaceID, f.taskIssue, kind, objectID, sha256Hex(body))
	return err != nil || !ok
}

// withholdsComment answers "stub this comment?" for list, timeline and preview.
func (f *readFilter) withholdsComment(c db.Comment) bool {
	return f.withholdsApproved(commentWithheld(c), c.WorkspaceID, "multica_comment", uuidToString(c.ID), c.Content)
}

// withheldStub replaces a body. The hash covers the exact bytes withheld, so a
// B3 approval of that hash can name it.
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
func (h *Handler) descriptionStub(desc pgtype.Text, creatorType string, creatorID pgtype.UUID, resp IssueResponse) string {
	body := ""
	if desc.Valid {
		body = desc.String
	}
	return withheldStub("issue description", resp.ID, creatorType, uuidToString(creatorID), body, resp.WebURL)
}

// stubDescription replaces resp.Description with a stub when issue's description
// is marked and the reader is elevated. Every route that returns a description
// calls it, so no representation (list, search, write response) hands the text
// back.
func (f *readFilter) stubDescription(h *Handler, i db.Issue, resp *IssueResponse) {
	f.stubDescriptionOf(h, i.DescriptionExternalRead, i.Description, i.CreatorType, i.CreatorID, resp)
}

// stubDescriptionOf is stubDescription for list rows, which are not db.Issue.
func (f *readFilter) stubDescriptionOf(h *Handler, marked bool, desc pgtype.Text, creatorType string, creatorID pgtype.UUID, resp *IssueResponse) {
	workspaceID, wsErr := util.ParseUUID(resp.WorkspaceID)
	// An unparseable workspace id cannot be approved: the stub stays (fail closed).
	if f.withholds(marked) && (wsErr != nil || f.withholdsApproved(marked, workspaceID, "multica_description", resp.ID, desc.String)) {
		if resp.WebURL == "" {
			resp.WebURL = h.workspaceWebURL(f.r.Context(), workspaceID, func(w db.Workspace) string { return weblink.Issue(w.Slug, resp.Identifier) })
		}
		stub := h.descriptionStub(desc, creatorType, creatorID, *resp)
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

// readableAttachments drops marked attachments for an elevated reader. Bulk
// responses carry the stored URL, a CloudFront-signed download URL and a
// markdown URL, each of which fetches the bytes, so metadata alone cannot stay.
// A failed elevation lookup counts as elevated (fail closed).
func (h *Handler) readableAttachments(r *http.Request, atts []db.Attachment) []db.Attachment {
	rf := h.newReadFilter(r)
	out := make([]db.Attachment, 0, len(atts))
	for _, a := range atts {
		if !rf.withholds(a.ExternalRead) {
			out = append(out, a)
		}
	}
	return out
}
