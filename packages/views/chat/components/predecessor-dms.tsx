"use client";

import { useState } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronRight, Quote } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@multica/ui/components/ui/collapsible";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { chatMessagesPageOptions } from "@multica/core/chat/queries";
import type { ChatMessage, ChatSession } from "@multica/core/types";
import { RichContent } from "../../rich-content";
import { useT } from "../../i18n";

/** Rows that are chrome of the earlier DM, not something the person or agent said. */
const HIDDEN_KINDS = new Set(["continuation_summary", "compact_notice", "onboarding_kickoff", "decision_reference"]);

const isQuotable = (message: ChatMessage) =>
  !message.failure_reason && message.message_kind !== "no_response" && message.content.trim().length > 0;

/**
 * The earlier DMs of a continuation chain, each collapsed and read-only. The
 * agent never sees them; they are here so a reader can quote one into the
 * composer. Messages load when a section is first opened.
 */
export function PredecessorDms({
  sessions,
  onQuoteMessage,
}: {
  sessions: ChatSession[];
  onQuoteMessage?: (content: string) => void;
}) {
  return (
    <div className="space-y-1 py-1">
      {sessions.map((session) => (
        <PredecessorDm key={session.id} session={session} onQuoteMessage={onQuoteMessage} />
      ))}
    </div>
  );
}

function PredecessorDm({
  session,
  onQuoteMessage,
}: {
  session: ChatSession;
  onQuoteMessage?: (content: string) => void;
}) {
  const { t } = useT("chat");
  const [open, setOpen] = useState(false);
  const title = session.title.trim();
  const label = title
    ? `${t(($) => $.message_list.earlier_dm)} · ${title}`
    : t(($) => $.message_list.earlier_dm);
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="flex items-center gap-1 text-caption text-muted-foreground hover:text-foreground transition-colors">
        {open ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
        {label}
      </CollapsibleTrigger>
      <CollapsibleContent>
        {open && <PredecessorMessages sessionId={session.id} onQuoteMessage={onQuoteMessage} />}
      </CollapsibleContent>
    </Collapsible>
  );
}

function PredecessorMessages({
  sessionId,
  onQuoteMessage,
}: {
  sessionId: string;
  onQuoteMessage?: (content: string) => void;
}) {
  const { t } = useT("chat");
  const { data, isLoading, hasNextPage, isFetchingNextPage, fetchNextPage } = useInfiniteQuery(
    chatMessagesPageOptions(sessionId),
  );
  if (isLoading) return <Skeleton className="my-2 h-3.5 w-1/2" />;
  const messages = [...(data?.pages ?? [])]
    .reverse()
    .flatMap((page) => page.messages)
    .filter((m) => !HIDDEN_KINDS.has(m.message_kind ?? ""));
  return (
    <div className="my-1 space-y-3 border-l-2 border-border pl-3">
      {hasNextPage && (
        <Button variant="ghost" size="xs" disabled={isFetchingNextPage} onClick={() => void fetchNextPage()}>
          {t(($) => $.message_list.load_older_messages)}
        </Button>
      )}
      {messages.map((message) => (
        <div key={message.id} data-predecessor-message={message.id} className="group/quote flex items-start gap-1">
          <div className="min-w-0 flex-1 text-muted-foreground">
            <RichContent content={message.content} attachments={message.attachments} density="compact" phase="settled" />
          </div>
          {onQuoteMessage && isQuotable(message) && (
            <Button
              variant="ghost"
              size="icon-xs"
              className="shrink-0 text-faint-foreground hover:text-foreground"
              aria-label={t(($) => $.message_list.quote_message)}
              onClick={() => onQuoteMessage(message.content)}
            >
              <Quote />
            </Button>
          )}
        </div>
      ))}
    </div>
  );
}
