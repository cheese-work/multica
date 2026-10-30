import type { ChatSessionUsage, ChatUsageTokens } from "../types/chat";
import {
  COST_USD_TICKS_PER_USD,
  estimateCost,
  isModelPriced,
} from "../runtimes/pricing";

interface ChatSessionUsageSummary {
  /** Dollars. Null when no recorded usage can be priced; never 0 for "unknown". */
  costUsd: number | null;
  /** True when part of the usage has no price or some finished runs reported none. */
  partial: boolean;
  /** True when at least one usage row was recorded for the session. */
  hasUsage: boolean;
  /** Finished runs that reported no usage. */
  unreportedRuns: number;
  totalTokens: number;
  /** Token counts by kind, for the whole session. */
  tokenKinds: ChatUsageTokens;
  totalRunMs: number;
  models: { model: string; tokens: number; tokenKinds: ChatUsageTokens; costUsd: number | null }[];
  runs: { completed: number; failed: number; cancelled: number; inProgress: number };
  messages: { user: number; assistant: number };
}

const TERMINAL_STATUSES = new Set(["completed", "failed", "cancelled"]);

function sumTokens(t: ChatUsageTokens): number {
  return t.input + t.output + t.cache_read + t.cache_write;
}

/**
 * Turns the usage endpoint's payload into what the Statistics dialog shows.
 *
 * Cost follows the one product formula (`estimateCost`): a model the provider
 * priced uses that price; a model it priced none of is estimated from the rate
 * table; a model with neither gets no cost and makes the total partial. The
 * payload splits uncosted tokens per session, not per model, so a model that
 * mixes priced and unpriced rows can only show its priced part, and the total
 * is partial when more uncosted tokens exist than the unpriced models explain.
 */
export function summarizeChatSessionUsage(usage: ChatSessionUsage): ChatSessionUsageSummary {
  let costUsd = 0;
  let pricedModels = 0;
  let partial = usage.runs.unreported > 0;
  let explainedUncosted = 0;

  const models = Object.entries(usage.by_model).map(([model, row]) => {
    const tokens = sumTokens(row.tokens);
    let modelCost: number | null;
    if (row.cost_usd_ticks > 0) {
      modelCost = row.cost_usd_ticks / COST_USD_TICKS_PER_USD;
    } else {
      explainedUncosted += tokens;
      modelCost = isModelPriced(model)
        ? estimateCost({
            model,
            input_tokens: row.tokens.input,
            output_tokens: row.tokens.output,
            cache_read_tokens: row.tokens.cache_read,
            cache_write_tokens: row.tokens.cache_write,
          })
        : null;
    }
    if (modelCost === null) partial = true;
    else {
      costUsd += modelCost;
      pricedModels += 1;
    }
    return { model, tokens, tokenKinds: row.tokens, costUsd: modelCost };
  });

  if (sumTokens(usage.uncosted_tokens) > explainedUncosted) partial = true;

  const status = usage.runs.by_status;
  const count = (key: string) => status[key] ?? 0;
  const inProgress = Object.entries(status)
    .filter(([key]) => !TERMINAL_STATUSES.has(key))
    .reduce((sum, [, n]) => sum + n, 0);

  return {
    costUsd: pricedModels > 0 ? costUsd : null,
    partial,
    hasUsage: models.length > 0,
    unreportedRuns: usage.runs.unreported,
    totalTokens: sumTokens(usage.tokens),
    tokenKinds: usage.tokens,
    totalRunMs: usage.runs.total_run_ms,
    models,
    runs: {
      completed: count("completed"),
      failed: count("failed"),
      cancelled: count("cancelled"),
      inProgress,
    },
    messages: {
      user: usage.messages.by_role.user ?? 0,
      assistant: usage.messages.by_role.assistant ?? 0,
    },
  };
}
