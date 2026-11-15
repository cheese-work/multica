"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentMember } from "@multica/core/permissions";
import { providerBreakersOptions, useResetProviderBreaker } from "@multica/core/runtimes";
import type { ProviderBreaker } from "@multica/core/types";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Button } from "@multica/ui/components/ui/button";
import { formatInTimeZone } from "../../common/format-in-time-zone";
import { useViewingTimezone } from "../../common/use-viewing-timezone";
import { useLocale, useT } from "../../i18n";
import { SettingsCard, SettingsSection, SettingsTab } from "./settings-layout";

const STATE_TONE: Record<ProviderBreaker["state"], string> = {
  closed: "text-success",
  open: "text-destructive",
  half_open: "text-warning",
};

/**
 * Per-provider circuit breakers of the workspace. Everyone reads the state;
 * only owners and admins can reset one (the server enforces it and audits it).
 */
export function ProviderBreakersTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const { role } = useCurrentMember(wsId);
  const canReset = role === "owner" || role === "admin";
  const { data: breakers = [], isError, refetch } = useQuery(providerBreakersOptions(wsId));
  const reset = useResetProviderBreaker(wsId);
  const [target, setTarget] = useState<string | null>(null);

  const confirmReset = async () => {
    if (!target) return;
    try {
      await reset.mutateAsync(target);
      toast.success(t(($) => $.provider_breakers.reset_success));
      setTarget(null);
    } catch {
      toast.error(t(($) => $.provider_breakers.reset_error));
    }
  };

  return (
    <SettingsTab
      title={t(($) => $.page.tabs.provider_breakers)}
      description={t(($) => $.provider_breakers.description)}
    >
      {isError && (
        <div role="alert" className="flex items-center justify-between gap-3 text-caption text-destructive">
          <span>{t(($) => $.provider_breakers.load_error)}</span>
          <Button type="button" size="sm" variant="outline" onClick={() => void refetch()}>
            {t(($) => $.provider_breakers.retry)}
          </Button>
        </div>
      )}
      <SettingsSection anchor="provider-breakers">
        {breakers.length === 0 && !isError ? (
          <p className="text-body text-muted-foreground">{t(($) => $.provider_breakers.empty)}</p>
        ) : (
          <SettingsCard>
            <BreakerTable breakers={breakers} canReset={canReset} onReset={setTarget} />
          </SettingsCard>
        )}
      </SettingsSection>
      <AlertDialog open={target !== null} onOpenChange={(next) => !next && !reset.isPending && setTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.provider_breakers.reset_title, { provider: target ?? "" })}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.provider_breakers.reset_description, { provider: target ?? "" })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={reset.isPending}>{t(($) => $.provider_breakers.reset_cancel)}</AlertDialogCancel>
            <AlertDialogAction variant="destructive" disabled={reset.isPending} onClick={() => void confirmReset()}>
              {reset.isPending ? t(($) => $.provider_breakers.resetting) : t(($) => $.provider_breakers.reset_confirm)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </SettingsTab>
  );
}

function BreakerTable({
  breakers,
  canReset,
  onReset,
}: {
  breakers: ProviderBreaker[];
  canReset: boolean;
  onReset: (provider: string) => void;
}) {
  const { t } = useT("settings");
  const locale = useLocale();
  const timeZone = useViewingTimezone();
  const time = (iso: string | null) =>
    iso ? formatInTimeZone(iso, timeZone, locale) : t(($) => $.provider_breakers.none);
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-body">
        <thead className="text-caption text-muted-foreground">
          <tr>
            <th className="px-4 py-2 font-medium">{t(($) => $.provider_breakers.columns.provider)}</th>
            <th className="px-4 py-2 font-medium">{t(($) => $.provider_breakers.columns.state)}</th>
            <th className="px-4 py-2 font-medium">{t(($) => $.provider_breakers.columns.reason)}</th>
            <th className="px-4 py-2 font-medium">{t(($) => $.provider_breakers.columns.failures)}</th>
            <th className="px-4 py-2 font-medium">{t(($) => $.provider_breakers.columns.opened)}</th>
            <th className="px-4 py-2 font-medium">{t(($) => $.provider_breakers.columns.next_probe)}</th>
            <th className="px-4 py-2 font-medium">{t(($) => $.provider_breakers.columns.last_success)}</th>
            {canReset && (
              <th className="px-4 py-2 font-medium">
                <span className="sr-only">{t(($) => $.provider_breakers.columns.actions)}</span>
              </th>
            )}
          </tr>
        </thead>
        <tbody className="divide-y divide-surface-border">
          {breakers.map((b) => (
            <tr key={b.provider}>
              <td className="px-4 py-2 font-medium">{b.provider}</td>
              <td className={`px-4 py-2 ${STATE_TONE[b.state]}`}>{t(($) => $.provider_breakers.states[b.state])}</td>
              <td className="px-4 py-2">{b.open_reason || t(($) => $.provider_breakers.none)}</td>
              <td className="px-4 py-2 tabular-nums">{b.failure_count}</td>
              <td className="px-4 py-2">{time(b.opened_at)}</td>
              <td className="px-4 py-2">{time(b.next_probe_at)}</td>
              <td className="px-4 py-2">{time(b.last_success_at)}</td>
              {canReset && (
                <td className="px-4 py-2 text-right">
                  {b.state !== "closed" && (
                    <Button
                      type="button"
                      size="sm"
                      variant="outline"
                      aria-label={t(($) => $.provider_breakers.reset_label, { provider: b.provider })}
                      onClick={() => onReset(b.provider)}
                    >
                      {t(($) => $.provider_breakers.reset)}
                    </Button>
                  )}
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
