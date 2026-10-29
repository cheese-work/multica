"use client";

import { useEffect, useState } from "react";
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
import { useActorName } from "@multica/core/workspace/hooks";
import { ActorAvatar } from "../../common/actor-avatar";
import { RichContent } from "../../rich-content";
import { useT } from "../../i18n";
import { AnnotatableAnswer } from "./annotatable-answer";
import { useChatAnnotationScope } from "./chat-annotation-context";

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
  const { getActorName } = useActorName();
  const [open, setOpen] = useState(false);
  const agentName = getActorName("agent", session.agent_id);
  const title = session.title.trim();
  const last = session.last_message;
  const label = title
    ? `${t(($) => $.message_list.earlier_dm)} · ${title}`
    : t(($) => $.message_list.earlier_dm);
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="flex items-center gap-1 text-caption text-muted-foreground hover:text-foreground transition-colors">
        {open ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
        {label}
      </CollapsibleTrigger>
      {!open && last && (
        <p data-testid="predecessor-preview" data-speaker={last.role} className="truncate pl-4 text-caption text-faint-foreground">
          {last.role === "user" ? t(($) => $.message_list.speaker_you) : agentName}: {last.content}
        </p>
      )}
      <CollapsibleContent>
        {open && <PredecessorMessages session={session} agentName={agentName} onQuoteMessage={onQuoteMessage} />}
      </CollapsibleContent>
    </Collapsible>
  );
}

function PredecessorMessages({
  session,
  agentName,
  onQuoteMessage,
}: {
  session: ChatSession;
  agentName: string;
  onQuoteMessage?: (content: string) => void;
}) {
  const { t } = useT("chat");
  const { data, isLoading, hasNextPage, isFetchingNextPage, fetchNextPage } = useInfiniteQuery(
    chatMessagesPageOptions(session.id),
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
        <PredecessorMessage key={message.id} message={message} agentId={session.agent_id} agentName={agentName} onQuoteMessage={onQuoteMessage} />
      ))}
    </div>
  );
}

/** One earlier message, drawn like the live DM: the person in a bubble, the agent with avatar and name. */
function PredecessorMessage({
  message,
  agentId,
  agentName,
  onQuoteMessage,
}: {
  message: ChatMessage;
  agentId: string;
  agentName: string;
  onQuoteMessage?: (content: string) => void;
}) {
  const { t } = useT("chat");
  const { registerSource } = useChatAnnotationScope();
  const isUser = message.role === "user";
  const quotable = isQuotable(message);
  useEffect(() => {
    if (!isUser && quotable) registerSource?.(message.id, agentName);
  }, [isUser, quotable, registerSource, message.id, agentName]);
  const body = (
    <RichContent content={message.content} attachments={message.attachments} density="compact" phase="settled" />
  );
  return (
    <div
      data-predecessor-message={message.id}
      data-speaker={message.role}
      className="group/quote flex items-start gap-1"
    >
      {isUser ? (
        <div className="flex min-w-0 flex-1 flex-col items-end">
          <div data-testid="chat-user-message" className="max-w-[80%] break-words rounded-2xl bg-muted px-3.5 py-2 text-body">
            {body}
          </div>
        </div>
      ) : (
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex items-center gap-1.5 text-caption text-muted-foreground">
            <ActorAvatar actorType="agent" actorId={agentId} size="xs" />
            <span>{agentName}</span>
          </div>
          <AnnotatableAnswer messageId={quotable ? message.id : undefined}>
            <div className="text-muted-foreground">{body}</div>
          </AnnotatableAnswer>
        </div>
      )}
      {onQuoteMessage && quotable && (
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
  );
}
