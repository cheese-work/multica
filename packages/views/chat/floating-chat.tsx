"use client";

import { lazy, Suspense } from "react";
import { useChatStore } from "@multica/core/chat";
import { useWorkspacePaths } from "@multica/core/paths";
import { useNavigation } from "../navigation";
import { ChatFab } from "./components/chat-fab";
import { isFloatingChatRouteSuppressed } from "./floating-chat-visibility";

// The window pulls in the markdown/editor graph; the FAB alone is what the
// first paint needs (CHE-3019).
const ChatWindow = lazy(() => import("./components/chat-window").then((m) => ({ default: m.ChatWindow })));

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
  const wsPaths = useWorkspacePaths();

  if (!enabled) return null;
  // Suppress on the Chat tab — it renders the same conversation full-page.
  if (isFloatingChatRouteSuppressed(pathname, wsPaths.chat())) return null;

  return (
    <>
      <Suspense fallback={null}>
        <ChatWindow />
      </Suspense>
      <ChatFab />
    </>
  );
}
