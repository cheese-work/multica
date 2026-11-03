import { useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api";
import { propertyListOptions } from "@multica/core/properties/queries";
import type { IssueWakeupInput } from "@multica/core/types";
import { useViewingTimezone } from "../../common/use-viewing-timezone";
import { useT } from "../../i18n";
import { useWakeupText } from "./wakeup-presentation";
import { buildWakeupInput, emptyWakeupDraft, type WakeupDraft, type WakeupDraftError } from "./wakeup-draft";

/**
 * Draft state plus the one place a draft becomes a saved rule: validate, build
 * the payload, hand it to `save`, and turn a refusal into a message. Where the
 * rule is stored is the caller's `save`.
 */
export function useWakeupForm({
  workspaceId,
  defaultAgentId,
  save,
  onSaved,
  onBusy,
}: {
  workspaceId: string;
  defaultAgentId: string;
  save: (input: IssueWakeupInput) => Promise<unknown>;
  onSaved: () => void;
  onBusy?: (busy: boolean) => void;
}) {
  const { t } = useT("issues");
  const text = useWakeupText();
  const timezone = useViewingTimezone();
  const [draft, setDraft] = useState(() => emptyWakeupDraft(defaultAgentId, timezone));
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const inFlight = useRef(false);
  const { data: properties = [] } = useQuery(propertyListOptions(workspaceId));

  const update = (patch: Partial<WakeupDraft>) => {
    setDraft((prev) => ({ ...prev, ...patch }));
    setError("");
  };
  const draftErrors: Record<WakeupDraftError, string> = {
    missing_condition: t(($) => $.wakeups.create.missing_condition),
    missing_value: t(($) => $.wakeups.create.missing_value),
    missing_issue: t(($) => $.wakeups.create.missing_issue),
    missing_agent: t(($) => $.wakeups.create.missing_agent),
    missing_events: t(($) => $.wakeups.create.missing_events),
    instruction_invalid: t(($) => $.wakeups.instruction_invalid),
    future_time: t(($) => $.wakeups.future_time),
    until_future: t(($) => $.wakeups.create.until_future),
  };

  const submit = async () => {
    if (inFlight.current) return;
    const propertyType =
      draft.field === "property" ? properties.find((p) => p.id === draft.fieldTarget)?.type : undefined;
    const result = buildWakeupInput(draft, new Date(), propertyType);
    if ("error" in result) {
      setError(draftErrors[result.error]);
      return;
    }
    inFlight.current = true;
    setPending(true);
    onBusy?.(true);
    try {
      await save(result.input);
      onSaved();
    } catch (err) {
      const code =
        err instanceof ApiError && err.body && typeof err.body === "object" && "code" in err.body
          ? err.body.code
          : undefined;
      setError(
        code === "wakeup_capacity_exceeded"
          ? t(($) => $.wakeups.create.capacity_error)
          : text.error(err, t(($) => $.wakeups.create.error)),
      );
    } finally {
      inFlight.current = false;
      setPending(false);
      onBusy?.(false);
    }
  };

  return { draft, update, error, pending, submit };
}
