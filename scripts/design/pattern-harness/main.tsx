import { StrictMode, useState } from "react";
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
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import { useT } from "../../../packages/views/i18n";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Alert, AlertDescription, AlertTitle } from "@multica/ui/components/ui/alert";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@multica/ui/components/ui/field";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@multica/ui/components/ui/dialog";
import { DecisionActionApproval } from "../../../packages/views/common/decision-action-approval";
import { DescriptionDisclosure } from "../../../packages/views/issues/components/description-disclosure";
import { RunBudgetChip } from "../../../packages/views/issues/components/run-budget-chip";
import { Archive, Copy, TriangleAlert, Download, Maximize2, RotateCcw, Trash2 } from "@multica/ui/icons";
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

// DecisionActionApproval reads its outcome from the API on mount. Stub that one read per action id so each
// reachable state renders without a server: pending -> approve and reject, a terminal state -> its outcome.
const OUTCOMES: Record<string, unknown> = {
  "act-pending": { id: "act-pending", state: "pending" },
  "act-executed": { id: "act-executed", state: "executed", result: { outcome: "cap_applied", from: 20, to: 30 } },
  "act-failed": { id: "act-failed", state: "failed", error_code: "stale_cap" },
  "act-rejected": { id: "act-rejected", state: "rejected" },
};
setApiInstance({
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

const PATTERNS: Record<string, () => React.JSX.Element> = {
  "icon-button-action": IconButtonAction,
  "property-picker": PropertyPickerDemo,
  "issue-mention-chip": IssueMentionChip,
  "decision-action-approval": DecisionActionApprovalDemo,
  "description-disclosure": DescriptionDisclosureDemo,
  "status-pill": StatusPillDemo,
  "header-chip-popover": HeaderChipPopoverDemo,
  "form-dialog": FormDialogDemo,
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
