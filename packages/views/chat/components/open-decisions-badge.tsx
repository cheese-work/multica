"use client";

import { useQuery } from "@tanstack/react-query";
import { Button } from "@multica/ui/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { useWorkspaceId } from "@multica/core/hooks";
import { paths, useWorkspaceSlug } from "@multica/core/paths";
import { openDecisionRequestsOptions } from "@multica/core/chat/queries";
import { useT } from "../../i18n";
import { AppLink } from "../../navigation";
import { decisionHeadline } from "./needs-you-list";

/**
 * "N decisions need you" in the DM header, next to "Scheduled". A button, not
 * a link: opening it only shows the list; a row is what navigates, to the DM
 * message that holds the card. Hidden while nothing is open.
 */
export function OpenDecisionsBadge() {
  const wsId = useWorkspaceId();
  const slug = useWorkspaceSlug();
  const { t } = useT("chat");
  const { data } = useQuery(openDecisionRequestsOptions(wsId));
  if (!slug || !data?.length) return null;
  const requests = [...data].sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at));
  return (
    <Popover>
      <PopoverTrigger
        render={
          <Button
            variant="outline"
            size="sm"
            className="shrink-0 border-warning/40 bg-warning/10 text-warning"
            data-testid="open-decisions-badge"
          />
        }
      >
        {t(($) => $.needs_you.chip, { count: requests.length })}
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80">
        <div className="text-caption font-medium text-muted-foreground">{t(($) => $.needs_you.decisions)}</div>
        <ul className="flex max-h-64 flex-col overflow-y-auto">
          {requests.map((request) => {
            const params = new URLSearchParams({ session: request.chat_session_id });
            if (request.message_id) params.set("message", request.message_id);
            return (
              <li key={request.id}>
                <AppLink
                  href={`${paths.workspace(slug).chat()}?${params}`}
                  className="flex min-h-11 flex-col gap-1 rounded-md px-2 py-2 text-caption outline-none hover:bg-accent/50 focus-visible:ring-1 focus-visible:ring-ring"
                >
                  <span className="flex items-center gap-2 text-muted-foreground">
                    <span className="font-medium text-foreground">{request.issue_identifier}</span>
                    <span className="min-w-0 flex-1 truncate">{request.asking_agent_name || request.asking_agent_id}</span>
                  </span>
                  <span className="line-clamp-2 font-medium">{decisionHeadline(request.ask, request.issue_identifier)}</span>
                </AppLink>
              </li>
            );
          })}
        </ul>
      </PopoverContent>
    </Popover>
  );
}
