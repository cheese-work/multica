"use client";

import { useEffect, useId, useRef, useState } from "react";
import type { Issue } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Maximize2 } from "@multica/ui/icons";
import { useT } from "../../../i18n";
import { HierarchyDialog } from "./hierarchy-dialog";
import { ISSUE_MODES, ModeSwitch, useIssueMode, type IssueMode } from "./issue-modes";

/**
 * Set by the dialog when a card opens another issue: the collapsed minimap of
 * the target takes focus on mount. Module-level because the target's minimap
 * may be a new instance by then (D4).
 */
let focusHeadingOnMount = false;
export const requestHierarchyFocus = () => { focusHeadingOnMount = true; };

export function IssueHierarchy({ issue, modes = ISSUE_MODES }: { issue: Issue; modes?: readonly IssueMode[] }) {
  const { t } = useT("issues");
  const headingId = useId();
  const headingRef = useRef<HTMLHeadingElement>(null);
  const [open, setOpen] = useState(false);
  const { mode, setMode } = useIssueMode(modes);

  useEffect(() => {
    if (!focusHeadingOnMount) return;
    focusHeadingOnMount = false;
    headingRef.current?.focus();
  }, [issue.id]);

  return (
    <section aria-labelledby={headingId} data-testid="issue-hierarchy" className="overflow-hidden rounded-xl border border-surface-border bg-card">
      <div className="flex items-center justify-between border-b border-surface-border py-1.5 pl-3 pr-2">
        <h3 id={headingId} ref={headingRef} tabIndex={-1} className="text-label font-bold outline-none">{t(($) => $.hierarchy.title)}</h3>
        <Button type="button" variant="outline" size="icon-sm" aria-label={t(($) => $.hierarchy.expand)} onClick={() => setOpen(true)}>
          <Maximize2 className="size-3.5" />
        </Button>
      </div>
      <div className="border-b border-surface-border px-3 py-1.5"><ModeSwitch modes={modes} activeId={mode.id} onChange={setMode} /></div>
      <mode.Card issue={issue} onOpenCanvas={() => setOpen(true)} />
      {open && <HierarchyDialog issue={issue} headingRef={headingRef} modes={modes} onClose={() => setOpen(false)} />}
    </section>
  );
}
