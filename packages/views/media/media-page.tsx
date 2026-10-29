"use client";

import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { useAuthStore } from "@multica/core/auth";
import type { MediaRoute } from "@multica/core/paths";
import { buttonVariants } from "@multica/ui/components/ui/button";
import { AppLink } from "../navigation";
import { AttachmentSourceStandalone } from "../editor/attachment-preview-modal";
import { MermaidDiagram } from "../editor/mermaid-diagram";
import { HtmlBlockPreview } from "../editor/html-block-preview";
import { RichContent } from "../rich-content";
import { MediaSourceProvider, StandaloneMediaProvider } from "./media-context";
import { resolveMedia, type ResolvedMedia } from "./resolve-media";
import { useT } from "../i18n";

export function MediaUnavailablePage() {
  const { t } = useT("common");
  return (
    <main className="flex min-h-screen w-full flex-col items-center justify-center gap-6 px-6 py-24 text-center">
      <p className="text-body font-medium text-muted-foreground">404</p>
      <h1 className="text-display-sm font-semibold tracking-tight">
        {t(($) => $.not_found.title)}
      </h1>
      <p className="max-w-md text-body text-muted-foreground">
        {t(($) => $.not_found.description)}
      </p>
      <AppLink href="/" className={buttonVariants({ className: "mt-2" })}>
        {t(($) => $.not_found.back_to_multica)}
      </AppLink>
    </main>
  );
}

export function MediaPage({ route }: { route: MediaRoute }) {
  const { t } = useT("editor");
  const workspaceId = useWorkspaceId();
  const userId = useAuthStore((state) => state.user?.id);
  const lastAuthorized = useRef<ResolvedMedia | null>(null);
  const query = useQuery({
    queryKey: ["media-page", workspaceId, userId, route.type, route.id, route.kind, route.mediaId],
    queryFn: () => resolveMedia(route, workspaceId),
    staleTime: 0,
    gcTime: 0,
    refetchOnMount: "always",
    retry: false,
    networkMode: "always",
  });
  const status = query.error && "status" in query.error ? Number(query.error.status) : null;
  const denied = status === 401 || status === 403 || status === 404;
  const offline = query.error instanceof TypeError || (typeof navigator !== "undefined" && !navigator.onLine);
  const media = query.isFetchedAfterMount && !denied && (query.isSuccess || (offline && lastAuthorized.current === query.data)) ? query.data : undefined;
  useEffect(() => {
    if (query.isSuccess && query.isFetchedAfterMount) lastAuthorized.current = query.data;
    if (denied) lastAuthorized.current = null;
  }, [query.isSuccess, query.isFetchedAfterMount, query.data, denied]);
  useEffect(() => { document.title = media ? t(($) => $.media.title) : "Multica"; }, [media, t]);

  if (denied) return <MediaUnavailablePage />;
  if (!media) return <main className="flex h-full flex-col items-center justify-center gap-3 bg-background text-foreground" data-testid="media-page-status">
    <h1>{query.isPending || (!query.isFetchedAfterMount && query.isFetching) ? t(($) => $.media.loading) : t(($) => $.media.load_failed)}</h1>
    {!query.isPending && !query.isFetching && <button type="button" onClick={() => void query.refetch()}>{t(($) => $.media.retry)}</button>}
  </main>;

  return <MediaSourceProvider source={{ type: route.type, id: route.id }}><StandaloneMediaProvider value={true}>
    <main className="flex h-full min-h-0 w-full flex-col overflow-hidden bg-background text-foreground" data-testid="media-page">
      {query.isError && <p role="status" className="shrink-0 bg-muted px-4 py-2 text-body">{t(($) => $.media.offline)} <button type="button" onClick={() => void query.refetch()}>{t(($) => $.media.retry)}</button></p>}
      {media.kind === "attachment" || media.kind === "image" ? <AttachmentSourceStandalone source={media.source} /> : media.kind === "mermaid" ? <MermaidDiagram chart={media.content} /> : media.kind === "html" ? <HtmlBlockPreview html={media.content} /> : <RichContent content={media.content} className="flex min-h-0 flex-1 flex-col" />}
    </main>
  </StandaloneMediaProvider></MediaSourceProvider>;
}
