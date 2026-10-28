import { useCallback, useState } from "react";
import { toast } from "sonner";

/**
 * One-shot "open this chat at that message" for a surface with no URL of its
 * own (the floating window). The target only applies while its session is the
 * open one, and clears once the list reports where it landed.
 */
export function useChatMessageJump(activeSessionId: string | null, missingMessage: string) {
  const [target, setTarget] = useState<{ sessionId: string; messageId: string } | null>(null);

  const request = useCallback((sessionId: string, messageId: string) => {
    setTarget({ sessionId, messageId });
  }, []);

  const settle = useCallback(
    (outcome: "landed" | "missing") => {
      if (outcome === "missing") toast.error(missingMessage);
      setTarget(null);
    },
    [missingMessage],
  );

  return {
    jumpToMessageId: target && target.sessionId === activeSessionId ? target.messageId : null,
    request,
    settle,
  };
}
