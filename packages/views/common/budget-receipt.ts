import type { BudgetReceipt } from "@multica/core/api/passkey-schema";
import type { useT } from "../i18n";

type T = ReturnType<typeof useT<"common">>["t"];

export type BudgetReceiptLines = {
  /** "Applied. At application, CHE-1 showed 0 of 25 runs."; null without numbers or an issue label. */
  effect: string | null;
  /** The "Runs used" row: label and "44 of 25 → 0 of 25 (new window)"; null unless every number is present. */
  runs: { label: string; change: string } | null;
  /** One line on what became of the refused run; null for `cap_applied` and for a token this client does not know. */
  outcome: string | null;
};

/**
 * The one mapping from a server receipt (CHE-6943 §4) to card text, shared by the decided card and
 * the approval done view. An unknown top-level token gives plain "Applied." and no outcome line; a known state with an
 * unknown reason gives that state's generic line.
 */
export function budgetReceiptLines(receipt: BudgetReceipt, t: T, issue = ""): BudgetReceiptLines {
  const { runs_used: used, runs_limit: limit, runs_limit_after: limitAfter } = receipt;
  const outcome = outcomeLine(receipt.outcome, t);
  // An unknown top-level token: plain "Applied.", no numbers, no outcome line (CHE-6943 D4).
  const numbers = outcome !== undefined && typeof used === "number" && typeof limit === "number" && typeof limitAfter === "number";
  const usedAfter = receipt.new_window ? 0 : used;
  return {
    effect: numbers && issue ? t(($) => $.budget_receipt.applied_runs, { issue, used: usedAfter, limit: limitAfter }) : null,
    runs: numbers
      ? {
          label: t(($) => $.budget_receipt.runs_used),
          change: receipt.new_window
            ? t(($) => $.budget_receipt.change_new_window, { used, limit, usedAfter, limitAfter })
            : t(($) => $.budget_receipt.change, { used, limit, usedAfter, limitAfter }),
        }
      : null,
    outcome: outcome ?? null,
  };
}

/** The outcome line; null for `cap_applied` (no extra line); undefined for a token this client does not know. */
function outcomeLine(outcome: string, t: T): string | null | undefined {
  const at = outcome.indexOf(":");
  const [token, reason] = at < 0 ? [outcome, ""] : [outcome.slice(0, at), outcome.slice(at + 1)];
  switch (token) {
    case "cap_applied": return null;
    case "run_queued": return t(($) => $.budget_receipt.outcome.run_queued);
    case "run_started": return t(($) => $.budget_receipt.outcome.run_started);
    case "status_restored": return t(($) => $.budget_receipt.outcome.status_restored);
    case "still_blocked":
      return reason === "runs" ? t(($) => $.budget_receipt.outcome.still_blocked_runs)
        : reason === "tokens" ? t(($) => $.budget_receipt.outcome.still_blocked_tokens)
        : t(($) => $.budget_receipt.outcome.still_blocked);
    case "superseded":
      switch (reason) {
        case "later_window": return t(($) => $.budget_receipt.outcome.later_window);
        case "status_moved": return t(($) => $.budget_receipt.outcome.status_moved);
        case "window_changed": return t(($) => $.budget_receipt.outcome.window_changed);
        case "issue_closed": return t(($) => $.budget_receipt.outcome.issue_closed);
        case "agent_archived": return t(($) => $.budget_receipt.outcome.agent_archived);
        case "restore_refused": return t(($) => $.budget_receipt.outcome.restore_refused);
        case "unknown_origin": return t(($) => $.budget_receipt.outcome.unknown_origin);
        case "assignee_changed": return t(($) => $.budget_receipt.outcome.assignee_changed);
        case "source_unknown": return t(($) => $.budget_receipt.outcome.source_unknown);
        case "source_not_delivered": return t(($) => $.budget_receipt.outcome.source_not_delivered);
        default: return t(($) => $.budget_receipt.outcome.superseded);
      }
    default: return undefined;
  }
}
