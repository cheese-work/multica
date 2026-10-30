export type MediaSource = { type: "issues" | "chats"; id: string };
export type MediaKind = "mermaid" | "table" | "image" | "html" | "attachment";
export type MediaRoute = MediaSource & { workspaceSlug: string; kind: MediaKind; mediaId: string };

const ID = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;
const KINDS = new Set<string>(["mermaid", "table", "image", "html", "attachment"]);

export function mediaPath(route: MediaRoute): string {
  return `/${route.workspaceSlug}/media/${route.type}/${route.id}/${route.kind}/${route.mediaId}`;
}

export function parseMediaPath(value: unknown): MediaRoute | null {
  if (typeof value !== "string" || value.length > 2048) return null;
  const match = /^\/([a-z0-9][a-z0-9-]{0,62})\/media\/(issues|chats)\/([^/]+)\/([^/]+)\/([^/?#]+)$/.exec(value);
  if (!match) return null;
  const [, workspaceSlug, type, id, kind, mediaId] = match;
  if (!workspaceSlug || !id || !mediaId || !ID.test(id) || !ID.test(mediaId) || !kind || !KINDS.has(kind)) return null;
  if (type !== "issues" && type !== "chats") return null;
  return { workspaceSlug, type, id, kind: kind as MediaKind, mediaId };
}
