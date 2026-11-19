"use client";

import type { IssueWorkflowLintFinding } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Spinner } from "@multica/ui/components/ui/spinner";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { useT } from "../i18n";
import { FindingRow } from "./workflow-checks-sheet";

/** The longest override reason the server accepts. */
export const LINT_REASON_MAX = 500;

/**
 * Shown when a save is refused (422 `workflow_lint_errors`). Lists the
 * unwaived errors, then asks for the reason that lets the save through. The
 * reason is the caller's state so it survives closing the dialog.
 */
export function WorkflowLintRefusalDialog({
  open,
  findings,
  reason,
  onReasonChange,
  pending,
  stepLabel,
  onSaveAnyway,
  onBack,
}: {
  open: boolean;
  findings: IssueWorkflowLintFinding[];
  reason: string;
  onReasonChange: (reason: string) => void;
  pending: boolean;
  stepLabel: (key: string) => string;
  onSaveAnyway: (reason: string) => void;
  onBack: () => void;
}) {
  const { t } = useT("issues");
  const trimmed = reason.trim();
  return (
    <Dialog open={open} onOpenChange={(v) => !pending && !v && onBack()}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>{t(($) => $.workflows.lint.refuse_title)}</DialogTitle>
          <DialogDescription>{t(($) => $.workflows.lint.refuse_description)}</DialogDescription>
        </DialogHeader>
        <ul className="max-h-64 overflow-y-auto">
          {findings.map((f, i) => (
            <FindingRow key={`${f.rule}:${f.step ?? ""}:${i}`} finding={f} stepName={f.step ? stepLabel(f.step) : undefined} />
          ))}
        </ul>
        <div className="space-y-1.5">
          <label htmlFor="workflow-lint-reason" className="text-body font-medium">
            {t(($) => $.workflows.lint.reason_label)}
          </label>
          <Textarea
            id="workflow-lint-reason"
            value={reason}
            maxLength={LINT_REASON_MAX}
            placeholder={t(($) => $.workflows.lint.reason_placeholder)}
            onChange={(e) => onReasonChange(e.target.value)}
          />
          <p className="text-right text-caption tabular-nums text-muted-foreground">
            {t(($) => $.workflows.lint.reason_count, { count: reason.length, max: LINT_REASON_MAX })}
          </p>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onBack} disabled={pending}>
            {t(($) => $.workflows.lint.back_to_editing)}
          </Button>
          <Button onClick={() => onSaveAnyway(trimmed)} disabled={trimmed.length === 0 || pending} aria-busy={pending}>
            {pending && <Spinner className="size-3.5" />}
            {t(($) => $.workflows.lint.save_anyway)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
