"use client";

import { useT } from "../../i18n";

function formatWindow(tokens: number): string {
  return tokens >= 1_000_000
    ? `${Number((tokens / 1_000_000).toFixed(1))}M`
    : `${Math.round(tokens / 1000)}k`;
}

/**
 * How full the DM's context is: the latest completed run's last prompt against
 * the model's window. Near the auto-compact token threshold it shows where the
 * DM compacts: with a known last-turn size, "after the next reply" only when
 * less than one turn remains; otherwise from 90% of the threshold tokens.
 * Hidden when the run reported no size. `thresholdTokens` is null when
 * auto-compact is off for this DM.
 */
export function ChatContextMeter({
  tokens,
  window,
  thresholdTokens,
  lastTurnTokens,
}: {
  tokens?: number | null;
  window?: number | null;
  thresholdTokens: number | null;
  /** Prompt growth of the last turn, when known. */
  lastTurnTokens?: number | null;
}) {
  const { t } = useT("chat");
  if (!tokens || !window || tokens < 0 || window <= 0) return null;
  const pct = Math.floor((tokens * 100) / window);
  const turn = lastTurnTokens && lastTurnTokens > 0 ? lastTurnTokens : null;
  const finalTurn =
    thresholdTokens !== null && turn !== null && thresholdTokens - tokens < turn;
  const nearThreshold =
    thresholdTokens !== null && turn === null && tokens >= thresholdTokens * 0.9;
  return (
    <div className="flex shrink-0 flex-col items-end leading-tight tabular-nums">
      <span className="text-caption text-muted-foreground">
        {t(($) => $.header.context_meter, { pct, window: formatWindow(window) })}
      </span>
      {finalTurn ? (
        <span className="text-caption text-warning">
          {t(($) => $.header.context_meter_warning)}
        </span>
      ) : nearThreshold ? (
        <span className="text-caption text-warning">
          {t(($) => $.header.context_meter_threshold, {
            threshold: formatWindow(thresholdTokens),
            now: formatWindow(tokens),
          })}
        </span>
      ) : null}
    </div>
  );
}
