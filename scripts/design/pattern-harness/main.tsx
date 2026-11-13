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

// Raw-class views: these ledger entries list no shared component yet (build: "one shared ..."),
// so each view renders the classes the fork uses today, copied from the cited files.
function SectionEyebrow() {
  return (
    <div className="p-6 max-w-[560px] space-y-6">
      <section>
        <p className="text-micro uppercase tracking-wide text-muted-foreground">Runtime</p>
        <p className="mt-1 text-body">x99-claude-sonnet-design</p>
      </section>
      <section>
        <dl className="grid grid-cols-3 gap-4">
          {[["Online", "12"], ["Idle", "3"], ["Offline", "1"]].map(([k, v]) => (
            <div key={k}>
              <dt className="truncate text-micro uppercase tracking-wider text-muted-foreground">{k}</dt>
              <dd className="text-title font-semibold">{v}</dd>
            </div>
          ))}
        </dl>
      </section>
      <section className="rounded-md border">
        <div className="grid grid-cols-[1.6fr_1fr_1fr] gap-3 border-b bg-muted/40 px-3 py-2 text-micro uppercase tracking-wide text-muted-foreground">
          <span>Name</span><span>Status</span><span>Last seen</span>
        </div>
        <div className="grid grid-cols-[1.6fr_1fr_1fr] gap-3 px-3 py-2 text-body">
          <span>c00-runtime</span><span>Online</span><span>2 min ago</span>
        </div>
      </section>
      <section>
        <p className="text-micro uppercase tracking-wide text-warning">Needs attention</p>
        <p className="mt-1 text-body">Status colour variants: warning here, emerald in the MCP tab.</p>
      </section>
    </div>
  );
}

const ROLES: [string, string, string][] = [
  ["text-micro", "11/15", "counters, badges, timestamps, overline labels"],
  ["text-caption", "12/16", "chips, shortcut hints, tooltips, helper text"],
  ["text-label", "13/18", "read in place: activity, property rows, table headers"],
  ["text-body", "14/20", "UI body, the workhorse"],
  ["text-body-lg", "15/22", "dialog copy, onboarding prose"],
  ["text-title-sm", "16/24", "small headings"],
  ["text-title", "18/28", "section headings"],
  ["text-title-lg", "20/28", "card and dialog titles"],
];
function TypeRoleTokens() {
  return (
    <div className="p-6 max-w-[720px]">
      {ROLES.map(([cls, px, role]) => (
        <div key={cls} className="flex items-baseline gap-4 border-b py-2">
          <code className="w-28 shrink-0 text-caption text-muted-foreground">{cls}</code>
          <span className="w-12 shrink-0 text-caption text-muted-foreground">{px}</span>
          <span className={cls}>The quick brown fox</span>
          <span className="ml-auto hidden text-caption text-muted-foreground md:inline">{role}</span>
        </div>
      ))}
      <div className="mt-6 grid gap-3 md:grid-cols-2">
        <div className="rounded-md border p-3">
          <p className="text-caption font-medium text-muted-foreground">Use</p>
          <p className="text-body">text-caption, text-body</p>
        </div>
        <div className="rounded-md border border-destructive/40 p-3">
          <p className="text-caption font-medium text-destructive">Do not use</p>
          <p className="text-body">text-xs, text-sm, text-body-sm (not a token)</p>
        </div>
      </div>
    </div>
  );
}

function InlineLinkAction() {
  return (
    <div className="p-6 max-w-[560px] space-y-4 text-body">
      <p>
        Continued from <a href="#" className="underline underline-offset-2 hover:text-foreground">chat session 4F2A</a>.
      </p>
      <p className="text-muted-foreground">
        Muted text with a link: <a href="#" className="underline underline-offset-2 hover:text-foreground">view status history</a>
      </p>
      <p>
        Emphasised (font-medium):{" "}
        <a href="#" className="inline-flex items-center gap-1 rounded-sm font-medium text-foreground underline underline-offset-2 transition-colors hover:text-foreground/80 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
          Manage runtime profiles
        </a>
      </p>
      <p>
        Same link on keyboard focus:{" "}
        <a href="#" className="rounded-sm font-medium text-foreground underline underline-offset-2 outline-none ring-2 ring-ring">Manage runtime profiles</a>
      </p>
    </div>
  );
}

const MODES = [
  { id: "link", title: "Link a directory", tag: "recommended", desc: "The agent works in place on your folder." },
  { id: "copy", title: "Copy a directory", tag: "isolated", desc: "The agent works on a copy; changes stay separate." },
  { id: "off", title: "Cloud only", tag: "unavailable", desc: "Needs a connected runtime.", disabled: true },
];
function ChoiceCard() {
  return (
    <div className="p-6 max-w-[480px]">
      <div role="radiogroup" aria-label="Directory mode" className="space-y-2">
        {MODES.map((m, i) => {
          const selected = i === 0;
          return (
            <button key={m.id} type="button" role="radio" aria-checked={selected} disabled={m.disabled}
              className={`flex w-full items-start gap-3 rounded-lg border p-3 text-left transition-colors ${
                selected ? "border-primary ring-1 ring-primary" : "border-border hover:bg-muted/50"
              } ${m.disabled ? "cursor-not-allowed opacity-60" : ""}`}>
              <span className={`mt-0.5 size-4 shrink-0 rounded-full border ${selected ? "border-primary bg-primary" : "border-muted-foreground"}`} />
              <span className="min-w-0 flex-1">
                <span className="flex items-center gap-2">
                  <span className="text-body font-medium">{m.title}</span>
                  <span className="font-mono text-micro text-muted-foreground">{m.tag}</span>
                </span>
                <span className="mt-0.5 block text-caption text-muted-foreground">{m.desc}</span>
              </span>
            </button>
          );
        })}
      </div>
    </div>
  );
}

const PATTERNS: Record<string, () => React.JSX.Element> = {
  "icon-button-action": IconButtonAction,
  "property-picker": PropertyPickerDemo,
  "issue-mention-chip": IssueMentionChip,
  "section-eyebrow": SectionEyebrow,
  "type-role-tokens": TypeRoleTokens,
  "inline-link-action": InlineLinkAction,
  "choice-card": ChoiceCard,
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
