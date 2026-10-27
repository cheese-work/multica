"use client";

import { useT } from "../../i18n";

function formatWindow(tokens: number): string {
  return tokens >= 1_000_000
    ? `${Number((tokens / 1_000_000).toFixed(1))}M`
    : `${Math.round(tokens / 1000)}k`;
}

/**
 * How full the DM's context is: the latest completed run's last prompt against
 * the model's window. Ten points below the auto-compact threshold it warns that
 * the DM compacts after the next reply. Hidden when the run reported no size.
 * `thresholdPct` is null when auto-compact is off for this DM.
 */
export function ChatContextMeter({
  tokens,
  window,
  thresholdPct,
}: {
  tokens?: number | null;
  window?: number | null;
  thresholdPct: number | null;
}) {
  const { t } = useT("chat");
  if (!tokens || !window || tokens < 0 || window <= 0) return null;
  const pct = Math.floor((tokens * 100) / window);
  const warn = thresholdPct !== null && pct >= thresholdPct - 10;
  return (
    <div className="flex shrink-0 flex-col items-end leading-tight tabular-nums">
      <span className="text-caption text-muted-foreground">
        {t(($) => $.header.context_meter, { pct, window: formatWindow(window) })}
      </span>
      {warn ? (
        <span className="text-caption text-warning">
          {t(($) => $.header.context_meter_warning)}
        </span>
      ) : null}
    </div>
  );
}
