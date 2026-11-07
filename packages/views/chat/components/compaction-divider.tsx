import { useState } from "react";
import { ChevronDown, ChevronRight } from "@multica/ui/icons";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@multica/ui/components/ui/collapsible";
import type { ChatMessage } from "@multica/core/types";
import { useT } from "../../i18n";
import { RichContent } from "../../rich-content";
import { AnnotatableAnswer } from "./annotatable-answer";
import { formatCompactionTime } from "./message-time";

/**
 * Where an in-place compaction happened (CHE-2169). Older messages stay above
 * it; the summary the agent wrote is one click away.
 */
export function CompactionDivider({ message }: { message: ChatMessage }) {
  const { t } = useT("chat");
  const [open, setOpen] = useState(false);
  const time = formatCompactionTime(message.created_at);
  const label = time
    ? t(($) => $.message_list.compaction_divider, { time })
    : t(($) => $.message_list.compaction_divider_untimed);
  return (
    <Collapsible open={open} onOpenChange={setOpen} data-testid="compaction-divider">
      <div className="flex items-center gap-3">
        <div aria-hidden className="h-px flex-1 bg-border" />
        <CollapsibleTrigger className="flex items-center gap-1 text-caption text-muted-foreground hover:text-foreground transition-colors">
          {open ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
          {label}
        </CollapsibleTrigger>
        <div aria-hidden className="h-px flex-1 bg-border" />
      </div>
      <CollapsibleContent>
        <div data-testid="compaction-summary" className="mt-2">
          <AnnotatableAnswer messageId={message.id}>
            <RichContent content={message.content} attachments={message.attachments} density="compact" />
          </AnnotatableAnswer>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}
