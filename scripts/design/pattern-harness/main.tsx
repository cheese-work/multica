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
import { Button } from "@multica/ui/components/ui/button";
import { Alert, AlertDescription, AlertTitle } from "@multica/ui/components/ui/alert";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription,
  AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@multica/ui/components/ui/tabs";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@multica/ui/components/ui/collapsible";
import { HunksRegion } from "../../../packages/views/chat/components/diff-hunks";
import { useT } from "../../../packages/views/i18n";
import { Archive, ChevronRight, Copy, Download, Maximize2, RotateCcw, Trash2, TriangleAlert } from "@multica/ui/icons";
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


function InlineRetry() {
  const { t } = useT("chat");
  return (
    <div className="p-6 max-w-[560px]">
      <Row label="needs-you list error">
        <p className="text-caption text-destructive">{t(($) => $.needs_you.error)}</p>
        <Button variant="outline" size="sm">{t(($) => $.needs_you.retry)}</Button>
      </Row>
      <Row label="stats dialog error">
        <p className="text-caption text-destructive">{t(($) => $.stats.error)}</p>
        <Button variant="outline" size="sm">{t(($) => $.stats.retry)}</Button>
      </Row>
      <Row label="narrow parent (240px)">
        <div className="w-[240px] rounded-md border p-2 space-y-2">
          <p className="text-caption text-destructive">{t(($) => $.needs_you.error)}</p>
          <Button variant="outline" size="sm">{t(($) => $.needs_you.retry)}</Button>
        </div>
      </Row>
      <Row label="retry disabled"><Button variant="outline" size="sm" disabled>{t(($) => $.needs_you.retry)}</Button></Row>
    </div>
  );
}

function InlineNotice() {
  return (
    <div className="p-6 max-w-[620px] space-y-3">
      <p className="text-caption text-muted-foreground">Alert tones today: default and destructive. Warning, muted and success are ad-hoc classes at the call sites (open decision, see ledger).</p>
      <Alert>
        <AlertTitle>Wakeups paused</AlertTitle>
        <AlertDescription>No wakeup rule fires until an owner resumes them.</AlertDescription>
      </Alert>
      <Alert variant="destructive">
        <TriangleAlert />
        <AlertTitle>Interactive login failed</AlertTitle>
        <AlertDescription>The runtime could not sign in. Re-run login on the machine.</AlertDescription>
      </Alert>
      <div className="rounded-lg border border-warning/40 bg-warning/10 px-2.5 py-2 text-body text-warning">Warning box (call-site classes, not an Alert variant)</div>
      <div className="rounded-lg border bg-muted px-2.5 py-2 text-body text-muted-foreground">Muted box (call-site classes)</div>
      <div className="rounded-lg border border-success/40 bg-success/10 px-2.5 py-2 text-body text-success">Success box (call-site classes)</div>
    </div>
  );
}

// The real SlackTab reads the auth store and api (the auth store hangs headless Chrome), so this view
// composes the same Card and AlertDialog primitives and the same locale keys, states side by side.
function IntegrationTab() {
  const { t } = useT("settings");
  const Block = ({ label, children }: { label: string; children: React.ReactNode }) => (
    <section className="space-y-2"><p className="text-micro uppercase tracking-wide text-muted-foreground">{label}</p>{children}</section>
  );
  return (
    <div className="p-6 max-w-[680px] space-y-5">
      {/* Frame only: drop the scrim so the page stays legible and park the dialog beside the rows. */}
      <style>{`[data-slot="alert-dialog-overlay"]{background:transparent!important;backdrop-filter:none!important;-webkit-backdrop-filter:none!important}
@media (min-width:1024px){[data-slot="alert-dialog-content"]{top:420px!important;left:calc(50% + 140px)!important;transform:none!important}}`}</style>
      <Block label="not enabled">
        <Card><CardContent className="space-y-2">
          <p className="text-body font-medium">{t(($) => $.slack.not_enabled_title)}</p>
          <p className="text-caption text-muted-foreground">{t(($) => $.slack.not_enabled_description_prefix)} <code className="rounded-xs bg-muted px-1 py-0.5 text-micro">MULTICA_SLACK_SECRET_KEY</code> {t(($) => $.slack.not_enabled_description_suffix)}</p>
        </CardContent></Card>
      </Block>
      <Block label="loading">
        <Card><CardContent><p className="text-body text-muted-foreground">{t(($) => $.slack.loading)}</p></CardContent></Card>
      </Block>
      <Block label="empty">
        <Card><CardContent className="space-y-2">
          <p className="text-body font-medium">{t(($) => $.slack.empty_title)}</p>
          <p className="text-caption text-muted-foreground">{t(($) => $.slack.empty_description_prefix)} <strong>{t(($) => $.slack.empty_description_cta)}</strong> {t(($) => $.slack.empty_description_suffix)}</p>
        </CardContent></Card>
      </Block>
      <Block label="installation rows (active, revoked)">
        <Card><CardContent className="divide-y">
          {[["Release bot", false], ["Docs bot", true]].map(([name, revoked]) => (
            <div key={String(name)} className="flex items-start justify-between gap-4 py-3 first:pt-0 last:pb-0">
              <div className="space-y-1">
                <p className="text-body font-medium">{name}{revoked && <span className="ml-2 rounded-xs bg-muted px-1.5 py-0.5 text-micro text-muted-foreground">{t(($) => $.slack.revoked_badge)}</span>}</p>
                <p className="text-micro text-muted-foreground">{t(($) => $.slack.installed_at_label, { when: "13/11/2026, 09:30" })}</p>
              </div>
              {!revoked && <Button variant="outline" size="sm"><Trash2 className="h-3 w-3" />{t(($) => $.slack.disconnect)}</Button>}
            </div>
          ))}
        </CardContent></Card>
      </Block>
      <AlertDialog open onOpenChange={() => {}}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.slack.disconnect_confirm_title)}</AlertDialogTitle>
            <AlertDialogDescription>{t(($) => $.slack.disconnect_confirm_description)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.slack.disconnect_confirm_cancel)}</AlertDialogCancel>
            <AlertDialogAction>{t(($) => $.slack.disconnect)}</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

const HUNKS = [
  { old_start: 3, new_start: 3, lines: [" ## Goal", "-Capture 3 pilot frames.", "+Capture the remaining Patterns frames.", " ", "+Each batch has 5 items at most."] },
  { old_start: 20, new_start: 21, lines: [" ## Done when", "-Three entries link a frame.", "+No active entry is pending."] },
];
function DiffDialog() {
  const { t } = useT("chat");
  return (
    <div className="p-6 max-w-[640px] space-y-4">
      <Tabs defaultValue="diff">
        <TabsList>
          <TabsTrigger value="diff">Diff</TabsTrigger>
          <TabsTrigger value="after">After</TabsTrigger>
        </TabsList>
        <TabsContent value="diff" className="mt-3">
          <HunksRegion hunks={HUNKS} label="Plan revision 18 to 19, changed lines" />
        </TabsContent>
        <TabsContent value="after" />
      </Tabs>
      <Collapsible defaultOpen>
        <CollapsibleTrigger className="flex items-center gap-1 text-caption text-muted-foreground"><ChevronRight className="size-3" />{t(($) => $.decision.artifacts.lines_hidden, { count: 14 })}</CollapsibleTrigger>
        <CollapsibleContent className="mt-2 text-caption text-muted-foreground">Folded unchanged lines open here.</CollapsibleContent>
      </Collapsible>
    </div>
  );
}

// Each local copy formats with Intl.DateTimeFormat; 5 use Asia/Bangkok, 1 uses Asia/Ho_Chi_Minh, one appends " ICT" by literal.
function IctTimestamp() {
  const d = new Date("2026-11-13T07:30:00Z");
  const fmt = (timeZone: string) => new Intl.DateTimeFormat("en-GB", { timeZone, day: "2-digit", month: "short", hour: "2-digit", minute: "2-digit", hourCycle: "h23" }).format(d);
  return (
    <div className="p-6 max-w-[620px]">
      <Row label="recent runs (literal suffix)"><span className="text-body"><time dateTime={d.toISOString()}>{fmt("Asia/Bangkok")} ICT</time></span></Row>
      <Row label="Asia/Bangkok"><span className="text-body">{fmt("Asia/Bangkok")} ICT</span></Row>
      <Row label="Asia/Ho_Chi_Minh"><span className="text-body">{fmt("Asia/Ho_Chi_Minh")} ICT</span></Row>
      <Row label="no suffix (the defect)"><span className="text-body text-destructive">{fmt("Asia/Bangkok")}</span></Row>
      <p className="mt-3 text-caption text-muted-foreground">Rule: every time a person reads is ICT with the ICT suffix. Both zones are UTC+7, so the output matches.</p>
    </div>
  );
}

const PATTERNS: Record<string, () => React.JSX.Element> = {
  "icon-button-action": IconButtonAction,
  "property-picker": PropertyPickerDemo,
  "issue-mention-chip": IssueMentionChip,
  "inline-retry": InlineRetry,
  "inline-notice": InlineNotice,
  "integration-tab": IntegrationTab,
  "diff-dialog": DiffDialog,
  "ict-timestamp": IctTimestamp,
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
