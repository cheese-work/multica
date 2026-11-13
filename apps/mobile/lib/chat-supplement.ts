import type { ChatSupplement } from "@multica/core/types";

export type SupplementAction = "none" | "resend" | "confirm_resend";

export interface SupplementPresentation {
  /** Key in the `chat` namespace. */
  labelKey: string;
  action: SupplementAction;
  /** The message already went out again as a new run. */
  resent: boolean;
}

/**
 * Bubble state of a steered DM follow-up. `uncertain` is never worded as
 * "not received" and never resends without the user accepting the duplicate
 * warning: the agent may already have the message. Mirrors
 * packages/views/chat/components/chat-supplement-status.tsx.
 */
export function presentSupplement(supplement: ChatSupplement): SupplementPresentation {
  const resent = supplement.resent_task_id !== null;
  switch (supplement.status) {
    case "pending":
    case "delivering":
      return { labelKey: "supplement.added", action: "none", resent: false };
    case "delivered":
      return {
        labelKey: supplement.acked_after_resend_at ? "supplement.read_twice" : "supplement.read",
        action: "none",
        resent: false,
      };
    case "failed":
      return { labelKey: "supplement.not_received", action: resent ? "none" : "resend", resent };
    case "uncertain":
      return { labelKey: "supplement.unconfirmed", action: resent ? "none" : "confirm_resend", resent };
  }
}
