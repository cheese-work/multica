"use client";

import { Trash2 } from "lucide-react";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  EDITABLE_GUARDS,
  addStepEdge,
  canAdvanceOnCIGreen,
  guardNeedsTimeout,
  removeStepEdge,
  stepEdges,
  updateStepEdge,
} from "@multica/core/issue-workflows";
import type { IssueWorkflowEdge, IssueWorkflowStep } from "@multica/core/types";
import { IconButton } from "@multica/ui/components/common/icon-button";
import { Input } from "@multica/ui/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
} from "@multica/ui/components/ui/select";
import { useStatusLabel } from "../issues/utils/status-label";
import { useT } from "../i18n";

/**
 * The exits of one step as a form (CHE-2150 A3b): the keyboard and
 * screen-reader route to everything the graph's drag-to-connect does, and the
 * only place a guard, timeout or on-timeout step is picked.
 */
export function WorkflowEdgeEditor({
  steps,
  stepKey,
  canEdit,
  onChange,
}: {
  steps: IssueWorkflowStep[];
  stepKey: string | null;
  canEdit: boolean;
  onChange: (steps: IssueWorkflowStep[]) => void;
}) {
  const { t } = useT("issues");
  const labelOf = useStatusLabel(useWorkspaceId());
  if (!stepKey) {
    return (
      <p className="text-caption text-muted-foreground">
        {t(($) => $.workflows.flow.edit.pick)}
      </p>
    );
  }
  const edges = stepEdges(steps, stepKey);
  const others = steps.map((s) => s.status_key).filter((k) => k !== stepKey);
  const statusItems = others.map((k) => ({ value: k, label: labelOf(k) }));
  const patch = (index: number, p: Partial<IssueWorkflowEdge>) =>
    onChange(updateStepEdge(steps, stepKey, index, p));

  return (
    <section
      aria-label={t(($) => $.workflows.flow.edit.title, { step: labelOf(stepKey) })}
      className="space-y-2"
    >
      <h4 className="text-body font-medium">
        {t(($) => $.workflows.flow.edit.title, { step: labelOf(stepKey) })}
      </h4>
      {edges.length === 0 && (
        <p className="text-caption text-muted-foreground">
          {t(($) => $.workflows.flow.edit.empty)}
        </p>
      )}
      {edges.map((e, i) => {
        // An edge the picker cannot offer (issue_status) stays selectable as-is.
        // CI green cannot move an issue out of every status.
        const pickable = EDITABLE_GUARDS.filter(
          (g) => g !== "pr.checks_green" || canAdvanceOnCIGreen(stepKey),
        );
        const guards = pickable.some((g) => g === e.guard)
          ? [...pickable]
          : [...pickable, e.guard];
        const onTimeout = e.on_timeout ?? (others.includes("blocked") ? "blocked" : "");
        return (
          <div
            key={i}
            role="group"
            aria-label={t(($) => $.workflows.flow.edit.group, { n: i + 1, to: labelOf(e.to) })}
            className="grid items-end gap-2 sm:grid-cols-[repeat(4,minmax(0,1fr))_auto]"
          >
            <Field label={t(($) => $.workflows.flow.edit.guard)}>
              <Select
                items={guards.map((g) => ({ value: g, label: g }))}
                value={e.guard}
                disabled={!canEdit}
                onValueChange={(v) => v && patch(i, { guard: v })}
              >
                <SelectTrigger aria-label={t(($) => $.workflows.flow.edit.guard)} className="w-full">
                  <span className="truncate">{e.guard}</span>
                </SelectTrigger>
                <SelectContent>
                  {guards.map((g) => (
                    <SelectItem key={g} value={g}>
                      {g}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </Field>
            <Field label={t(($) => $.workflows.flow.edit.target)}>
              <StatusSelect
                label={t(($) => $.workflows.flow.edit.target)}
                items={statusItems}
                value={e.to}
                disabled={!canEdit}
                onValueChange={(to) => patch(i, { to })}
              />
            </Field>
            {guardNeedsTimeout(e.guard) ? (
              <>
                <Field label={t(($) => $.workflows.flow.edit.timeout)}>
                  <Input
                    aria-label={t(($) => $.workflows.flow.edit.timeout)}
                    value={e.timeout ?? ""}
                    // eslint-disable-next-line no-restricted-syntax -- duration token, not copy
                    placeholder="72h"
                    disabled={!canEdit}
                    onChange={(ev) => patch(i, { timeout: ev.target.value })}
                  />
                </Field>
                <Field label={t(($) => $.workflows.flow.edit.on_timeout)}>
                  <StatusSelect
                    label={t(($) => $.workflows.flow.edit.on_timeout)}
                    items={statusItems}
                    value={onTimeout}
                    disabled={!canEdit}
                    onValueChange={(on_timeout) => patch(i, { on_timeout })}
                  />
                </Field>
              </>
            ) : (
              <>
                <span />
                <span />
              </>
            )}
            {canEdit && (
              <IconButton
                label={t(($) => $.workflows.flow.edit.remove)}
                onClick={() => onChange(removeStepEdge(steps, stepKey, i))}
              >
                <Trash2 className="size-3.5" />
              </IconButton>
            )}
          </div>
        );
      })}
      {canEdit && (
        <div className="max-w-56">
          <StatusSelect
            label={t(($) => $.workflows.flow.edit.add)}
            items={statusItems}
            value=""
            disabled={edges.length >= 8}
            onValueChange={(to) => onChange(addStepEdge(steps, stepKey, to))}
          />
        </div>
      )}
    </section>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="min-w-0 space-y-1">
      <span className="block text-caption font-medium">{label}</span>
      {children}
    </div>
  );
}

function StatusSelect({
  label,
  items,
  value,
  disabled,
  onValueChange,
}: {
  label: string;
  items: { value: string; label: string }[];
  value: string;
  disabled?: boolean;
  onValueChange: (value: string) => void;
}) {
  return (
    <Select
      items={items}
      value={value}
      disabled={disabled}
      onValueChange={(v) => v && onValueChange(v)}
    >
      <SelectTrigger aria-label={label} className="w-full">
        <span className="truncate">
          {items.find((i) => i.value === value)?.label ?? label}
        </span>
      </SelectTrigger>
      <SelectContent>
        {items.map((i) => (
          <SelectItem key={i.value} value={i.value}>
            {i.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
