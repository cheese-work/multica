"use client";

import { lazy, Suspense, useEffect, useState } from "react";
import { useChatStore } from "@multica/core/chat";
import { useWorkspacePaths } from "@multica/core/paths";
import { useNavigation } from "../navigation";
import { ChatFab } from "./components/chat-fab";
import { isFloatingChatRouteSuppressed } from "./floating-chat-visibility";

// The window pulls in the chat composer (editor) and message list (markdown).
// Loading it after first paint keeps those chunks off the page's critical path.
const ChatWindow = lazy(() =>
  import("./components/chat-window").then((m) => ({ default: m.ChatWindow })),
);

function useAfterIdle() {
  const [idle, setIdle] = useState(false);
  useEffect(() => {
    if (typeof window.requestIdleCallback !== "function") {
      const id = window.setTimeout(() => setIdle(true), 2000);
      return () => window.clearTimeout(id);
    }
    const id = window.requestIdleCallback(() => setIdle(true), { timeout: 5000 });
    return () => window.cancelIdleCallback(id);
  }, []);
  return idle;
}

/**
 * Mount point for the floating chat overlay (FAB + window). Rendered once in
 * each app shell's dashboard layout; owns the two gates that decide whether the
 * overlay exists at all:
 *
 *  1. The Settings → Chat preference (`floatingChatEnabled`). When a user turns
 *     the floating window off, Chat lives only in its dedicated tab.
 *  2. The Chat tab route itself. On `/:slug/chat` the full-page surface already
 *     owns the conversation, so a floating copy of the same `activeSessionId`
 *     would be pure duplication — hide it there.
 */
export function FloatingChat() {
  const enabled = useChatStore((s) => s.floatingChatEnabled);
  const { pathname } = useNavigation();
  const isOpen = useChatStore((s) => s.isOpen);
  const wsPaths = useWorkspacePaths();
  const idle = useAfterIdle();

  if (!enabled) return null;
  // Suppress on the Chat tab — it renders the same conversation full-page.
  if (isFloatingChatRouteSuppressed(pathname, wsPaths.chat())) return null;

  return (
    <>
      {(idle || isOpen) && (
        <Suspense fallback={null}>
          <ChatWindow />
        </Suspense>
      )}
      <ChatFab />
    </>
  );
}
