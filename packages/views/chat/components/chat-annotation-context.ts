import { createContext, useContext } from "react";

/** Lets a settled agent reply opt in to text annotation without prop drilling through the list. */
export interface ChatAnnotationScope {
  enabled: boolean;
  agentName?: string;
}

export const ChatAnnotationContext = createContext<ChatAnnotationScope>({ enabled: false });

export const useChatAnnotationScope = () => useContext(ChatAnnotationContext);

export const DECISION_REQUEST_SOURCE_PREFIX = "decision-request:";
