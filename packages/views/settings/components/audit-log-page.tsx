"use client";

import { useMemo, useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { auditEventsOptions, type AuditLogFilters } from "@multica/core/audit/queries";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import type { AuditEvent } from "@multica/core/types";
import { agentListOptions, memberListOptions } from "@multica/core/workspace/queries";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { formatInTimeZone } from "../../common/format-in-time-zone";
import { useViewingTimezone } from "../../common/use-viewing-timezone";
import { useLocale, useT } from "../../i18n";
import { AppLink } from "../../navigation";
import { SettingsCard, SettingsTab } from "./settings-layout";

/** What the server stores in place of a value it must not keep. */
const CHANGED_MARKER = "changed";
const KIND_PREFIXES = ["agent", "runtime", "skill", "squad", "project", "workspace", "autopilot", "member"] as const;
const CONTROL_CLASS =
  "h-8 rounded-md border border-input bg-background px-2 text-body focus-visible:outline-2 focus-visible:outline-ring";

/**
 * Settings → Audit log (CHE-5653): who changed what, when. Owners and admins
 * only; Settings hides the tab from everyone else and the server refuses them.
 */
export function AuditLogPage() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const [kind, setKind] = useState("");
  const [actor, setActor] = useState("");
  const [since, setSince] = useState("");
  const [until, setUntil] = useState("");
  const filters = useMemo<AuditLogFilters>(
    () => ({
      ...(kind ? { kind } : {}),
      ...(actor ? { actor_id: actor } : {}),
      ...(isoOrUndefined(since) ? { since: isoOrUndefined(since) } : {}),
      ...(isoOrUndefined(until) ? { until: isoOrUndefined(until) } : {}),
    }),
    [kind, actor, since, until],
  );
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data, isPending, isError, refetch, hasNextPage, fetchNextPage, isFetchingNextPage } = useInfiniteQuery(
    auditEventsOptions(wsId, filters),
  );
  const events = data?.pages.flatMap((page) => page.events) ?? [];
  const filtered = Object.keys(filters).length > 0;

  return (
    <SettingsTab title={t(($) => $.page.tabs.audit_log)} description={t(($) => $.audit_log.description)}>
      <div role="group" aria-label={t(($) => $.audit_log.filters.label)} className="flex flex-wrap items-end gap-3">
        <FilterField label={t(($) => $.audit_log.filters.kind)}>
          {(id) => (
            <select id={id} className={CONTROL_CLASS} value={kind} onChange={(e) => setKind(e.target.value)}>
              <option value="">{t(($) => $.audit_log.filters.kind_all)}</option>
              {KIND_PREFIXES.map((prefix) => (
                <option key={prefix} value={`${prefix}.`}>{t(($) => $.audit_log.kinds[prefix])}</option>
              ))}
            </select>
          )}
        </FilterField>
        <FilterField label={t(($) => $.audit_log.filters.actor)}>
          {(id) => (
            <select id={id} className={CONTROL_CLASS} value={actor} onChange={(e) => setActor(e.target.value)}>
              <option value="">{t(($) => $.audit_log.filters.actor_all)}</option>
              <optgroup label={t(($) => $.audit_log.actor_types.member)}>
                {members.map((m) => (
                  <option key={m.user_id} value={m.user_id}>{m.name}</option>
                ))}
              </optgroup>
              <optgroup label={t(($) => $.audit_log.actor_types.agent)}>
                {agents.map((a) => (
                  <option key={a.id} value={a.id}>{a.name}</option>
                ))}
              </optgroup>
            </select>
          )}
        </FilterField>
        <FilterField label={t(($) => $.audit_log.filters.since)}>
          {(id) => (
            <input id={id} type="datetime-local" className={CONTROL_CLASS} value={since} onChange={(e) => setSince(e.target.value)} />
          )}
        </FilterField>
        <FilterField label={t(($) => $.audit_log.filters.until)}>
          {(id) => (
            <input id={id} type="datetime-local" className={CONTROL_CLASS} value={until} onChange={(e) => setUntil(e.target.value)} />
          )}
        </FilterField>
        {filtered && (
          <Button
            type="button"
            size="sm"
            variant="ghost"
            onClick={() => {
              setKind("");
              setActor("");
              setSince("");
              setUntil("");
            }}
          >
            {t(($) => $.audit_log.filters.clear)}
          </Button>
        )}
      </div>

      {isError && (
        <div role="alert" className="flex items-center justify-between gap-3 text-caption text-destructive">
          <span>{t(($) => $.audit_log.load_error)}</span>
          <Button type="button" size="sm" variant="outline" onClick={() => void refetch()}>
            {t(($) => $.audit_log.retry)}
          </Button>
        </div>
      )}
      {isPending && !isError ? (
        <p className="text-body text-muted-foreground">{t(($) => $.audit_log.loading)}</p>
      ) : events.length === 0 ? (
        !isError && <p className="text-body text-muted-foreground">{t(($) => $.audit_log.empty)}</p>
      ) : (
        <SettingsCard>
          <EventTable events={events} />
        </SettingsCard>
      )}
      {hasNextPage && (
        <div>
          <Button type="button" variant="outline" disabled={isFetchingNextPage} onClick={() => void fetchNextPage()}>
            {isFetchingNextPage ? t(($) => $.audit_log.loading_more) : t(($) => $.audit_log.load_more)}
          </Button>
        </div>
      )}
    </SettingsTab>
  );
}

/** `datetime-local` holds a wall-clock time in the browser's zone; the API wants an instant. */
function isoOrUndefined(local: string): string | undefined {
  if (!local) return undefined;
  const at = new Date(local);
  return Number.isNaN(at.getTime()) ? undefined : at.toISOString();
}

function FilterField({ label, children }: { label: string; children: (id: string) => React.ReactNode }) {
  const id = `audit-filter-${label.replace(/\s+/g, "-").toLowerCase()}`;
  return (
    <div className="flex flex-col gap-1">
      <label htmlFor={id} className="text-caption text-muted-foreground">{label}</label>
      {children(id)}
    </div>
  );
}

function EventTable({ events }: { events: AuditEvent[] }) {
  const { t } = useT("settings");
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-body">
        <thead className="text-caption text-muted-foreground">
          <tr>
            <th className="px-4 py-2 font-medium">{t(($) => $.audit_log.columns.time)}</th>
            <th className="px-4 py-2 font-medium">{t(($) => $.audit_log.columns.actor)}</th>
            <th className="px-4 py-2 font-medium">{t(($) => $.audit_log.columns.event)}</th>
            <th className="px-4 py-2 font-medium">{t(($) => $.audit_log.columns.target)}</th>
            <th className="px-4 py-2 font-medium">{t(($) => $.audit_log.columns.changes)}</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-surface-border">
          {events.map((event) => (
            <EventRow key={event.id} event={event} />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function EventRow({ event }: { event: AuditEvent }) {
  const { t } = useT("settings");
  const locale = useLocale();
  const timeZone = useViewingTimezone();
  return (
    <tr className="align-top">
      <td className="whitespace-nowrap px-4 py-2 tabular-nums">
        <time dateTime={event.at} title={event.at}>{formatInTimeZone(event.at, timeZone, locale)}</time>
      </td>
      <td className="px-4 py-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-medium">{event.actor_name ?? t(($) => $.audit_log.actor_types[event.actor_type])}</span>
          <Badge variant="outline">{t(($) => $.audit_log.actor_types[event.actor_type])}</Badge>
        </div>
      </td>
      <td className="px-4 py-2"><code className="text-caption">{event.kind}</code></td>
      <td className="px-4 py-2"><TargetCell event={event} /></td>
      <td className="px-4 py-2"><Diff before={event.before} after={event.after} /></td>
    </tr>
  );
}

function TargetCell({ event }: { event: AuditEvent }) {
  const { t } = useT("settings");
  const paths = useWorkspacePaths();
  if (!event.target_id) return <span className="text-muted-foreground">{t(($) => $.audit_log.none)}</span>;
  if (!event.target_name) return <span className="text-muted-foreground">{t(($) => $.audit_log.target_deleted)}</span>;
  const href = targetHref(paths, event.target_type, event.target_id);
  return href ? (
    <AppLink href={href} className="text-primary hover:underline">{event.target_name}</AppLink>
  ) : (
    <span>{event.target_name}</span>
  );
}

function targetHref(paths: ReturnType<typeof useWorkspacePaths>, type: string, id: string): string | null {
  switch (type) {
    case "agent": return paths.agentDetail(id);
    case "runtime": return paths.runtimeDetail(id);
    case "skill": return paths.skillDetail(id);
    case "squad": return paths.squadDetail(id);
    case "project": return paths.projectDetail(id);
    case "autopilot": return paths.autopilotDetail(id);
    case "workspace": return paths.settings();
    default: return null;
  }
}

/** One line per field. A field the server redacted holds the marker on both sides and shows it once. */
function Diff({ before, after }: { before: Record<string, unknown>; after: Record<string, unknown> }) {
  const { t } = useT("settings");
  const fields = [...new Set([...Object.keys(before), ...Object.keys(after)])].sort();
  if (fields.length === 0) {
    return <span className="text-muted-foreground">{t(($) => $.audit_log.no_changes)}</span>;
  }
  return (
    <ul className="space-y-1">
      {fields.map((field) => {
        const redacted = before[field] === CHANGED_MARKER && after[field] === CHANGED_MARKER;
        return (
          <li key={field} className="flex flex-wrap items-center gap-1.5">
            <code className="text-caption text-muted-foreground">{field}</code>
            {redacted ? (
              <Marker />
            ) : (
              <>
                <Value value={before[field]} />
                <span aria-hidden className="text-muted-foreground">→</span>
                <Value value={after[field]} />
              </>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function Marker() {
  const { t } = useT("settings");
  return (
    <span className="rounded bg-muted px-1.5 text-caption italic" title={t(($) => $.audit_log.changed_hint)}>
      {t(($) => $.audit_log.changed)}
    </span>
  );
}

function Value({ value }: { value: unknown }) {
  const { t } = useT("settings");
  if (value === CHANGED_MARKER) return <Marker />;
  if (value === undefined || value === null) {
    return <span className="text-muted-foreground">{t(($) => $.audit_log.none)}</span>;
  }
  return <span className="break-all">{typeof value === "string" ? value : JSON.stringify(value)}</span>;
}
