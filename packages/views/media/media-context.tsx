"use client";

import { createContext, use, useEffect, useMemo, useState, type ReactNode } from "react";
import { Check, ExternalLink, Link, MoreHorizontal } from "lucide-react";
import { toast } from "sonner";
import { mediaPath, useWorkspaceSlug, type MediaKind, type MediaSource } from "@multica/core/paths";
import { copyText } from "@multica/ui/lib/clipboard";
import { useIsMobile } from "@multica/ui/hooks/use-mobile";
import { useOptionalNavigation } from "../navigation/context";
import { useT } from "../i18n";
import { mediaContentId } from "./media-id";

const SourceContext = createContext<MediaSource | null>(null);
const StandaloneContext = createContext(false);
const PersistedEditorMediaContext = createContext<string | undefined>(undefined);

export function MediaSourceProvider({ source, children }: { source: MediaSource | null; children: ReactNode }) {
  const type = source?.type;
  const id = source?.id;
  const value = useMemo(() => type && id ? { type, id } : null, [type, id]);
  return <SourceContext.Provider value={value}>{children}</SourceContext.Provider>;
}

export const StandaloneMediaProvider = StandaloneContext.Provider;
export const PersistedEditorMediaProvider = PersistedEditorMediaContext.Provider;
export function useStandaloneMedia() { return use(StandaloneContext); }
export function useMediaSource() { return use(SourceContext); }
export function usePersistedEditorMedia() { return use(PersistedEditorMediaContext); }

export function MediaLinkAction({ kind, content, attachmentId, source: explicitSource, copyLink }: {
  kind: MediaKind;
  content: string;
  attachmentId?: string;
  source?: MediaSource | null;
  copyLink?: boolean;
}) {
  const source = use(SourceContext);
  const { t } = useT("editor");
  const standaloneContext = useStandaloneMedia();
  const standalone = copyLink ?? standaloneContext;
  const slug = useWorkspaceSlug();
  const navigation = useOptionalNavigation();
  const [identity, setIdentity] = useState<{ content: string; id: string } | null>(null);
  const [copied, setCopied] = useState(false);
  const scope = explicitSource ?? source;
  const scopeType = scope?.type;
  const scopeId = scope?.id;
  const fingerprint = attachmentId ?? content;
  useEffect(() => {
    let active = true;
    if (scopeType && scopeId) {
      void mediaContentId(fingerprint).then((id) => { if (active) setIdentity({ content: fingerprint, id }); }).catch(() => { if (active) setIdentity(null); });
    }
    return () => { active = false; };
  }, [fingerprint, scopeType, scopeId]);
  if (!standalone && (!scope || !slug)) return null;
  const mediaId = identity?.content === fingerprint ? identity.id : null;
  const path = scope && slug && mediaId ? mediaPath({ ...scope, workspaceSlug: slug, kind, mediaId }) : null;
  const label = standalone ? (copied ? t(($) => $.image.link_copied) : t(($) => $.image.copy_link)) : navigation?.openMediaWindow ? t(($) => $.media.open_in_new_window) : t(($) => $.attachment.open_in_new_tab);
  const activate = async () => {
    if (standalone) {
      const current = navigation ? `${navigation.pathname}${navigation.searchParams.size ? `?${navigation.searchParams}` : ""}${navigation.hash}` : `${window.location.pathname}${window.location.search}${window.location.hash}`;
      const url = navigation?.getShareableUrl(current) ?? `${window.location.origin}${current}`;
      const succeeded = await copyText(url);
      setCopied(succeeded);
      if (!succeeded) toast.error(t(($) => $.image.copy_link_failed));
    } else if (path) {
      if (navigation?.openMediaWindow) {
        const titles: Record<MediaKind, string> = {
          mermaid: t(($) => $.mermaid.diagram_label),
          table: t(($) => $.table.expanded_title),
          html: t(($) => $.code_block.html_preview),
          image: t(($) => $.image.view),
          attachment: t(($) => $.media.title),
        };
        await navigation.openMediaWindow(path, titles[kind]);
      }
      else window.open(navigation?.getShareableUrl(path) ?? `${window.location.origin}${path}`, "_blank", "noopener,noreferrer");
    }
  };
  return <button type="button" aria-label={label} title={label} disabled={!standalone && !path} onClick={() => { void activate().catch(() => toast.error(t(($) => $.media.open_failed))); }} data-testid="media-link-action" className="flex size-8 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-secondary hover:text-foreground disabled:opacity-40">{copied ? <Check className="size-4" /> : standalone ? <Link className="size-4" /> : <ExternalLink className="size-4" />}</button>;
}

export function ViewerActions({ children }: { children: ReactNode }) {
  const { t } = useT("editor");
  const mobile = useIsMobile();
  if (!mobile) return <div className="ml-auto flex items-center gap-0.5">{children}</div>;
  return <details className="group relative ml-auto" onKeyDownCapture={(event) => {
    if (event.key === "Escape" && event.currentTarget.open) {
      event.currentTarget.open = false;
      event.currentTarget.querySelector("summary")?.focus();
      event.preventDefault();
      event.stopPropagation();
    }
  }}>
    <summary aria-label={t(($) => $.media.more)} className="flex size-10 cursor-pointer list-none items-center justify-center rounded-md hover:bg-secondary"><MoreHorizontal className="size-5" /></summary>
    <div className="absolute right-0 top-full z-50 hidden w-max max-w-[calc(100vw-2rem)] flex-wrap gap-1 rounded-md border border-border bg-background p-2 shadow-lg group-open:flex [&_button]:min-h-10 [&_button]:min-w-10">{children}</div>
  </details>;
}
