"use client";

import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentWorkspace } from "@multica/core/paths";
import { GUARD_WINDOW_DAYS, guardDecisionsOptions, guardStatsOptions } from "@multica/core/guard/queries";
import type { GuardDecision } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { ChevronLeft } from "@multica/ui/icons";
import { cn } from "@multica/ui/lib/utils";
import { formatInTimeZone } from "../../common/format-in-time-zone";
import { useViewingTimezone } from "../../common/use-viewing-timezone";
import { useLocale, useT } from "../../i18n";
import { type GuardAction, type GuardRow, buildGuardRows, parseGuardPolicy } from "./guard-model";
import { SettingsTab } from "./settings-layout";

const DECISION_LIMIT = 20;
const CHART_HEIGHT_PX = 84;
const REVIEW_PREVIEW = 3;
const STATS_COMMAND = "multica guard stats";
const PLUGIN_SET_COMMAND = "multica guard plugin set --file policy.json --out action.json";

const PILL = "inline-flex h-5 items-center rounded-sm px-2 text-caption font-semibold";
const ACTION_PILL: Record<GuardAction, string> = {
  fix: "bg-info/10 text-info",
  deny: "bg-destructive/10 text-destructive",
  shadow: "bg-muted text-muted-foreground",
  pass: "bg-muted text-muted-foreground",
  unknown: "bg-muted text-muted-foreground",
};
const FOCUS = "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring";

/**
 * Tool-guard plugins with what each one did in the last week. Reads the same
 * server rows as `multica guard log` and `multica guard stats`, so the numbers
 * match. Edits go through a passkey decision, not this page.
 */
export function GuardTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const workspace = useCurrentWorkspace();
  const policy = parseGuardPolicy(workspace?.settings);
  const { data: stats, isError, refetch } = useQuery(guardStatsOptions(wsId));
  const rows = stats ? buildGuardRows(policy, stats) : [];
  const [picked, setPicked] = useState<string | null>(null);
  const [detailOpen, setDetailOpen] = useState(false);
  const rowRefs = useRef(new Map<string, HTMLButtonElement>());
  const detailRef = useRef<HTMLElement>(null);
  const selected = rows.find((r) => r.name === picked) ?? rows[0] ?? null;

  const pick = (name: string) => {
    setPicked(name);
    setDetailOpen(true);
  };
  const restoreFocus = useRef(false);
  const back = () => {
    restoreFocus.current = true;
    setDetailOpen(false);
  };
  // Focus after the list is visible again; a hidden element cannot take focus.
  useEffect(() => {
    if (detailOpen || !restoreFocus.current) return;
    restoreFocus.current = false;
    if (selected) rowRefs.current.get(selected.name)?.focus();
  }, [detailOpen, selected]);

  return (
    <SettingsTab
      title={t(($) => $.page.tabs.guard)}
      description={t(($) => $.guard.description)}
      actions={
        <span className="text-caption text-muted-foreground">
          {policy ? t(($) => $.guard.version, { version: policy.version }) : t(($) => $.guard.defaults)}
        </span>
      }
    >
      {isError ? (
        <div role="alert" className="flex items-center justify-between gap-3 text-caption text-destructive">
          <span>{t(($) => $.guard.load_error)}</span>
          <Button type="button" size="sm" variant="outline" className="min-h-11 md:min-h-0" onClick={() => void refetch()}>
            {t(($) => $.guard.retry)}
          </Button>
        </div>
      ) : !stats ? (
        <p role="status" className="text-body text-muted-foreground">{t(($) => $.guard.loading)}</p>
      ) : rows.length === 0 ? (
        <div className="space-y-1">
          <p className="text-body font-medium">{t(($) => $.guard.empty_title)}</p>
          <p className="text-caption text-muted-foreground">{t(($) => $.guard.empty_hint)}</p>
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-[minmax(0,340px)_minmax(0,1fr)] md:items-start">
          <div className={cn("space-y-4", detailOpen && "max-md:hidden")}>
            <button
              type="button"
              className={cn("sr-only focus:not-sr-only focus:rounded-md focus:bg-primary focus:px-3 focus:py-2 focus:text-primary-foreground", FOCUS)}
              onClick={() => detailRef.current?.focus()}
            >
              {t(($) => $.guard.skip_to_detail)}
            </button>
            <PluginList
              rows={rows}
              selected={selected?.name ?? null}
              locked={policy?.locked ?? false}
              days={stats.days}
              onPick={pick}
              register={(name, el) => (el ? rowRefs.current.set(name, el) : rowRefs.current.delete(name))}
            />
            {policy && policy.review.length > 0 && <ReviewPolicy rules={policy.review} />}
          </div>
          {selected && (
            <PluginDetail
              key={selected.name}
              row={selected}
              locked={policy?.locked ?? false}
              since={stats.since}
              tz={stats.tz}
              open={detailOpen}
              onBack={back}
              regionRef={detailRef}
            />
          )}
        </div>
      )}
      <p className="flex flex-wrap items-center gap-x-4 gap-y-1 text-caption text-muted-foreground">
        <span>{t(($) => $.guard.cli_footer)}</span>
        <Code>{STATS_COMMAND}</Code>
        <Code>
          {selected ? `multica guard log --plugin ${selected.name} --since ${GUARD_WINDOW_DAYS}d` : "multica guard log"}
        </Code>
      </p>
    </SettingsTab>
  );
}

function Code({ children }: { children: string }) {
  return <code className="rounded-sm bg-muted px-1.5 py-px font-mono text-caption">{children}</code>;
}

function ActionPill({ action, locked }: { action: GuardAction; locked: boolean }) {
  const { t } = useT("settings");
  if (action === "unknown") return null;
  const label = {
    fix: t(($) => $.guard.action_fix),
    deny: t(($) => $.guard.action_deny),
    shadow: t(($) => $.guard.action_watch),
    pass: t(($) => $.guard.action_pass),
  }[action];
  return (
    <>
      <span className={cn(PILL, ACTION_PILL[action])}>{label}</span>
      {locked && <span className={cn(PILL, "bg-surface-selected text-surface-selected-foreground")}>{t(($) => $.guard.locked)}</span>}
    </>
  );
}

function PluginList({
  rows,
  selected,
  locked,
  days,
  onPick,
  register,
}: {
  rows: GuardRow[];
  selected: string | null;
  locked: boolean;
  days: number;
  onPick: (name: string) => void;
  register: (name: string, el: HTMLButtonElement | null) => void;
}) {
  const { t } = useT("settings");
  return (
    <section className="rounded-lg border border-surface-border bg-card p-1.5">
      <h3 className="px-2.5 pb-1 pt-2.5 text-micro font-semibold uppercase tracking-wide text-muted-foreground">
        {t(($) => $.guard.plugins_heading, { count: rows.length, days })}
      </h3>
      <ul aria-label={t(($) => $.guard.list_label)} className="space-y-0.5">
        {rows.map((row) => (
          <li key={row.name}>
            <button
              type="button"
              ref={(el) => register(row.name, el)}
              aria-current={row.name === selected ? "true" : undefined}
              onClick={() => onPick(row.name)}
              className={cn(
                "flex min-h-11 w-full flex-col justify-center gap-1 rounded-md border border-transparent p-2.5 text-left hover:bg-surface-hover",
                row.name === selected && "border-surface-border bg-surface-selected hover:bg-surface-selected",
                FOCUS,
                "focus-visible:-outline-offset-2",
              )}
            >
              <span className="flex items-center gap-2">
                <StatusDot errors={row.errors} />
                <span className="min-w-0 flex-1 truncate text-body font-semibold">{row.name}</span>
                <ActionPill action={row.action} locked={locked} />
              </span>
              <span className="flex gap-3 text-caption tabular-nums text-muted-foreground">
                <Count label={t(($) => $.guard.fixed)} value={row.fixed} />
                <Count label={t(($) => $.guard.refused)} value={row.refused} />
                <Count label={t(($) => $.guard.errors)} value={row.errors} />
              </span>
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

function Count({ label, value }: { label: string; value: number }) {
  return (
    <span>
      {label} <b className="font-semibold text-foreground">{value}</b>
    </span>
  );
}

function StatusDot({ errors }: { errors: number }) {
  const { t } = useT("settings");
  return (
    <span
      role="img"
      aria-label={errors > 0 ? t(($) => $.guard.status_errors) : t(($) => $.guard.status_on)}
      className={cn("size-2 shrink-0 rounded-full", errors > 0 ? "bg-warning" : "bg-success")}
    />
  );
}

function ReviewPolicy({ rules }: { rules: { repo: string; github: string; reviewers: string[] }[] }) {
  const { t } = useT("settings");
  const [all, setAll] = useState(false);
  const shown = all ? rules : rules.slice(0, REVIEW_PREVIEW);
  return (
    <section className="rounded-lg border border-surface-border bg-card p-1.5">
      <h3 className="px-2.5 pb-1 pt-2.5 text-micro font-semibold uppercase tracking-wide text-muted-foreground">
        {t(($) => $.guard.review_heading, { count: rules.length })}
      </h3>
      <ul className="space-y-0.5">
        {shown.map((rule) => {
          const parts = [
            rule.reviewers.length > 0 ? t(($) => $.guard.review_reviewers, { count: rule.reviewers.length }) : "",
            rule.github === "skip" ? t(($) => $.guard.review_skip) : t(($) => $.guard.review_required),
          ].filter(Boolean);
          return (
            <li key={rule.repo} className="space-y-1 p-2.5">
              <p className="break-all text-body font-semibold">{rule.repo}</p>
              <p className="text-caption text-muted-foreground">{parts.join(" · ")}</p>
            </li>
          );
        })}
      </ul>
      {rules.length > REVIEW_PREVIEW && (
        <Button type="button" variant="ghost" size="sm" className="min-h-11 md:min-h-0" onClick={() => setAll((v) => !v)}>
          {all ? t(($) => $.guard.show_fewer) : t(($) => $.guard.show_all, { count: rules.length })}
        </Button>
      )}
    </section>
  );
}

function PluginDetail({
  row,
  locked,
  since,
  tz,
  open,
  onBack,
  regionRef,
}: {
  row: GuardRow;
  locked: boolean;
  since: string;
  tz: string;
  open: boolean;
  onBack: () => void;
  regionRef: React.Ref<HTMLElement>;
}) {
  const { t } = useT("settings");
  const [editing, setEditing] = useState(false);
  const actionText = {
    fix: t(($) => $.guard.fix_desc),
    deny: t(($) => $.guard.deny_desc),
    shadow: t(($) => $.guard.watch_desc),
    pass: t(($) => $.guard.pass_desc),
    unknown: t(($) => $.guard.not_in_policy),
  }[row.action];
  const matches = [row.tool, ...row.subcommand].filter(Boolean).join(" ");
  return (
    <section
      ref={regionRef}
      tabIndex={-1}
      role="region"
      aria-label={t(($) => $.guard.detail_label)}
      data-open={open ? "true" : "false"}
      className={cn("min-w-0 rounded-lg border border-surface-border bg-card", !open && "max-md:hidden")}
    >
      <header className="flex flex-wrap items-start gap-3 border-b border-surface-border p-4">
        <div className="min-w-0 flex-1 basis-full space-y-1 md:basis-0">
          <button
            type="button"
            onClick={onBack}
            className={cn("inline-flex min-h-11 items-center gap-1 text-label font-medium text-brand md:hidden", FOCUS)}
          >
            <ChevronLeft className="size-4" aria-hidden="true" />
            {t(($) => $.guard.back)}
          </button>
          <h2 className="break-words text-body-lg font-semibold">{row.name}</h2>
        </div>
        <span className="inline-flex h-5 items-center rounded-full bg-muted px-2 text-caption font-medium text-muted-foreground">
          {t(($) => $.guard.scope)}
        </span>
        <Button type="button" variant="outline" size="sm" className="min-h-11 md:min-h-0" aria-expanded={editing} onClick={() => setEditing((v) => !v)}>
          {t(($) => $.guard.edit)}
        </Button>
      </header>
      {editing && (
        <div className="space-y-2 border-b border-surface-border bg-muted/40 p-4 text-label">
          <p className="font-medium">{t(($) => $.guard.edit_title)}</p>
          <p className="text-muted-foreground">{t(($) => $.guard.edit_step1)}</p>
          <Code>{PLUGIN_SET_COMMAND}</Code>
          <p className="text-muted-foreground">{t(($) => $.guard.edit_step2)}</p>
        </div>
      )}
      <dl className="grid gap-x-4 gap-y-2.5 border-b border-surface-border p-4 text-label md:grid-cols-[140px_minmax(0,1fr)]">
        {matches && (
          <>
            <dt className="text-muted-foreground">{t(($) => $.guard.matches)}</dt>
            <dd className="mb-2 md:mb-0"><Code>{matches}</Code></dd>
          </>
        )}
        <dt className="text-muted-foreground">{t(($) => $.guard.action)}</dt>
        <dd className="mb-2 flex flex-wrap items-center gap-2 md:mb-0">
          <ActionPill action={row.action} locked={locked} />
          <span>{actionText}</span>
        </dd>
        {row.action !== "unknown" && (
          <>
            <dt className="text-muted-foreground">{t(($) => $.guard.message)}</dt>
            <dd className="mb-2 break-words md:mb-0">
              {row.message ? <Code>{row.message}</Code> : <span className="text-muted-foreground">{t(($) => $.guard.message_builtin)}</span>}
            </dd>
            <dt className="text-muted-foreground">{t(($) => $.guard.on_error)}</dt>
            <dd className="mb-2 md:mb-0">{row.failClosed ? t(($) => $.guard.fail_closed) : t(($) => $.guard.fail_open)}</dd>
          </>
        )}
        <dt className="text-muted-foreground">{t(($) => $.guard.status)}</dt>
        <dd className="flex items-center gap-2">
          <StatusDot errors={row.errors} />
          {row.errors > 0 ? t(($) => $.guard.status_errors) : t(($) => $.guard.status_on)}
        </dd>
      </dl>
      <HitsChart row={row} since={since} tz={tz} />
      <RecentDecisions plugin={row.name} />
    </section>
  );
}

function dayLabel(day: string, locale: string): string {
  // The day is already in the viewer's zone; format it as a bare calendar date.
  return new Intl.DateTimeFormat(locale, { weekday: "short", timeZone: "UTC" }).format(new Date(`${day}T12:00:00Z`));
}

function HitsChart({ row, since, tz }: { row: GuardRow; since: string; tz: string }) {
  const { t } = useT("settings");
  const locale = useLocale();
  const daily = row.daily;
  const peak = Math.max(1, ...daily.map((d) => d.fixed + d.refused));
  const unit = CHART_HEIGHT_PX / peak;
  const to = daily.length > 0 ? daily[daily.length - 1]!.day : since;
  return (
    <div className="space-y-2.5 border-b border-surface-border p-4">
      <h3 className="text-label font-semibold">{t(($) => $.guard.hits_title, { from: since, to, tz })}</h3>
      <div
        role="img"
        aria-label={t(($) => $.guard.chart_label, {
          fixed: daily.map((d) => d.fixed).join(", "),
          refused: daily.map((d) => d.refused).join(", "),
        })}
        className="flex items-end gap-1.5"
        style={{ height: CHART_HEIGHT_PX + 18 }}
      >
        {daily.map((d) => (
          <div key={d.day} className="flex flex-1 flex-col justify-end gap-px text-center text-micro text-muted-foreground">
            {d.refused > 0 && <i className="block rounded-t-sm bg-destructive" style={{ height: d.refused * unit }} />}
            {d.fixed > 0 && <i className={cn("block bg-brand", d.refused === 0 && "rounded-t-sm")} style={{ height: d.fixed * unit }} />}
            {dayLabel(d.day, locale)}
          </div>
        ))}
      </div>
      <p className="flex gap-3.5 text-caption text-muted-foreground">
        <span className="flex items-center gap-1.5"><i className="size-2.5 rounded-sm bg-brand" />{t(($) => $.guard.fixed)} {row.fixed}</span>
        <span className="flex items-center gap-1.5"><i className="size-2.5 rounded-sm bg-destructive" />{t(($) => $.guard.refused)} {row.refused}</span>
      </p>
    </div>
  );
}

function RecentDecisions({ plugin }: { plugin: string }) {
  const { t } = useT("settings");
  const locale = useLocale();
  const viewingTz = useViewingTimezone();
  const wsId = useWorkspaceId();
  const { data, isError, isPending } = useQuery(guardDecisionsOptions(wsId, { plugin, limit: DECISION_LIMIT }));
  const outcome = (action: string) =>
    ({
      fixed: t(($) => $.guard.outcome_fixed),
      refused: t(($) => $.guard.outcome_refused),
      "plugin-error": t(($) => $.guard.outcome_error),
      "shadow-hit": t(($) => $.guard.outcome_seen),
      pass: t(($) => $.guard.outcome_pass),
    })[action] ?? action;
  const pill = (d: GuardDecision) =>
    d.action === "refused" ? "bg-destructive/10 text-destructive" : d.action === "fixed" ? "bg-info/10 text-info" : "bg-muted text-muted-foreground";
  return (
    <div className="space-y-2 p-4">
      <h3 className="text-label font-semibold">{t(($) => $.guard.decisions_title)}</h3>
      {isError ? (
        <p role="alert" className="text-caption text-destructive">{t(($) => $.guard.decisions_error)}</p>
      ) : isPending ? (
        <p role="status" className="text-caption text-muted-foreground">{t(($) => $.guard.loading)}</p>
      ) : data.length === 0 ? (
        <p className="text-caption text-muted-foreground">{t(($) => $.guard.decisions_empty)}</p>
      ) : (
        <div role="region" aria-label={t(($) => $.guard.decisions_title)} tabIndex={0} className={cn("overflow-x-auto", FOCUS)}>
          <table className="w-full min-w-[520px] text-left text-caption">
            <thead className="text-muted-foreground">
              <tr>
                {(["col_time", "col_runtime", "col_issue", "col_outcome", "col_line"] as const).map((key) => (
                  <th key={key} className="py-1.5 pr-3 font-semibold">{t(($) => $.guard[key])}</th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-surface-border">
              {data.map((d) => (
                <tr key={d.id}>
                  <td className="whitespace-nowrap py-2 pr-3">{formatInTimeZone(d.time, viewingTz, locale)}</td>
                  <td className="py-2 pr-3">{d.runtime}</td>
                  <td className="py-2 pr-3">{d.issue}</td>
                  <td className="py-2 pr-3"><span className={cn(PILL, pill(d))}>{outcome(d.action)}</span></td>
                  <td className="py-2 break-words">{d.line}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
