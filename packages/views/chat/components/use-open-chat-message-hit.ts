import { useCallback } from "react";
import { useWorkspacePaths } from "@multica/core/paths";
import { useNavigation } from "../../navigation";

/**
 * Open a message search hit from the floating window. The window has no URL of
 * its own, so the hit pushes the same `?session=&message=` entry the full page
 * and decision cards use: the page scrolls to the message and Back returns to
 * where the search began. The floating window hides itself on that route.
 */
export function useOpenChatMessageHit() {
  const { push } = useNavigation();
  const chatPath = useWorkspacePaths().chat();
  return useCallback(
    (sessionId: string, messageId: string) =>
      push(`${chatPath}?session=${sessionId}&message=${messageId}`),
    [push, chatPath],
  );
}
