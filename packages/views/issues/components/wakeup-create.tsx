"use client";

import { useId, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Info, Plus } from "@multica/ui/icons";
import { issueDetailOptions, useCreateIssueWakeup } from "@multica/core/issues";
import { shortcutMatchesEvent, useShortcut } from "@multica/core/shortcuts";
import { Button } from "@multica/ui/components/ui/button";
import { IconButton } from "@multica/ui/components/common/icon-button";
import { Popover, PopoverContent, PopoverTitle, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { DialogTitle } from "@multica/ui/components/ui/dialog";
import { ShortcutKeycaps } from "../../common/shortcut-keycaps";
import { useT } from "../../i18n";
import { WakeupFields } from "./wakeup-fields";
import { useWakeupForm } from "./use-wakeup-form";

/**
 * Lets a member add a wakeup from the issue sidebar. It creates the same rule
 * an agent creates with `multica issue wakeup create`.
 */
export function WakeupCreate({
  workspaceId,
  issueId,
  defaultAgentId,
}: {
  workspaceId: string;
  issueId: string;
  defaultAgentId?: string;
}) {
  const { t } = useT("issues");
  const [open, setOpen] = useState(false);
  const busy = useRef(false);
  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        if (!busy.current) setOpen(next);
      }}
    >
      <PopoverTrigger
        render={
          <IconButton
            variant="ghost"
            size="icon-xs"
            label={t(($) => $.wakeups.create.open)}
          />
        }
      >
        <Plus aria-hidden="true" />
      </PopoverTrigger>
      <PopoverContent
        side="left"
        align="start"
        sideOffset={12}
        className="w-[420px] max-w-[calc(100vw-2rem)] gap-0 p-4"
      >
        {open && (
          <WakeupCreateForm
            workspaceId={workspaceId}
            issueId={issueId}
            defaultAgentId={defaultAgentId ?? ""}
            onBusy={(value) => (busy.current = value)}
            onClose={() => setOpen(false)}
          />
        )}
      </PopoverContent>
    </Popover>
  );
}

export function WakeupCreateForm({
  workspaceId,
  issueId,
  defaultAgentId,
  onBusy,
  onClose,
  inDialog = false,
}: {
  workspaceId: string;
  issueId: string;
  defaultAgentId: string;
  onBusy?: (busy: boolean) => void;
  onClose: () => void;
  /** Rendered in a dialog (the workspace list) rather than the sidebar popover. */
  inDialog?: boolean;
}) {
  const Title = inDialog ? DialogTitle : PopoverTitle;
  const { t } = useT("issues");
  const id = useId();
  const create = useCreateIssueWakeup(workspaceId, issueId);
  const sendShortcut = useShortcut("send");
  const { data: issue } = useQuery(issueDetailOptions(workspaceId, issueId));
  const { draft, update, error, pending, submit } = useWakeupForm({
    workspaceId,
    defaultAgentId,
    draftKey: issueId,
    save: create.mutateAsync,
    onSaved: onClose,
    onBusy,
  });

  return (
    <form
      aria-labelledby={`${id}-title`}
      onSubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
      onKeyDown={(event) => {
        if (sendShortcut && shortcutMatchesEvent(sendShortcut, event.nativeEvent) && !event.nativeEvent.isComposing) {
          event.preventDefault();
          void submit();
        }
      }}
    >
      <Title id={`${id}-title`} className="text-title-sm font-semibold">
        {t(($) => $.wakeups.create.title)}
      </Title>
      <WakeupFields
        draft={draft}
        update={update}
        scope={{ workspaceId, issueId }}
        assigneeAgentId={issue?.assignee_type === "agent" ? (issue.assignee_id ?? null) : null}
        onBusy={onBusy}
      />
      {error && (
        <p role="alert" className="mt-3 text-caption text-destructive">
          {error}
        </p>
      )}
      <div className="mt-4 flex items-center gap-2 border-t border-border pt-3">
        <span className="flex min-w-0 flex-1 items-center gap-1 text-caption text-muted-foreground">
          <Info className="size-3 shrink-0" aria-hidden="true" />
          {t(($) => $.wakeups.create.on_behalf)}
        </span>
        <Button type="button" variant="ghost" size="sm" disabled={pending} onClick={onClose}>
          {t(($) => $.wakeups.create.cancel)}
        </Button>
        <Button type="submit" size="sm" disabled={pending} aria-busy={pending}>
          {t(($) => (pending ? $.wakeups.create.submitting : $.wakeups.create.submit))}
          {sendShortcut && !pending ? (
            <ShortcutKeycaps
              shortcut={sendShortcut}
              decorative
              className="ml-1 max-sm:hidden"
              keyClassName="border-background/30 bg-background/15 text-primary-foreground shadow-none"
            />
          ) : null}
        </Button>
      </div>
    </form>
  );
}
