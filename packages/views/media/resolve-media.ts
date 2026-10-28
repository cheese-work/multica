import { api } from "@multica/core/api";
import type { Attachment } from "@multica/core/types";
import type { MediaRoute } from "@multica/core/paths";
import { collectAttachmentSequence } from "@multica/core/attachments/image-sequence";
import { canOpenPreview, getPreviewKind } from "../editor/utils/preview";
import type { PreviewSource } from "../editor/attachment-preview-modal";
import { mediaContentId } from "./media-id";
import { markdownMedia } from "./markdown-media";

export { markdownMedia } from "./markdown-media";

export type ResolvedMedia =
  | { kind: "mermaid"; content: string }
  | { kind: "html"; content: string }
  | { kind: "table"; content: string }
  | { kind: "attachment"; source: PreviewSource }
  | { kind: "image"; source: PreviewSource };

export function mediaNotFound(): Error & { status: number } {
  return Object.assign(new Error("Not found"), { status: 404 });
}

export async function resolveMedia(route: MediaRoute, workspaceId: string): Promise<ResolvedMedia> {
  let blocks: Array<{ content: string; attachments?: Attachment[] }>;
  if (route.type === "issues") {
    const issue = await api.getIssue(route.id);
    if (issue.workspace_id !== workspaceId) throw mediaNotFound();
    const comments = await api.listComments(issue.id);
    const attachments = await api.listAttachments(issue.id);
    blocks = [{ content: issue.description ?? "", attachments }, ...comments.map((comment) => ({ content: comment.content ?? "", attachments: comment.attachments }))];
  } else {
    const session = await api.getChatSession(route.id);
    if (session.workspace_id !== workspaceId) throw mediaNotFound();
    const messages = await api.listChatMessages(session.id);
    blocks = messages.map((message) => ({ content: message.content ?? "", attachments: message.attachments }));
  }
  if (route.kind === "attachment" || route.kind === "image") {
    const items = collectAttachmentSequence(blocks, ({ contentType, filename, hasRecord }) => canOpenPreview(getPreviewKind(contentType, filename), hasRecord));
    for (const item of items) {
      if (route.kind === "attachment" && item.attachment && await mediaContentId(item.attachment.id) === route.mediaId) {
        return { kind: "attachment", source: { kind: "full", attachment: item.attachment } };
      }
      if (route.kind === "image" && item.imageByConstruction && await mediaContentId(item.url) === route.mediaId) {
        return { kind: "image", source: item.attachment ? { kind: "full", attachment: item.attachment } : { kind: "url", url: item.url, filename: item.filename, forceKind: "image" } };
      }
    }
  } else {
    for (const block of blocks) {
      for (const item of markdownMedia(block.content)) {
        if (item.kind === route.kind && await mediaContentId(item.content) === route.mediaId) {
          if (item.kind === "mermaid") return { kind: "mermaid", content: item.content };
          if (item.kind === "html") return { kind: "html", content: item.content };
          return { kind: "table", content: item.content };
        }
      }
    }
  }
  throw mediaNotFound();
}
