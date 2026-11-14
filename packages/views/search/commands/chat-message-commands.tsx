"use client";

import { useEffect, useMemo, useState } from "react";
import { keepPreviousData, useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Button } from "@multica/ui/components/ui/button";
import { Loader2, MessageSquare } from "@multica/ui/icons";
import { chatSessionsOptions } from "@multica/core/chat/queries";
import { chatSearchFailure, chatSearchInfiniteOptions, type ChatSearchFailure } from "@multica/core/chat/search";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import type { ChatSearchHit } from "@multica/core/types";
import { useT } from "../../i18n";
import { HighlightText } from "../highlight-text";
import { usePalette } from "../palette-context";
import { ObjectRow } from "../result-rows";
import { PaletteGroup, ROW_ICON_CLASS } from "../rows";

const SEARCH_DEBOUNCE_MS = 250;
const NO_HITS: ChatSearchHit[] = [];

export type ChatMessageSearchState = "idle" | "searching" | "ready" | "error" | "offline";

export interface ChatMessageSearch {
  state: ChatMessageSearchState;
  failure: ChatSearchFailure | null;
  /** Content hits only; title hits stay in the palette's Chats group. */
  hits: ChatSearchHit[];
  retry: () => void;
}

/**
 * Message-content search for the palette. Reuses the DM list's query options,
 * so a query typed in either place shares one cache entry. An empty or
 * over-long query never reaches the server.
 */
export function useChatMessageSearch(rawQuery: string, enabled: boolean): ChatMessageSearch {
  const wsId = useWorkspaceId();
  const trimmed = rawQuery.trim();
  const [settled, setSettled] = useState(trimmed);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(trimmed), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [trimmed]);

  const options = chatSearchInfiniteOptions(wsId, enabled ? settled : "");
  // Rows of the previous query stay on screen until the next answer lands.
  const search = useInfiniteQuery({ ...options, placeholderData: keepPreviousData });

  const hits = useMemo(
    () => search.data?.pages.flatMap((page) => page.results).filter((h) => h.message_id) ?? NO_HITS,
    [search.data],
  );

  const retry = () => void search.refetch();
  if (!enabled || !options.enabled) return { state: "idle", failure: null, hits: NO_HITS, retry };
  // Offline, TanStack pauses the request: it neither runs nor fails.
  if (search.fetchStatus === "paused" && (search.isPending || search.isPlaceholderData)) {
    return { state: "offline", failure: "disconnected", hits, retry };
  }
  if (search.isError) {
    const failure = chatSearchFailure(search.error);
    return { state: failure === "disconnected" ? "offline" : "error", failure, hits: NO_HITS, retry };
  }
  if (settled !== trimmed || search.isPending || search.isPlaceholderData) {
    return { state: "searching", failure: null, hits, retry };
  }
  return { state: "ready", failure: null, hits, retry };
}

/** Message hits, opening the DM at the matched message. */
export function ChatMessageGroup({ search, query }: { search: ChatMessageSearch; query: string }) {
  const { t } = useT("search");
  const palette = usePalette();
  const p = useWorkspacePaths();
  if (search.hits.length === 0 || search.state === "offline" || search.state === "error") return null;
  return (
    <PaletteGroup heading={t(($) => $.groups.messages)}>
      {search.hits.map((hit) => (
        <ObjectRow
          key={`${hit.session_id}:${hit.message_id}`}
          value={`message:${hit.session_id}:${hit.message_id}`}
          icon={<MessageSquare className={ROW_ICON_CLASS} />}
          title={hit.title.trim() || t(($) => $.messages.untitled)}
          query=""
          detail={<HighlightText text={hit.snippet} query={query} />}
          onSelect={() => palette.navigate(`${p.chat()}?session=${hit.session_id}&message=${hit.message_id}`)}
        />
      ))}
    </PaletteGroup>
  );
}

/** The searching, error and offline states of the message search. */
export function ChatMessageStatus({ search }: { search: ChatMessageSearch }) {
  const { t } = useT("search");
  if (search.state === "searching") {
    return (
      <p role="status" className="flex items-center gap-2 px-4 py-2 text-caption text-muted-foreground">
        <Loader2 className="size-3.5 animate-spin motion-reduce:animate-none" />
        {t(($) => $.messages.searching)}
      </p>
    );
  }
  if (search.state === "offline") return <OfflineStatus />;
  if (search.state !== "error") return null;
  return (
    <div role="alert" className="flex flex-col items-start gap-1.5 px-4 py-2 text-caption text-muted-foreground">
      <p className="font-medium text-foreground">{t(($) => $.messages.error_title)}</p>
      <p>{search.failure === "timed_out" ? t(($) => $.messages.error_timed_out) : t(($) => $.messages.error_failed)}</p>
      {/* The design lock draws a 2px ring on Retry; Button's default is 3px. */}
      <Button variant="ghost" size="xs" onClick={search.retry} className="focus-visible:ring-2">
        {t(($) => $.messages.retry)}
      </Button>
    </div>
  );
}

function OfflineStatus() {
  const { t } = useT("search");
  const wsId = useWorkspaceId();
  const { data: sessions } = useQuery(chatSessionsOptions(wsId));
  const count = sessions?.filter((s) => s.status !== "archived").length ?? 0;
  return (
    <div role="status" className="flex flex-col gap-0.5 px-4 py-2 text-caption text-muted-foreground">
      <p className="font-medium text-foreground">{t(($) => $.messages.offline_title)}</p>
      <p>{t(($) => $.messages.offline_body, { count })}</p>
    </div>
  );
}
