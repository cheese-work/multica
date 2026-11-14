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
import { memberListOptions } from "@multica/core/workspace/queries";
import { openDecisionRequestsOptions } from "@multica/core/chat/queries";
import { runtimeListOptions } from "@multica/core/runtimes/queries";
import { runBudgetKeys } from "@multica/core/run-budget";
import { issueWakeupsOptions } from "@multica/core/issues/wakeups";
import { MachinePauseBanner } from "../../../packages/views/layout/machine-pause-banner";
import { RunBudgetMeter } from "../../../packages/views/issues/components/run-budget-meter";
import { AttachmentList } from "../../../packages/views/issues/components/attachment-list";
import { WakeupInstructionEditor } from "../../../packages/views/issues/components/wakeup-instruction-editor";
import { AutoCompactTokensRow } from "../../../packages/views/settings/components/auto-compact-tokens-row";
import { getApi, setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import { useT } from "../../../packages/views/i18n";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Alert, AlertDescription, AlertTitle } from "@multica/ui/components/ui/alert";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Spinner } from "@multica/ui/components/ui/spinner";
import { Input } from "@multica/ui/components/ui/input";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@multica/ui/components/ui/field";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@multica/ui/components/ui/dialog";
import { DecisionActionApproval } from "../../../packages/views/common/decision-action-approval";
import { DescriptionDisclosure } from "../../../packages/views/issues/components/description-disclosure";
import { RunBudgetChip } from "../../../packages/views/issues/components/run-budget-chip";
import { Label } from "@multica/ui/components/ui/label";
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription,
  AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@multica/ui/components/ui/tabs";
import { HunksRegion } from "../../../packages/views/chat/components/diff-hunks";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@multica/ui/components/ui/collapsible";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@multica/ui/components/ui/select";
import { cn } from "@multica/ui/lib/utils";
import { PillMenu } from "../../../packages/views/issues/components/wakeup-pickers";
import {
  Archive, ChevronDown, ChevronRight, Copy, Download, Maximize2, MessageSquareQuote, Plus, RotateCcw, Trash2, TriangleAlert,
} from "@multica/ui/icons";
import {
  SettingsCard, SettingsReadOnlyNotice, SettingsRow, SettingsSaveState, SettingsSection, SettingsTab,
} from "../../../packages/views/settings/components/settings-layout";
// Lazy: the SettingsPage module graph is large; the other views must not pay for it (or hang on it) at init.
const SettingsPage = lazy(() =>
  import("../../../packages/views/settings/components/settings-page").then((m) => ({ default: m.SettingsPage })),
);
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@multica/ui/components/ui/table";
import { AgentCreateShell, AgentCreateChip } from "../../../packages/views/agents/create/create-shell";
import { WorkspaceSlugProvider } from "../../../packages/core/paths/hooks";
import { NavigationProvider } from "../../../packages/views/navigation/context";
import {
  seedRawElements, DecisionBanner, HierarchyMinimap, HierarchyDialogPage, WakeupEventRowPage, ConditionParamsPage,
  WakeupFieldsPage, WakeupPickersPage, WorkspaceWakeupRulesPage, ChatSearchPage, ConfigChangeViewerPage,
} from "./raw-elements";
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

// staleTime Infinity: seeded data must not refetch (no server here) and flip to the error state.
const qc = new QueryClient({ defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false, staleTime: Infinity } } });
seedRawElements(qc);
const nav = {
  push: () => {}, replace: () => {}, back: () => {}, pathname: "/", searchParams: new URLSearchParams(), hash: "",
  getShareableUrl: (path: string) => path,
};
// The chip reads the issue list cache; seed one bucket (flattenIssueBuckets walks every category).
qc.setQueryData(issueListOptions(WS).queryKey, {
  byStatus: { [STATUS_ORDER[0]!]: { issues: ISSUES, total: ISSUES.length } },
});

const MIN = 60_000;
const runtime = (id: string, name: string, daemon: string, tasks: number, pause: object | null) => ({
  id, workspace_id: WS, daemon_id: daemon, name, runtime_mode: "local", provider: "claude", launch_header: "",
  status: "online", device_info: "Linux x86_64", metadata: {}, owner_id: "u-owner", visibility: "private",
  pause, daemon_running_tasks: tasks, last_seen_at: new Date().toISOString(),
  created_at: "2026-11-01T00:00:00Z", updated_at: new Date().toISOString(),
});
const paused = (reason: string, inMin: number) => ({
  reason, paused_by: "u-owner", paused_at: new Date(Date.now() - 10 * MIN).toISOString(),
  expires_at: new Date(Date.now() + inMin * MIN).toISOString(),
});
qc.setQueryData(runtimeListOptions(WS).queryKey, [
  runtime("r1", "x99 (Claude)", "d-x99", 3, paused("maintenance", 42)),
  runtime("r2", "c00 (Codex)", "d-c00", 0, paused("incident", 3)),
]);
qc.setQueryData(memberListOptions(WS).queryKey, [
  { id: "m1", workspace_id: WS, user_id: "u-owner", role: "owner", created_at: "2026-09-01T00:00:00Z", name: "Cheese", email: "cheese@example.com", avatar_url: null },
]);
qc.setQueryData(openDecisionRequestsOptions(WS).queryKey, []);
const budget = (used: number, left: number, state: string, canRaise: boolean) => ({
  budgeted: true, runsUsed: used, runsLeft: left, runsLimit: 20, state, windowId: "w1",
  windowStartedAt: "2026-11-13T00:00:00Z", blocking: [], canRaise,
  raisePreview: canRaise ? { addRuns: 10, limitAfter: 30 } : null,
});
qc.setQueryData(runBudgetKeys.issue(WS, "b-ok"), budget(8, 12, "ok", true));
qc.setQueryData(runBudgetKeys.issue(WS, "b-near"), budget(17, 3, "near", true));
qc.setQueryData(runBudgetKeys.issue(WS, "b-limit"), budget(20, 0, "at_limit", false));
qc.setQueryData(issueWakeupsOptions(WS, "i-wake").queryKey, [{
  id: "w1", issue_id: "i-wake", agent_id: "a1", agent_name: "x99-claude-sonnet-design",
  instruction: "Check the PR for CI results and post the verdict on the issue.", kind: "event", mode: "once",
  event_types: ["pr.checks"], filter_agent_id: null, filter_task_id: null, interval_seconds: null, cron_expression: null,
  timezone: "Asia/Bangkok", next_fire_at: null, enabled: true, revision: 3, disabled_at: null, last_task_id: null, last_error: null,
}]);

// Runs after the first paint: click, type, submit. Virtual time in the capture makes the delays free.
const drive = (...steps: Array<() => void>) => steps.forEach((step, i) => setTimeout(step, 400 * (i + 1)));
const typeInto = (el: HTMLInputElement | HTMLTextAreaElement, value: string) => {
  const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, "value")!.set!.call(el, value);
  el.dispatchEvent(new Event("input", { bubbles: true }));
};
const member = (id: string, name: string, role: string) => ({ id, user_id: id, workspace_id: WS, name, role, email: `${id}@example.com` });
qc.setQueryData(memberListOptions("ws-managers").queryKey, [
  member("m1", "Mai Tran", "owner"), member("m2", "Linh Pham", "admin"), member("m3", "Bao Nguyen", "member"),
]);
qc.setQueryData(memberListOptions("ws-no-managers").queryKey, [member("m4", "Bao Nguyen", "member")]);

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

function SettingsPageDemo() {
  return (
    <div className="mx-auto max-w-4xl p-6">
      <SettingsTab title="Preferences" description="How Multica looks and behaves on this device." scope="device">
        <SettingsSection title="Appearance" description="Theme and language apply on this device only.">
          <SettingsCard>
            <SettingsRow label="Theme" description="Follows the system by default." size="select">
              <div className="h-9 rounded-lg border border-input px-3 text-body leading-9">System</div>
            </SettingsRow>
            <SettingsRow label="Language" size="select">
              <div className="h-9 rounded-lg border border-input px-3 text-body leading-9">English</div>
            </SettingsRow>
          </SettingsCard>
        </SettingsSection>
        <SettingsSection title="Workspace profile" scope="workspace">
          <SettingsCard>
            <SettingsRow label="Display name" description="Shown to other members of this workspace." size="text">
              <div className="h-9 rounded-lg border border-input px-3 text-body leading-9">Ada Lovelace</div>
            </SettingsRow>
            <SettingsRow label="Issue prefix" size="code">
              <div className="h-9 rounded-lg border border-input px-3 text-body leading-9">CHE</div>
            </SettingsRow>
            <SettingsRow label="Email digest" description="One summary a day, at 09:00 ICT.">
              <div className="h-5 w-9 rounded-full bg-primary" />
            </SettingsRow>
          </SettingsCard>
        </SettingsSection>
      </SettingsTab>
    </div>
  );
}

const bar = (children: React.ReactNode) => <div className="p-6 max-w-[760px]">{children}</div>;
const Pad = ({ label, children }: { label: string; children: React.ReactNode }) => (
  <section className="mb-6"><p className="mb-2 text-caption text-muted-foreground">{label}</p>{children}</section>
);

const PauseBanner = () => bar(<div className="-mx-3 -mt-3"><MachinePauseBanner /></div>);

const RunBudget = () => bar(
  <div className="w-[260px]">
    <Pad label="ok, 8 of 20"><RunBudgetMeter issueId="b-ok" /></Pad>
    <Pad label="near the limit (80 %)"><RunBudgetMeter issueId="b-near" /></Pad>
    <Pad label="exhausted"><RunBudgetMeter issueId="b-limit" /></Pad>
  </div>,
);

const att = (id: string, filename: string, content_type: string, size_bytes: number, revision = 1, revision_of: string | null = null) => ({
  id, workspace_id: WS, issue_id: "i1", comment_id: null, chat_session_id: null, chat_message_id: null,
  uploader_type: "agent", uploader_id: "a1", filename, url: `https://files.example/${id}`,
  download_url: `https://files.example/${id}`, markdown_url: `https://files.example/${id}`,
  content_type, size_bytes, created_at: "2026-11-13T09:00:00Z", revision, revision_of,
});
const ATTACHMENTS = [
  att("a1", "CHE-5529-patterns-report.html", "text/html", 48_213),
  att("a2", "release-notes.md", "text/markdown", 3_120, 3, "a2-prev"),
  att("a2-prev", "release-notes.md", "text/markdown", 2_900, 2),
  att("a3", "capture-frame.log", "text/plain", 912),
  att("a4", "design-review.pdf", "application/pdf", 1_204_551),
];
const AttachmentCards = () => bar(<AttachmentList attachments={ATTACHMENTS as never} />);

function WakeupRuleForm() {
  useEffect(() => drive(
    () => document.querySelector<HTMLButtonElement>("#root button")?.click(),
    () => {
      const area = document.querySelector<HTMLTextAreaElement>("form textarea");
      if (area) typeInto(area, "");
    },
    () => document.querySelector<HTMLButtonElement>("form button[type=submit]")?.click(),
  ), []);
  return bar(<WakeupInstructionEditor workspaceId={WS} issueId="i-wake" wakeupId="w1" />);
}

function SettingsNumberRow() {
  useEffect(() => drive(() => {
    const input = document.querySelector<HTMLInputElement>("input[name=hard]");
    if (input) typeInto(input, "9999999");
  }), []);
  const common = { min: 1000, max: 1_000_000, disabled: false, invalidText: "Enter a whole number from 1,000 to 1,000,000.", onSave: async () => {} };
  return bar(
    <div className="divide-y rounded-lg border">
      <AutoCompactTokensRow {...common} label="Soft limit (tokens)" name="soft" saved={150_000} />
      <AutoCompactTokensRow {...common} label="Hard limit (tokens)" name="hard" saved={200_000} />
    </div>,
  );
}

// DecisionActionApproval reads its outcome from the API on mount. Stub that one read per action id so each
// reachable state renders without a server: pending -> approve and reject, a terminal state -> its outcome.
const OUTCOMES: Record<string, unknown> = {
  "act-pending": { id: "act-pending", state: "pending" },
  "act-executed": { id: "act-executed", state: "executed", result: { outcome: "cap_applied", from: 20, to: 30 } },
  "act-failed": { id: "act-failed", state: "failed", error_code: "stale_cap" },
  "act-rejected": { id: "act-rejected", state: "rejected" },
};
setApiInstance({
  ...getApi(),
  getDecisionActionOutcome: async (id: string) => {
    const outcome = OUTCOMES[id];
    if (!outcome) throw new Error("outcome unavailable");
    return outcome;
  },
} as unknown as ApiClient);

function DecisionActionApprovalDemo() {
  const sha = "0".repeat(64);
  const Cell = ({ label, children }: { label: string; children: React.ReactNode }) => (
    <div className="flex items-start gap-4 border-b py-3 last:border-b-0">
      <span className="w-44 shrink-0 pt-1 text-caption text-muted-foreground">{label}</span>
      <div className="min-w-0 flex-1">{children}</div>
    </div>
  );
  return (
    <div className="p-6 max-w-[760px]">
      <Cell label="undecided, owner">
        <DecisionActionApproval actionId="act-pending" payloadSha256={sha} />
      </Cell>
      <Cell label="undecided, not owner (approve hidden)">
        <DecisionActionApproval actionId="act-pending" payloadSha256={sha} canApprove={false} />
      </Cell>
      <Cell label="details unavailable (approve disabled)">
        <DecisionActionApproval actionId="act-pending" payloadSha256={sha} approveDisabled />
      </Cell>
      <Cell label="executed, cap applied"><DecisionActionApproval actionId="act-executed" payloadSha256={sha} /></Cell>
      <Cell label="failed, stale cap"><DecisionActionApproval actionId="act-failed" payloadSha256={sha} /></Cell>
      <Cell label="rejected"><DecisionActionApproval actionId="act-rejected" payloadSha256={sha} /></Cell>
      <Cell label="outcome unreadable (lost)"><DecisionActionApproval actionId="act-unknown" payloadSha256={sha} /></Cell>
    </div>
  );
}

const LONG_DESCRIPTION = Array.from({ length: 9 }, (_, n) =>
  `Step ${n + 1}: the agent reads the issue, the comment thread and the open decisions before it touches the repository.`);
function DescriptionDisclosureDemo() {
  const { t } = useT("chat");
  const [open, setOpen] = useState<Record<string, boolean>>({ expanded: true });
  const labels = {
    preview: "Description preview", loading: "Loading", showMore: t(($) => $.needs_you.show_all, { count: 9 }),
    showLess: t(($) => $.needs_you.show_less), moreLines: () => "",
  };
  const Block = ({ k, lines }: { k: string; lines: string[] }) => (
    <DescriptionDisclosure id={k} expanded={!!open[k]} onExpandedChange={(v) => setOpen((o) => ({ ...o, [k]: v }))} labels={labels}>
      <div className="space-y-2 text-body">{lines.map((l) => <p key={l}>{l}</p>)}</div>
    </DescriptionDisclosure>
  );
  return (
    <div className="p-6 max-w-[640px] space-y-8">
      <div><p className="mb-2 text-caption text-muted-foreground">collapsed, long text (preview + count)</p><Block k="collapsed" lines={LONG_DESCRIPTION} /></div>
      <div><p className="mb-2 text-caption text-muted-foreground">expanded (Show less)</p><Block k="expanded" lines={LONG_DESCRIPTION.slice(0, 7)} /></div>
      <div><p className="mb-2 text-caption text-muted-foreground">short text: no toggle</p><Block k="short" lines={["One short line of description."]} /></div>
    </div>
  );
}

function StatusPillDemo() {
  return (
    <div className="p-6">
      <Row label="Badge default"><Badge>In progress</Badge></Row>
      <Row label="secondary"><Badge variant="secondary">Todo</Badge></Row>
      <Row label="outline"><Badge variant="outline">Agent review</Badge></Row>
      <Row label="destructive"><Badge variant="destructive">Blocked</Badge></Row>
      <Row label="ghost"><Badge variant="ghost">Backlog</Badge></Row>
      <Row label="run-budget-chip (real)"><RunBudgetChip issue={{ status: "blocked", run_budget_blocked: true }} /></Row>
      <Row label="beside a status label">
        <span className="text-body">CHE-5420</span><Badge variant="destructive">Blocked</Badge>
        <RunBudgetChip issue={{ status: "blocked", run_budget_blocked: true }} />
      </Row>
    </div>
  );
}

const VARIANTS = ["default", "outline", "secondary", "ghost", "brand", "brandSubtle", "destructive", "link"] as const;
function Buttons() {
  return (
    <div className="p-6">
      <Row label="variants, default size">
        {VARIANTS.map((v) => <Button key={v} variant={v} type="button">{v}</Button>)}
      </Row>
      <Row label="sizes, default variant">
        {(["xs", "sm", "default", "lg"] as const).map((z) => <Button key={z} size={z} type="button">{z}</Button>)}
      </Row>
      <Row label="icon-only (named)">
        {(["icon-xs", "icon-sm", "icon", "icon-lg"] as const).map((z) => (
          <Button key={z} size={z} variant="ghost" type="button" aria-label={`Add item (${z})`}><Plus /></Button>
        ))}
      </Row>
      <Row label="one dominant action">
        <Button type="button" variant="outline">Cancel</Button>
        <Button type="submit">Save</Button>
      </Row>
      <Row label="pending: disabled + aria-busy">
        <Button type="submit" disabled aria-busy="true"><Spinner />Saving</Button>
      </Row>
      <Row label="disabled"><Button type="button" disabled>Save</Button></Row>
    </div>
  );
}

const navAdapter = {
  push() {}, replace() {}, back() {},
  pathname: "/acme/settings", searchParams: new URLSearchParams(""), hash: "",
  getShareableUrl: (path: string) => path,
};
function SettingsNavigation() {
  return (
    <NavigationProvider value={navAdapter}>
      <div className="flex h-[900px] w-full">
        <Suspense fallback={null}><SettingsPage /></Suspense>
      </div>
    </NavigationProvider>
  );
}

function ReadOnlyMemberView() {
  return (
    <div className="max-w-2xl space-y-6 p-6">
      <Row label="owners and admins named"><div className="w-full"><SettingsReadOnlyNotice wsId="ws-managers" /></div></Row>
      <Row label="no manager found"><div className="w-full"><SettingsReadOnlyNotice wsId="ws-no-managers" /></div></Row>
    </div>
  );
}

function HeaderChipPopoverDemo() {
  const { t } = useT("chat");
  const rows = [
    ["CHE-5428", "x99-claude-sonnet-design", "Approve the cap raise for the doop deploy?"],
    ["CHE-5420", "x99-codex-leader", "Pick a direction for the Patterns page"],
    ["CHE-5303", "x99-claude-opus", "Run budget stopped this issue at 20 runs"],
  ];
  return (
    <div className="p-6 min-h-[420px]">
      <div className="flex items-center justify-between rounded-md border px-3 py-2">
        <span className="text-body font-medium">Direct message</span>
        <Popover open>
          <PopoverTrigger render={<Button variant="outline" size="sm" className="shrink-0 border-warning/40 bg-warning/10 text-warning" />}>
            {t(($) => $.needs_you.chip, { count: rows.length })}
          </PopoverTrigger>
          <PopoverContent align="end" className="w-80">
            <div className="text-caption font-medium text-muted-foreground">{t(($) => $.needs_you.decisions)}</div>
            <ul className="flex max-h-64 flex-col overflow-y-auto">
              {rows.map(([key, agent, ask]) => (
                <li key={key} className="flex min-h-11 flex-col gap-1 rounded-md px-2 py-2 text-caption hover:bg-accent/50">
                  <span className="flex items-center gap-2 text-muted-foreground">
                    <span className="font-medium text-foreground">{key}</span><span className="min-w-0 flex-1 truncate">{agent}</span>
                  </span>
                  <span className="line-clamp-2 font-medium">{ask}</span>
                </li>
              ))}
            </ul>
          </PopoverContent>
        </Popover>
      </div>
    </div>
  );
}

function Dialogs() {
  return (
    <Dialog open>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Edit title</DialogTitle>
          <DialogDescription>Update the task title.</DialogDescription>
        </DialogHeader>
        <div className="grid gap-1.5">
          <Label htmlFor="t">Title</Label>
          <Input id="t" defaultValue="Review interface" />
        </div>
        <DialogFooter>
          <Button type="button" variant="outline">Cancel</Button>
          <Button type="button">Save</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function AlertDialogs() {
  return (
    <AlertDialog open>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete this wakeup rule?</AlertDialogTitle>
          <AlertDialogDescription>Runs already queued still start. You cannot undo this.</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction variant="destructive">Delete rule</AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function TabsDemo() {
  return (
    <div className="grid gap-8 p-6 max-w-[560px]">
      <Tabs defaultValue="overview">
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="runs">Runs</TabsTrigger>
          <TabsTrigger value="settings">Settings</TabsTrigger>
          <TabsTrigger value="audit" disabled>Audit</TabsTrigger>
        </TabsList>
        <TabsContent value="overview">One tabpanel per tab. Left and Right move, Home and End jump.</TabsContent>
        <TabsContent value="runs">Runs</TabsContent>
        <TabsContent value="settings">Settings</TabsContent>
      </Tabs>
      <Tabs defaultValue="history">
        <TabsList variant="line">
          <TabsTrigger value="history">History</TabsTrigger>
          <TabsTrigger value="planned">Planned</TabsTrigger>
        </TabsList>
        <TabsContent value="history">line variant</TabsContent>
        <TabsContent value="planned">Planned</TabsContent>
      </Tabs>
      <Tabs defaultValue="a" orientation="vertical" className="flex-row">
        <TabsList>
          <TabsTrigger value="a">General</TabsTrigger>
          <TabsTrigger value="b">Members</TabsTrigger>
        </TabsList>
        <TabsContent value="a" className="p-2">vertical</TabsContent>
        <TabsContent value="b" className="p-2">Members</TabsContent>
      </Tabs>
    </div>
  );
}

const CopyField = ({ label, help, value }: { label: string; help?: string; value: string }) => (
  <div className="grid gap-1.5">
    <Label>{label}</Label>
    <Input defaultValue={value} />
    {help ? <p className="text-caption text-muted-foreground">{help}</p> : null}
  </div>
);
function UiCopy() {
  return (
    <div className="grid gap-6 p-6 max-w-[520px]">
      <section className="grid gap-3 rounded-md border p-4">
        <h3 className="text-label font-medium">Omit: the label already says it</h3>
        <CopyField label="Workspace name" value="Cheese Work" />
        <CopyField label="Wakeup interval (minutes)" help="Minimum 5. Shorter values are rounded up." value="15" />
      </section>
      <section className="grid gap-3 rounded-md border p-4">
        <h3 className="text-label font-medium">Avoid: restates the label, the value and the button</h3>
        <CopyField label="Workspace name" help="Enter the name of your workspace here. You can change this later." value="Cheese Work" />
        <Button type="button" className="w-fit">Save</Button>
        <p className="text-caption text-muted-foreground">Click Save to save your changes.</p>
      </section>
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

function SaveState() {
  const labels = { savingLabel: "Saving…", savedLabel: "Saved", errorLabel: "Could not save. Try again." };
  return (
    <div className="p-6">
      <Row label="idle (renders nothing)"><SettingsSaveState status="idle" {...labels} /></Row>
      <Row label="saving"><SettingsSaveState status="saving" {...labels} /></Row>
      <Row label="saved"><SettingsSaveState status="saved" {...labels} /></Row>
      <Row label="error"><SettingsSaveState status="error" {...labels} /></Row>
    </div>
  );
}

function FormDialogDemo() {
  return (
    <div className="min-h-[640px]">
      <Dialog open onOpenChange={() => {}}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>Add local directory</DialogTitle>
            <DialogDescription>Runs on this machine can read and edit the folder.</DialogDescription>
          </DialogHeader>
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="dir-label">Label</FieldLabel>
              <Input id="dir-label" defaultValue="multica fork" />
            </Field>
            <Field data-invalid>
              <FieldLabel htmlFor="dir-ref">Branch</FieldLabel>
              <Input id="dir-ref" aria-invalid defaultValue="feature//bad" />
              <FieldDescription>Use letters, digits, dash and slash.</FieldDescription>
              <FieldError>A branch name cannot hold two slashes in a row.</FieldError>
            </Field>
          </FieldGroup>
          <div className="rounded-md bg-muted px-2.5 py-1.5 font-mono text-micro text-muted-foreground break-all">
            /mnt/ssd2/multica_workspaces/cheese-work/che-5528/workdir/multica
          </div>
          <Alert variant="destructive">
            <TriangleAlert />
            <AlertTitle>In-place runs edit this folder</AlertTitle>
            <AlertDescription>Uncommitted changes in it can be overwritten.</AlertDescription>
          </Alert>
          <DialogFooter>
            <Button variant="ghost">Cancel</Button>
            <Button>Add directory</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
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

const APPS: [string, string, "default" | "secondary" | "outline", string][] = [
  ["GitHub", "Pull requests, issues and checks for cheese-work repos.", "default", "Connected"],
  ["Linear", "Read and write issues from agent runs.", "secondary", "Needs reauth"],
  ["Notion", "Search and update workspace pages.", "outline", "Not connected"],
  ["Slack", "Post run summaries to a channel.", "default", "Connected"],
];
function IntegrationCardGrid() {
  return (
    <div className="grid max-w-[900px] grid-cols-1 gap-3 p-6 md:grid-cols-2">
      {APPS.map(([name, desc, variant, status]) => (
        <Card key={name}>
          <CardContent className="flex items-center gap-3 p-3">
            <div className="min-w-0 flex-1">
              <div className="truncate text-body font-medium">{name}</div>
              <div className="truncate text-caption text-muted-foreground">{desc}</div>
            </div>
            <Badge variant={variant}>{status}</Badge>
          </CardContent>
        </Card>
      ))}
    </div>
  );
}

const BREAKERS: [string, string, string, string][] = [
  ["anthropic", "Open", "5 failures in 60 s", "14:02"],
  ["openai", "Closed", "none", "13:47"],
  ["google", "Half-open", "1 probe pending", "13:55"],
];
function SettingsTableSection() {
  return (
    <div className="max-w-[760px] p-6">
      <div className="rounded-md border">
        <Table>
          <TableHeader>
            <TableRow><TableHead>Provider</TableHead><TableHead>State</TableHead><TableHead>Reason</TableHead><TableHead className="text-right">Updated</TableHead></TableRow>
          </TableHeader>
          <TableBody>
            {BREAKERS.map(([p, s, r, u]) => (
              <TableRow key={p}><TableCell className="font-medium">{p}</TableCell><TableCell>{s}</TableCell><TableCell className="text-muted-foreground">{r}</TableCell><TableCell className="text-right tabular-nums">{u}</TableCell></TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

// Same markup as settings-layout's SettingsRow. Importing it pulls the core auth store, which hangs headless Chrome at module init.
const ActionRowLayout = ({ label, description, children }: { label: string; description: string; children: React.ReactNode }) => (
  <div className="flex min-h-16 flex-row items-center justify-between gap-4 px-4 py-3.5 sm:gap-8">
    <div className="min-w-0 flex-1">
      <div className="text-body font-medium">{label}</div>
      <div className="mt-0.5 text-caption leading-5 text-muted-foreground">{description}</div>
    </div>
    <div className="w-auto max-w-[56%] shrink-0">{children}</div>
  </div>
);
function SettingsActionRow() {
  return (
    <div className="max-w-[760px] divide-y rounded-md border m-6">
      <ActionRowLayout label="Scheduling" description="Pausing stops every agent from claiming new runs. Running tasks finish; queued work waits.">
        <Button variant="outline" size="sm">Pause workspace</Button>
      </ActionRowLayout>
      <ActionRowLayout label="Scheduling (paused)" description="Resume lets agents claim queued work again. Resume needs no confirm.">
        <Button variant="outline" size="sm">Resume workspace</Button>
      </ActionRowLayout>
      <ActionRowLayout label="Scheduling (saving)" description="The button disables while the change is in flight.">
        <Button variant="outline" size="sm" disabled>Pause workspace</Button>
      </ActionRowLayout>
    </div>
  );
}

function CreateShell() {
  return (
    <div className="flex h-[860px] flex-col">
      <AgentCreateShell title="Create an agent" step="" stepIndex={2} onClose={() => {}}
        chips={<><AgentCreateChip>Build with AI</AgentCreateChip><AgentCreateChip>x99 · Claude Code</AgentCreateChip></>}>
        <div className="p-6 text-body text-muted-foreground">Step content renders here.</div>
      </AgentCreateShell>
    </div>
  );
}

function ConfirmDialog() {
  return (
    <div className="min-h-[520px] p-6">
      <AlertDialog open>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Pause this workspace?</AlertDialogTitle>
            <AlertDialogDescription>No agent claims new runs until you resume. Queued work is kept.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep running</AlertDialogCancel>
            <AlertDialogAction variant="destructive">Pause</AlertDialogAction>
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

const CHOICE_MODES = [
  { id: "link", title: "Link a directory", tag: "recommended", desc: "The agent works in place on your folder." },
  { id: "copy", title: "Copy a directory", tag: "isolated", desc: "The agent works on a copy; changes stay separate." },
  { id: "off", title: "Cloud only", tag: "unavailable", desc: "Needs a connected runtime.", disabled: true },
];
function ChoiceCard() {
  return (
    <div className="p-6 max-w-[480px]">
      <div role="radiogroup" aria-label="Directory mode" className="space-y-2">
        {CHOICE_MODES.map((m, i) => {
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
  "settings-page": SettingsPageDemo,
  "settings-navigation": SettingsNavigation,
  "read-only-member-view": ReadOnlyMemberView,
  "save-state": SaveState,
  "pause-banner": PauseBanner,
  "run-budget": RunBudget,
  "attachment-card-list": AttachmentCards,
  "wakeup-rule-form": WakeupRuleForm,
  "settings-number-row": SettingsNumberRow,
  "decision-action-approval": DecisionActionApprovalDemo,
  "description-disclosure": DescriptionDisclosureDemo,
  "status-pill": StatusPillDemo,
  "header-chip-popover": HeaderChipPopoverDemo,
  "form-dialog": FormDialogDemo,
  "decision-banner": DecisionBanner,
  "hierarchy-minimap": HierarchyMinimap,
  "hierarchy-dialog": HierarchyDialogPage,
  "wakeup-event-row": WakeupEventRowPage,
  "wakeup-condition-params": ConditionParamsPage,
  "wakeup-fields": WakeupFieldsPage,
  "wakeup-pickers": WakeupPickersPage,
  "workspace-wakeup-rules": WorkspaceWakeupRulesPage,
  "chat-search": ChatSearchPage,
  "config-change-viewer": ConfigChangeViewerPage,
  buttons: Buttons,
  dialogs: Dialogs,
  "dialogs-alert": AlertDialogs,
  tabs: TabsDemo,
  "ui-copy": UiCopy,
  "lazy-detail": LazyDetail,
  "decision-receipt-card": DecisionReceiptCard,
  "collapsible-section": CollapsibleSection,
  "settings-select-row": SettingsSelectRow,
  "pill-menu": PillMenuDemo,
  "inline-retry": InlineRetry,
  "inline-notice": InlineNotice,
  "integration-tab": IntegrationTab,
  "diff-dialog": DiffDialog,
  "ict-timestamp": IctTimestamp,
  "integration-card-grid": IntegrationCardGrid,
  "settings-table-section": SettingsTableSection,
  "settings-action-row": SettingsActionRow,
  "create-shell": CreateShell,
  "confirm-dialog": ConfirmDialog,
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
        <WorkspaceSlugProvider slug="harness">
          <NavigationProvider value={nav as never}>
            <TooltipProvider>{View ? <View /> : <p className="p-6">unknown pattern: {id}</p>}</TooltipProvider>
          </NavigationProvider>
        </WorkspaceSlugProvider>
      </I18nProvider>
    </QueryClientProvider>
  </StrictMode>,
);
