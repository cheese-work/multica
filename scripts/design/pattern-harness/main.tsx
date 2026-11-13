import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// Not "@multica/core/i18n/react": its barrel pulls the auth store, which hangs headless Chrome at module init.
import { I18nProvider } from "../../../packages/core/i18n/provider";
import { RESOURCES } from "@multica/views/locales";
import { issueListOptions } from "@multica/core/issues/queries";
import { STATUS_ORDER } from "@multica/core/issues/config";
import { IconButton } from "@multica/ui/components/common/icon-button";
import { TooltipProvider } from "@multica/ui/components/ui/tooltip";
import { PropertyPicker } from "../../../packages/views/issues/components/pickers/property-picker";
import { IssueChip } from "../../../packages/views/issues/components/issue-chip";
import { Archive, Copy, Download, Maximize2, RotateCcw, Trash2 } from "@multica/ui/icons";
import "./globals.css";

const WS = "ws-harness";
const issue = (id: string, number: number, status: string, title: string) => ({
  id, workspace_id: WS, number, identifier: `CHE-${number}`, title, description: null, status,
  priority: "medium", assignee_type: null, assignee_id: null, creator_type: "member", creator_id: "u1",
  parent_issue_id: null, created_at: "2026-11-13T09:00:00Z", updated_at: "2026-11-13T09:00:00Z",
});
const ISSUES = [
  issue("i1", 5428, "in_progress", "Add the doop Patterns page and X99 capture script"),
  issue("i2", 5420, "agent_review", "Design system v2: pattern ledger, doop Patterns page and CI gates"),
  issue("i3", 5482, "blocked", "Doop Patterns page, 3 frames and patterns.yml links"),
  issue("i4", 5303, "done", "Wakeup rules"),
  issue("i5", 5001, "todo", "Short"),
];

const qc = new QueryClient({ defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } } });
// The chip reads the issue list cache; seed one bucket (flattenIssueBuckets walks every category).
qc.setQueryData(issueListOptions(WS).queryKey, {
  byStatus: { [STATUS_ORDER[0]!]: { issues: ISSUES, total: ISSUES.length } },
});

const Row = ({ label, children }: { label: string; children: React.ReactNode }) => (
  <div className="flex items-center gap-4 py-2">
    <span className="w-44 shrink-0 text-caption text-muted-foreground">{label}</span>
    <div className="flex items-center gap-2">{children}</div>
  </div>
);

function IconButtonAction() {
  return (
    <div className="p-6">
      <Row label="default, ghost icon-sm">
        <IconButton label="Copy link"><Copy /></IconButton>
        <IconButton label="Open full view"><Maximize2 /></IconButton>
        <IconButton label="Restore from archive"><RotateCcw /></IconButton>
      </Row>
      <Row label="outline">
        <IconButton label="Download" variant="outline"><Download /></IconButton>
        <IconButton label="Archive" variant="outline"><Archive /></IconButton>
      </Row>
      <Row label="destructive">
        <IconButton label="Delete" variant="destructive"><Trash2 /></IconButton>
      </Row>
      <Row label="disabled (native)"><IconButton label="Download" disabled><Download /></IconButton></Row>
      <Row label="disabled with reason (aria-disabled)">
        <IconButton label="Restore" disabled disabledReason="Archived over 30 days ago"><RotateCcw /></IconButton>
      </Row>
    </div>
  );
}

const PROJECTS = ["No project", "Cheese Work Setup", "Multica fork", "doop.design", "Android app"];
function PropertyPickerDemo() {
  return (
    <div className="p-6 min-h-[420px]">
      <PropertyPicker
        open
        onOpenChange={() => {}}
        searchable
        align="start"
        side="bottom"
        width="w-56"
        trigger={<span className="text-body">Cheese Work Setup</span>}
      >
        {PROJECTS.map((p, i) => (
          <button key={p} type="button" data-picker-item {...(i === 0 ? { "data-picker-empty": "" } : {})}
            className="flex w-full items-center gap-2 rounded-xs px-2 py-1.5 text-left text-body hover:bg-accent">
            {p}
          </button>
        ))}
      </PropertyPicker>
    </div>
  );
}

function IssueMentionChip() {
  return (
    <div className="p-6 max-w-[620px]">
      <p className="text-body leading-8">
        The capture script landed in <IssueChip issueId="i1" /> under the plan <IssueChip issueId="i2" />, then the doop
        half moved to <IssueChip issueId="i3" /> after <IssueChip issueId="i4" /> closed. Short title: <IssueChip issueId="i5" />.
      </p>
      <div className="mt-4 w-[240px] rounded-md border p-2 text-body">
        Narrow parent (240px), long title truncates:
        <div className="mt-1"><IssueChip issueId="i2" /></div>
      </div>
      <p className="mt-4 text-body">Unresolved issue falls back: <IssueChip issueId="deleted-issue-id" fallbackLabel="CHE-0000" /></p>
    </div>
  );
}

const PATTERNS: Record<string, () => React.JSX.Element> = {
  "icon-button-action": IconButtonAction,
  "property-picker": PropertyPickerDemo,
  "issue-mention-chip": IssueMentionChip,
};
const id = new URLSearchParams(location.search).get("pattern") ?? "";
const View = PATTERNS[id];

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={{ en: RESOURCES.en }}>
        <TooltipProvider>{View ? <View /> : <p className="p-6">unknown pattern: {id}</p>}</TooltipProvider>
      </I18nProvider>
    </QueryClientProvider>
  </StrictMode>,
);
