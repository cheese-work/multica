import { createKeyedDraftStore } from "../drafts/create-keyed-draft-store";

/** The page a chat draft was attached to, as it was when attached. */
export interface ChatPageContext {
  id: string;
  label: string;
  type: "member" | "agent" | "squad" | "issue" | "project" | "all";
  description?: string;
}

export interface PageContextDraft {
  page: ChatPageContext | null;
}

export const EMPTY_PAGE_CONTEXT: PageContextDraft = { page: null };

/** Key: chat draft key. The attached page stays with its draft until sent or removed. */
export const usePageContextDraftStore = createKeyedDraftStore<PageContextDraft>({
  storageKey: "multica_chat_page_context_drafts",
  emptyData: EMPTY_PAGE_CONTEXT,
  hasMeaningful: (d) => d.page !== null,
});
