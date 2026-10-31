"use client";

import { useState } from "react";
import { ChevronDown, ChevronRight } from "@multica/ui/icons";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@multica/ui/components/ui/collapsible";
import type { ChatMessage, ChatSession } from "@multica/core/types";
import { useWorkspacePaths } from "@multica/core/paths";
import { AppLink } from "../../navigation";
import { RichContent } from "../../rich-content";
import { useT } from "../../i18n";
import { AnnotatableAnswer } from "./annotatable-answer";
import { PredecessorDms } from "./predecessor-dms";

/**
 * The top of a rolled-over DM: one line saying where it continues from, and one
 * expand that holds the agent's summary and the earlier DMs.
 */
export function ContinuationBlock({
  continuedFromSessionId,
  summary,
  predecessors,
  onQuoteMessage,
}: {
  continuedFromSessionId?: string | null;
  summary?: ChatMessage;
  predecessors: ChatSession[];
  onQuoteMessage?: (content: string) => void;
}) {
  const { t } = useT("chat");
  const wsPaths = useWorkspacePaths();
  const [open, setOpen] = useState(false);
  const hasEarlierContext = !!summary || predecessors.length > 0;
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="py-2">
      <div className="flex items-center gap-1 text-caption text-muted-foreground">
        {hasEarlierContext && (
          <CollapsibleTrigger
            aria-label={t(($) => $.message_list.show_earlier_context)}
            className="hover:text-foreground transition-colors"
          >
            {open ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
          </CollapsibleTrigger>
        )}
        <p className="text-muted-foreground">
          {continuedFromSessionId ? (
            <>
              {t(($) => $.message_list.continued_from)}{" "}
              <AppLink
                href={`${wsPaths.chat()}?session=${encodeURIComponent(continuedFromSessionId)}`}
                className="underline underline-offset-2 hover:text-foreground"
              >
                {t(($) => $.message_list.previous_dm)}
              </AppLink>
            </>
          ) : (
            t(($) => $.message_list.continuation_summary)
          )}
        </p>
      </div>
      <CollapsibleContent className="space-y-2 pt-1">
        {summary && (
          <AnnotatableAnswer messageId={summary.id}>
            <RichContent content={summary.content} attachments={summary.attachments} density="compact" />
          </AnnotatableAnswer>
        )}
        {predecessors.length > 0 && <PredecessorDms sessions={predecessors} onQuoteMessage={onQuoteMessage} />}
      </CollapsibleContent>
    </Collapsible>
  );
}
