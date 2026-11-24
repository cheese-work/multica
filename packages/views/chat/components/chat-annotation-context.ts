import { createContext, useContext } from "react";

/** Lets a settled agent reply opt in to text annotation without prop drilling through the list. */
export interface ChatAnnotationScope {
  enabled: boolean;
  agentName?: string;
  /** An earlier DM's message loads after the hook renders; it joins the commentable sources here. */
  registerSource?: (id: string, name: string) => void;
  /** A saved comment's source row may be unmounted (virtualized or in an older page): the list brings this message into view. */
  revealMessageId?: string | null;
  /** The list could not bring `revealMessageId` into view. */
  onRevealMissing?: () => void;
  /** An annotatable source mounted; a pending reveal waiting on it can open its editor. */
  onSourceMounted?: (sourceId: string) => void;
}

export const ChatAnnotationContext = createContext<ChatAnnotationScope>({ enabled: false });

export const useChatAnnotationScope = () => useContext(ChatAnnotationContext);

export const DECISION_REQUEST_SOURCE_PREFIX = "decision-request:";
