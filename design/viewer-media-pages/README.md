## Request

Cheese's decision D1 = B of 26 October 2026 (DM, recorded in the description of CHE-2123): "Chỉ render mỗi cái media đấy. Không có issue comment bên dưới" (render only that media, no issue comments below). Source comment: `01a1774a-e824-7423-b1f6-8f9a1eb9c723`. Also: media that the viewer has no read permission for must not be viewable by a user or an agent.

- Issue: [CHE-2127](mention://issue/01a17753-edd9-7214-8409-4ad071712cc0), design prerequisite for [CHE-2123](mention://issue/01a1774a-665c-79ea-bfae-a20ffef9efcf)
- Pages in scope: the media viewer dialog toolbar (new "Open in new tab" action) and one new standalone page per media item. Existing precedent: `packages/views/attachments/attachment-preview-page.tsx` is the standalone page for attachments; this topic covers the four kinds the dialog shows for comment media.
- Media kinds, all four preserved: mermaid diagram (`mermaid-viewer.tsx`), table (`rich-table-dialog.tsx`), image and HTML (`attachment-preview-modal.tsx`).
- Surface: web opens a browser tab; the desktop app opens a new app window, not the external browser.
- Content: the page shows only that media item with the viewer's toolbar. No issue, no comment thread, no app chrome below it.
- Access: the page enforces the same read permission as the source issue, comment or chat. No access gives the standard not-found page and never the media bytes.
- Out of scope: media kinds Multica does not render today; the route shape (the implementer chooses it and records it in the PR; the mockups show `/issues/<key>/media/<comment-id>/<n>` for context only); the diff-view-between-revisions request in the same DM (separate topic).
- Tokens: `packages/ui/styles/tokens.css` (dark values, because the viewer is dark), copied into each mockup.
- Status: selected — approved when PR #403 merges (rev-1, direction A)
