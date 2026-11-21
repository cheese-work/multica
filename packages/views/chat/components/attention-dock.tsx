"use client";

import { useState } from "react";
import { useIsBelowDock } from "@multica/ui/hooks/use-mobile";
import { Button } from "@multica/ui/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@multica/ui/components/ui/sheet";
import type { Agent, ChatSession } from "@multica/core/types";
import { useT } from "../../i18n";
import { NeedsYouList, attentionRowCount, hasAttention } from "./needs-you-list";

type Props = { sessions: ChatSession[]; agents: Agent[] };

/**
 * Design lock CHE-4144 (B): at 1280px and wider the Needs you queue docks
 * beside the open conversation. The thread list already runs the attention
 * refresh, so the panel does not start a second one.
 */
export function AttentionDock({ sessions, agents }: Props) {
  const { t } = useT("chat");
  const below = useIsBelowDock();
  if (below || !hasAttention(sessions)) return null;
  return (
    <aside
      aria-label={t(($) => $.needs_you.title)}
      className="hidden w-80 shrink-0 overflow-y-auto border-l px-2 min-[1280px]:block"
    >
      <NeedsYouList sessions={sessions} agents={agents} refresh={false} />
    </aside>
  );
}

/** Below 1280px the same queue folds into a drawer opened from the header. */
export function AttentionDrawer({ sessions, agents }: Props) {
  const { t } = useT("chat");
  const below = useIsBelowDock();
  const [open, setOpen] = useState(false);
  if (!below || !hasAttention(sessions)) return null;
  return (
    <>
      <Button type="button" variant="ghost" size="xs" onClick={() => setOpen(true)}>
        {t(($) => $.needs_you.title)}{" "}
        <span className="tabular-nums text-muted-foreground">{attentionRowCount(sessions)}</span>
      </Button>
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="right" className="gap-0 overflow-y-auto px-2">
          <SheetHeader className="sr-only">
            <SheetTitle>{t(($) => $.needs_you.title)}</SheetTitle>
            <SheetDescription>{t(($) => $.needs_you.title)}</SheetDescription>
          </SheetHeader>
          <NeedsYouList sessions={sessions} agents={agents} refresh={false} />
        </SheetContent>
      </Sheet>
    </>
  );
}
