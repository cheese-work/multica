import { createKeyedDraftStore } from "../../drafts/create-keyed-draft-store";
import type {
  AutopilotAssigneeType,
  AutopilotExecutionMode,
  WebhookEventFilter,
} from "../../types";

/** A schedule as its cron string plus IANA zone; the views parse it back. */
export interface ScheduleDraft {
  cron: string;
  timezone: string;
}

/**
 * Overlay on the dialog's initial values: a field is `undefined` until the user
 * touches it, so an edit-mode draft never masks a server value they did not
 * change, and a create-from-template draft never replaces the template's.
 */
export interface AutopilotDialogDraft {
  title?: string;
  description?: string;
  projectId?: string | null;
  assigneeType?: AutopilotAssigneeType;
  assigneeId?: string;
  executionMode?: AutopilotExecutionMode;
  subscriberUserIds?: string[];
  triggerKind?: "schedule" | "webhook";
  schedule?: ScheduleDraft;
  scheduleAdded?: boolean;
  eventFilters?: WebhookEventFilter[];
  /**
   * Set once a create has made the autopilot but its trigger write failed, so a
   * retry (even after a reload) updates that autopilot instead of creating a
   * duplicate.
   */
  createdAutopilotId?: string;
}

/** One draft per dialog target: `create`, `create:<template>` or `edit:<id>`. */
export const useAutopilotDialogDraftStore = createKeyedDraftStore<AutopilotDialogDraft>({
  storageKey: "multica_autopilot_dialog_drafts",
  emptyData: {},
  hasMeaningful: (d) => Object.values(d).some((v) => v !== undefined),
});

export interface AutopilotTriggerDraft {
  kind?: "schedule" | "webhook";
  schedule?: ScheduleDraft;
  label?: string;
}

/** Add-trigger dialog on the autopilot detail page, keyed by autopilot id. */
export const useAutopilotTriggerDraftStore = createKeyedDraftStore<AutopilotTriggerDraft>({
  storageKey: "multica_autopilot_trigger_drafts",
  emptyData: {},
  hasMeaningful: (d) => Object.values(d).some((v) => v !== undefined),
});
