"use client";

// N-mode registry for the sidebar card and its dialog (CHE-4312 D5, premise 5). A mode is one data entry; the switch,
// the card and the dialog read this list and never name a mode. Gantt joins later with one entry.
import type { ComponentType, RefObject } from "react";
import { useIssueViewStore } from "@multica/core/issues/stores/view-store";
import type { Issue } from "@multica/core/types";
import { Network, Rows3 } from "@multica/ui/icons";
import { useT } from "../../../i18n";
import { Swimlane2IssueTree } from "../swimlane2/swimlane2-issue-tree";
import { HierarchyCard, HierarchyDialogBody } from "./hierarchy-mode";

type T = ReturnType<typeof useT<"issues">>["t"];

export interface IssueMode {
  /** Persisted as-is in the view store. */
  id: string;
  label: (t: T) => string;
  icon: ComponentType<{ className?: string }>;
  Card: ComponentType<{ issue: Issue; onOpenCanvas: () => void }>;
  Dialog: ComponentType<{ issue: Issue; viewportRef: RefObject<HTMLDivElement | null>; onOpen: (id: string) => void }>;
}

export const ISSUE_MODES: IssueMode[] = [
  { id: "hierarchy", label: (t) => t(($) => $.hierarchy.title), icon: Network, Card: HierarchyCard, Dialog: HierarchyDialogBody },
  {
    id: "swimlane2",
    label: (t) => t(($) => $.view.swimlane2),
    icon: Rows3,
    Card: ({ issue }) => <div className="max-h-72 overflow-auto"><Swimlane2IssueTree issue={issue} /></div>,
    Dialog: ({ issue }) => <div className="min-h-0 flex-1 overflow-auto rounded-xl border border-surface-border"><Swimlane2IssueTree issue={issue} /></div>,
  },
];

/** The persisted mode, or the first mode when its id is unknown (a removed mode). */
export function useIssueMode(modes: readonly IssueMode[]) {
  const id = useIssueViewStore((s) => s.sidebarMode);
  const setId = useIssueViewStore((s) => s.setSidebarMode);
  return { mode: modes.find((m) => m.id === id) ?? modes[0]!, setMode: setId };
}

/** Segmented switch. `flex-wrap` lets N entries wrap; ponytail: a dropdown past 4 modes when a fifth is added. */
export function ModeSwitch({ modes, activeId, onChange }: { modes: readonly IssueMode[]; activeId: string; onChange: (id: string) => void }) {
  const { t } = useT("issues");
  return (
    <div role="group" aria-label={t(($) => $.view.section)} className="flex flex-wrap items-center gap-1">
      {modes.map(({ id, label, icon: Icon }) => (
        <button
          key={id}
          type="button"
          aria-pressed={id === activeId}
          onClick={() => onChange(id)}
          className="inline-flex h-7 items-center gap-1 rounded-full border border-surface-border px-2 text-caption font-medium outline-none focus-visible:ring-2 focus-visible:ring-ring aria-pressed:border-info aria-pressed:bg-info/10"
        >
          <Icon className="size-3.5" />{label(t)}
        </button>
      ))}
    </div>
  );
}
