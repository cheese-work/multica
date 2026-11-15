import { createKeyedDraftStore } from "../drafts/create-keyed-draft-store";

/**
 * Unsent input on a pending app-request card: the edited action arguments (JSON
 * text) and the rejection reason. Keyed by request id. Kept until the request is
 * decided; a request nobody decides ages out with the default draft TTL.
 */
export interface AppRequestDraft {
  editing: boolean;
  args: string;
  rejecting: boolean;
  reason: string;
}

export const EMPTY_APP_REQUEST_DRAFT: AppRequestDraft = {
  editing: false,
  args: "",
  rejecting: false,
  reason: "",
};

export const useAppRequestDraftStore = createKeyedDraftStore<AppRequestDraft>({
  storageKey: "multica_app_request_drafts",
  emptyData: EMPTY_APP_REQUEST_DRAFT,
  hasMeaningful: (d) => d.editing || d.rejecting || d.reason.trim() !== "",
});
