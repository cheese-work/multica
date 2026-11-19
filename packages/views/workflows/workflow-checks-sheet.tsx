"use client";

import { useState } from "react";
import { filterLint, groupLintByStep, lintCounts, type LintFilter } from "@multica/core/issue-workflows";
import type { IssueWorkflowLintFinding, WorkflowLintResponse } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@multica/ui/components/ui/sheet";
import { Spinner } from "@multica/ui/components/ui/spinner";
import { Tabs, TabsList, TabsTrigger } from "@multica/ui/components/ui/tabs";
import { CircleCheck } from "@multica/ui/icons";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../i18n";
import { LintLevelIcon } from "./lint-level";

const ICT = "Asia/Ho_Chi_Minh";

/** "19 Nov, 14:02 ICT": times in this UI carry the ICT suffix. */
export function formatIct(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return iso;
  const text = new Intl.DateTimeFormat("en-GB", {
    timeZone: ICT,
    day: "numeric",
    month: "short",
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  }).format(at);
  return `${text} ICT`;
}

/**
 * The Checks sheet (CHE-3665, design direction D): the saved workflow's lint
 * findings, filtered by level and grouped by step. Waived findings stay in the
 * list, muted, with their reason; they are not counted.
 */
export function WorkflowChecksSheet({
  open,
  onOpenChange,
  workflowName,
  result,
  loading,
  failed,
  checkedAt,
  stepLabel,
  onRerun,
  onGoToStep,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  workflowName: string;
  result: WorkflowLintResponse | undefined;
  loading: boolean;
  failed: boolean;
  /** ISO time of the last lint result. */
  checkedAt?: string;
  stepLabel: (key: string) => string;
  onRerun: () => void;
  onGoToStep: (key: string) => void;
}) {
  const { t } = useT("issues");
  const [filter, setFilter] = useState<LintFilter>("all");
  const findings = result?.findings ?? [];
  const counts = lintCounts(findings);
  const shown = groupLintByStep(filterLint(findings, filter));
  const tabs: { value: LintFilter; label: string; count: number }[] = [
    { value: "all", label: t(($) => $.workflows.lint.tab_all), count: findings.length },
    { value: "error", label: t(($) => $.workflows.lint.tab_errors), count: counts.errors },
    { value: "warning", label: t(($) => $.workflows.lint.tab_warnings), count: counts.warnings },
    { value: "info", label: t(($) => $.workflows.lint.tab_notes), count: counts.infos },
  ];

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="w-full gap-0 sm:max-w-[440px]">
        <SheetHeader className="gap-0 px-5 pb-0 pt-4">
          <SheetTitle>{t(($) => $.workflows.lint.sheet_title)}</SheetTitle>
          <SheetDescription className="flex items-center gap-1.5">
            <span>
              {checkedAt
                ? t(($) => $.workflows.lint.sheet_meta, { workflow: workflowName, time: formatIct(checkedAt) })
                : workflowName}
            </span>
            <Button variant="link" size="sm" className="h-auto p-0" onClick={onRerun}>
              {t(($) => $.workflows.lint.rerun)}
            </Button>
          </SheetDescription>
          <Tabs value={filter} onValueChange={(v) => setFilter(v as LintFilter)} className="mt-3">
            <TabsList variant="line" aria-label={t(($) => $.workflows.lint.tablist_label)} className="w-full justify-start gap-4 border-b">
              {tabs.map((tab) => (
                <TabsTrigger key={tab.value} value={tab.value} className="flex-none px-0">
                  {tab.label}
                  <span className="font-mono text-caption font-normal tabular-nums">{tab.count}</span>
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
        </SheetHeader>
        {/* The findings scroll under the fixed header and footer. */}
        <div role="tabpanel" aria-label={tabs.find((x) => x.value === filter)?.label} className="min-h-0 flex-1 overflow-y-auto px-5 py-1">
          {loading ? (
            <p className="flex items-center gap-2 py-6 text-body text-muted-foreground">
              <Spinner className="size-3.5" />
              {t(($) => $.workflows.lint.running)}
            </p>
          ) : failed ? (
            <div role="alert" className="py-6 text-body">
              <p className="font-medium">{t(($) => $.workflows.lint.failed)}</p>
              <p className="mt-1 text-muted-foreground">{t(($) => $.workflows.lint.failed_note)}</p>
            </div>
          ) : shown.length === 0 ? (
            <p className="flex items-center gap-2 py-6 text-body text-muted-foreground">
              <CircleCheck aria-hidden className="size-3.5" />
              {t(($) => $.workflows.lint.clean)}
            </p>
          ) : (
            shown.map((group) => (
              <section key={group.step} className="pt-3">
                <div className="flex items-center justify-between gap-2">
                  <h4 className="truncate text-body font-semibold">
                    {group.step ? stepLabel(group.step) : t(($) => $.workflows.lint.whole_workflow)}
                  </h4>
                  {group.step && (
                    <Button
                      variant="link"
                      size="sm"
                      className="h-auto shrink-0 p-0 text-caption"
                      aria-label={t(($) => $.workflows.lint.go_to_step_named, { step: stepLabel(group.step) })}
                      onClick={() => onGoToStep(group.step)}
                    >
                      {t(($) => $.workflows.lint.go_to_step)}
                    </Button>
                  )}
                </div>
                <ul>
                  {group.findings.map((f, i) => (
                    <FindingRow key={`${f.rule}:${i}`} finding={f} />
                  ))}
                </ul>
              </section>
            ))
          )}
        </div>
        <p className="border-t px-5 py-3 text-caption text-muted-foreground">{t(($) => $.workflows.lint.footer)}</p>
      </SheetContent>
    </Sheet>
  );
}

/** One finding: level icon, message, fix hint, rule id. Shared by the sheet and the refusal dialog. */
export function FindingRow({ finding, stepName }: { finding: IssueWorkflowLintFinding; stepName?: string }) {
  const { t } = useT("issues");
  return (
    <li className={cn("grid grid-cols-[14px_1fr] gap-x-2.5 border-t py-2.5 first:border-t-0", finding.waived && "text-muted-foreground")}>
      {finding.waived ? <CircleCheck aria-hidden className="mt-0.5 size-3.5" /> : <LintLevelIcon level={finding.level} className="mt-0.5" />}
      <div className="min-w-0 text-body">
        {stepName && <p className="text-caption text-muted-foreground">{stepName}</p>}
        <p className={cn(!finding.waived && "font-medium")}>{finding.message}</p>
        {finding.waived ? (
          <p>{t(($) => $.workflows.lint.waived, { reason: finding.waive_reason ?? "" })}</p>
        ) : (
          finding.hint && (
            <p className="text-muted-foreground">
              <b className="font-medium text-foreground">{t(($) => $.workflows.lint.fix)}</b> {finding.hint}
            </p>
          )
        )}
        <p className="mt-0.5 font-mono text-micro">{finding.rule}</p>
      </div>
    </li>
  );
}
