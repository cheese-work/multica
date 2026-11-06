"use client";

import { useQuery } from "@tanstack/react-query";
import { ciWatchEventsOptions, type CIWatchEvent } from "@multica/core/ci-watch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@multica/ui/components/ui/table";
import { useT } from "../../i18n";

const ICT = new Intl.DateTimeFormat("en-CA", {
  timeZone: "Asia/Ho_Chi_Minh",
  year: "numeric",
  month: "2-digit",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});

/** `2026-11-06 08:00 ICT`: people here read ICT, never bare UTC. */
export function formatICT(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  const p = Object.fromEntries(ICT.formatToParts(date).map((x) => [x.type, x.value]));
  return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute} ICT`;
}

function EventRow({ event, repo, showWouldDo }: { event: CIWatchEvent; repo: string; showWouldDo: boolean }) {
  const { t } = useT("settings");
  return (
    <TableRow>
      <TableCell className="whitespace-nowrap">{formatICT(event.at)}</TableCell>
      <TableCell>{event.workflow || event.detail}</TableCell>
      <TableCell>{event.class || event.kind}</TableCell>
      <TableCell>{event.protocol}</TableCell>
      <TableCell>{event.action}</TableCell>
      <TableCell>{event.state}</TableCell>
      <TableCell>{event.outcome}</TableCell>
      {showWouldDo ? <TableCell className="min-w-32 whitespace-normal">{event.would_do}</TableCell> : null}
      <TableCell>
        {event.run_id > 0 ? (
          <a
            className="underline underline-offset-2"
            href={`https://github.com/${repo}/actions/runs/${event.run_id}`}
            target="_blank"
            rel="noreferrer"
            aria-label={t(($) => $.ci_watch.run_link, { id: event.run_id })}
          >
            #{event.run_id}
          </a>
        ) : null}
      </TableCell>
    </TableRow>
  );
}

/** Last 50 events; the "would have done" column shows only while the saved mode is Shadow. */
export function CIWatcherEventsTable({
  wsId,
  repo,
  shadow,
}: {
  wsId: string;
  repo: string;
  shadow: boolean;
}) {
  const { t } = useT("settings");
  const { data: events = [], isError, isPending } = useQuery(ciWatchEventsOptions(wsId, repo));
  const title = t(($) => $.ci_watch.events_title);
  const heads = [
    t(($) => $.ci_watch.col_time),
    t(($) => $.ci_watch.col_workflow),
    t(($) => $.ci_watch.col_class),
    t(($) => $.ci_watch.col_protocol),
    t(($) => $.ci_watch.col_action),
    t(($) => $.ci_watch.col_state),
    t(($) => $.ci_watch.col_outcome),
    ...(shadow ? [t(($) => $.ci_watch.col_would_do)] : []),
    t(($) => $.ci_watch.col_run),
  ];
  return (
    <div className="space-y-2 px-4 py-3.5">
      <h4 className="text-body font-medium">{title}</h4>
      {isError ? (
        <p className="text-caption text-destructive">{t(($) => $.ci_watch.events_error)}</p>
      ) : isPending ? null : events.length === 0 ? (
        <p className="text-caption text-muted-foreground">{t(($) => $.ci_watch.events_empty)}</p>
      ) : (
        <Table aria-label={title} className="text-caption">
          <TableHeader>
            <TableRow>
              {heads.map((h) => (
                <TableHead key={h}>{h}</TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody>
            {events.map((e, i) => (
              <EventRow key={`${e.at}-${e.run_id}-${i}`} event={e} repo={repo} showWouldDo={shadow} />
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
