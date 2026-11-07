import { useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api";
import { useWakeupFormDraftStore } from "@multica/core/issues/stores";
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
  draftKey,
  initial,
  save,
  onSaved,
  onBusy,
  describeError,
}: {
  workspaceId: string;
  defaultAgentId: string;
  /**
   * Persist the member's edits under this key until a save succeeds, so the
   * unsent form comes back after the host unmounts. Unset: nothing persisted.
   */
  draftKey?: string;
  /** Draft fields to open with, for editing a saved rule. */
  initial?: Partial<WakeupDraft>;
  save: (input: IssueWakeupInput) => Promise<unknown>;
  onSaved: () => void;
  onBusy?: (busy: boolean) => void;
  /** A message for a refusal the caller knows; anything else gets the shared one. */
  describeError?: (err: unknown) => string | undefined;
}) {
  const { t } = useT("issues");
  const text = useWakeupText();
  const timezone = useViewingTimezone();
  const [draft, setDraft] = useState<WakeupDraft>(() => ({
    ...emptyWakeupDraft(defaultAgentId, timezone),
    ...initial,
    ...(draftKey ? (useWakeupFormDraftStore.getState().getDraft(draftKey)?.fields as Partial<WakeupDraft> | null | undefined) : null),
  }));
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const inFlight = useRef(false);
  const { data: properties = [] } = useQuery(propertyListOptions(workspaceId));

  const update = (patch: Partial<WakeupDraft>) => {
    setDraft((prev) => ({ ...prev, ...patch }));
    if (draftKey) {
      const store = useWakeupFormDraftStore.getState();
      store.setDraft(draftKey, { fields: { ...store.getDraft(draftKey)?.fields, ...patch } });
    }
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

  /** The payload save sends: a property condition's value is typed by the property. */
  const build = () => {
    const propertyType =
      draft.field === "property" ? properties.find((p) => p.id === draft.fieldTarget)?.type : undefined;
    return buildWakeupInput(draft, new Date(), propertyType);
  };

  const submit = async () => {
    if (inFlight.current) return;
    const result = build();
    if ("error" in result) {
      setError(draftErrors[result.error]);
      return;
    }
    inFlight.current = true;
    setPending(true);
    onBusy?.(true);
    try {
      await save(result.input);
      if (draftKey) useWakeupFormDraftStore.getState().clearDraft(draftKey);
      onSaved();
    } catch (err) {
      const code =
        err instanceof ApiError && err.body && typeof err.body === "object" && "code" in err.body
          ? err.body.code
          : undefined;
      setError(
        describeError?.(err) ??
          (code === "wakeup_capacity_exceeded"
            ? t(($) => $.wakeups.create.capacity_error)
            : text.error(err, t(($) => $.wakeups.create.error))),
      );
    } finally {
      inFlight.current = false;
      setPending(false);
      onBusy?.(false);
    }
  };

  return { draft, update, error, pending, submit, build };
}
