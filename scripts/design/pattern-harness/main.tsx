import { StrictMode, Suspense, lazy, useEffect, useRef, useState } from "react";
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
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@multica/ui/components/ui/collapsible";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@multica/ui/components/ui/select";
import { cn } from "@multica/ui/lib/utils";
import { SettingsCard, SettingsRow, SettingsSection } from "../../../packages/views/settings/components/settings-layout";
import { PillMenu } from "../../../packages/views/issues/components/wakeup-pickers";
import {
  Archive, ChevronDown, ChevronRight, Copy, Download, Maximize2, MessageSquareQuote, RotateCcw, Trash2,
} from "@multica/ui/icons";
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


// lazy-detail: the ledger entry has no shared component yet (3 near-copies). This stand-in is the recipe:
// React.lazy + Suspense with a null fallback, so the surface shows nothing until the chunk lands.
const LazyBody = lazy(() => Promise.resolve({ default: () => (
  <div className="rounded-md border border-surface-border bg-surface p-3 text-body">
    <p className="font-medium">CHE-5527 · Capture 5 Patterns frames</p>
    <p className="mt-1 text-caption text-muted-foreground">Loaded chunk: the detail body renders in place of the null fallback.</p>
  </div>
) }));
const NeverBody = lazy(() => new Promise<{ default: () => React.JSX.Element }>(() => {}));
function LazyDetail() {
  return (
    <div className="p-6 max-w-[620px] space-y-6">
      <div>
        <p className="mb-2 text-caption text-muted-foreground">Loading: fallback is null, the slot stays empty (reserved height is the caller's job)</p>
        <div className="min-h-16 rounded-md border border-dashed border-surface-border p-2">
          <Suspense fallback={null}><NeverBody /></Suspense>
        </div>
      </div>
      <div>
        <p className="mb-2 text-caption text-muted-foreground">Loaded</p>
        <Suspense fallback={null}><LazyBody /></Suspense>
      </div>
    </div>
  );
}

// decision-receipt-card: stand-in for the shared frame behind decision-dm-card, decision-quote-card,
// decision-withdrawal-card and decision-answer-content (one recipe, copied 3 times today).
const PILL = "rounded-full bg-muted px-2 text-micro font-medium text-muted-foreground";
function ReceiptCard({ title, pill, option, quote, muted }: { title: string; pill: string; option?: string; quote?: string; muted?: boolean }) {
  return (
    <div className={cn("w-full min-w-0 max-w-[28rem] overflow-hidden rounded-xl border border-surface-border bg-surface", muted && "bg-muted text-muted-foreground")}>
      <div className="flex flex-wrap items-center gap-2 border-b bg-muted px-2.5 py-1.5 text-caption">
        <MessageSquareQuote className="size-3.5 shrink-0" aria-hidden />
        <span className="font-medium">{title}</span>
        <span className={PILL}>{pill}</span>
        {option && <span className="rounded-xs border px-1.5 text-micro font-medium">{option}</span>}
      </div>
      {quote && (
        <figure className="px-3 py-2">
          <blockquote className="border-l-2 border-surface-border pl-2.5 text-label">{quote}</blockquote>
        </figure>
      )}
    </div>
  );
}
function DecisionReceiptCard() {
  return (
    <div className="p-6 space-y-3">
      <ReceiptCard title="Decision recorded on CHE-5420" pill="Delivered" option="Option B" quote="Ship the ledger first, frames after." />
      <ReceiptCard title="Decision asked on CHE-5430" pill="Waiting" />
      <ReceiptCard title="Decision withdrawn on CHE-5311" pill="Withdrawn" muted />
    </div>
  );
}

function ContinuationDemo({ open, setOpen, label }: { open: boolean; setOpen: (v: boolean) => void; label: string }) {
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="py-2">
      <div className="flex items-center gap-1 text-caption text-muted-foreground">
        <CollapsibleTrigger aria-label="Show earlier context" className="transition-colors hover:text-foreground">
          {open ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
        </CollapsibleTrigger>
        <p>{label}</p>
      </div>
      <CollapsibleContent className="space-y-2 pt-1 text-body">
        <p>Summary of the earlier DM: the ledger gained five frames and the validator stayed green.</p>
        <p className="text-muted-foreground">Two earlier DMs are folded in below this block.</p>
      </CollapsibleContent>
    </Collapsible>
  );
}
function CollapsibleSection() {
  const [a, setA] = useState(true);
  const [b, setB] = useState(false);
  return (
    <div className="p-6 max-w-[620px] divide-y divide-surface-border">
      <ContinuationDemo open={a} setOpen={setA} label="Continued from previous DM (open)" />
      <ContinuationDemo open={b} setOpen={setB} label="Continued from previous DM (closed)" />
    </div>
  );
}

const MODES = [{ value: "off", label: "Off" }, { value: "shadow", label: "Shadow" }, { value: "act", label: "Act" }];
const TZ = [{ value: "ict", label: "Asia/Ho_Chi_Minh (UTC+07:00)" }, { value: "utc", label: "UTC" }];
function SettingsSelectRow() {
  return (
    <div className="p-6 max-w-[760px]">
      <SettingsSection title="Workspace" description="Rows pair a label with one control; the select tier sets the control width.">
        <SettingsCard>
          <SettingsRow label="Idle-issue guard" description="Moves started issues with no run and no wakeup after N hours. Shadow only logs." size="select">
            <Select items={MODES} value="shadow" onValueChange={() => {}}>
              <SelectTrigger size="sm" className="w-full" aria-label="Idle-issue guard"><SelectValue /></SelectTrigger>
              <SelectContent align="end">{MODES.map((m) => <SelectItem key={m.value} value={m.value}>{m.label}</SelectItem>)}</SelectContent>
            </Select>
          </SettingsRow>
          <SettingsRow label="Timezone" description="Long option labels use the wide tier." size="select-wide">
            <Select items={TZ} value="ict" onValueChange={() => {}}>
              <SelectTrigger size="sm" className="w-full" aria-label="Timezone"><SelectValue /></SelectTrigger>
              <SelectContent align="end">{TZ.map((m) => <SelectItem key={m.value} value={m.value}>{m.label}</SelectItem>)}</SelectContent>
            </Select>
          </SettingsRow>
        </SettingsCard>
      </SettingsSection>
    </div>
  );
}

function PillMenuDemo() {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    // Open the third pill so the capture shows the radio menu.
    const t = setTimeout(() => ref.current?.querySelector<HTMLButtonElement>("[data-open-me] button")?.click(), 300);
    return () => clearTimeout(t);
  }, []);
  const when = [{ value: "now", label: "Now" }, { value: "in", label: "In a while" }, { value: "at", label: "At a time" }];
  const kind = [{ value: "status", label: "Status changes" }, { value: "pr", label: "PR checks" }, { value: "child", label: "Children done" }];
  return (
    <div ref={ref} className="p-6 min-h-[360px] max-w-[620px] space-y-4">
      <Row label="closed, value chosen"><PillMenu label="Wake" value="now" options={when} onChange={() => {}} /></Row>
      <Row label="closed, placeholder"><PillMenu label="Condition" value="" options={kind} placeholder="Choose condition" onChange={() => {}} /></Row>
      <Row label="open (radio menu)"><span data-open-me><PillMenu label="Condition" value="pr" options={kind} onChange={() => {}} /></span></Row>
    </div>
  );
}

const PATTERNS: Record<string, () => React.JSX.Element> = {
  "icon-button-action": IconButtonAction,
  "property-picker": PropertyPickerDemo,
  "issue-mention-chip": IssueMentionChip,
  "lazy-detail": LazyDetail,
  "decision-receipt-card": DecisionReceiptCard,
  "collapsible-section": CollapsibleSection,
  "settings-select-row": SettingsSelectRow,
  "pill-menu": PillMenuDemo,
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
