import type { ReactNode } from "react";
import { useT } from "../../i18n";
import { useChatAnnotationScope } from "./chat-annotation-context";

export function AnnotatableAnswer({ messageId, children, readOnly = false }: { messageId?: string; children: ReactNode; readOnly?: boolean }) {
  const { t } = useT("chat");
  const { enabled, agentName } = useChatAnnotationScope();
  if (!enabled) return <>{children}</>;
  return (
    <div data-annotation-overlay-host>
      <div
        data-comment-content={readOnly ? undefined : messageId}
        tabIndex={messageId ? 0 : undefined}
        role={messageId ? "group" : undefined}
        aria-label={messageId ? t(($) => $.annotations.source_label, { name: agentName ?? "" }) : undefined}
        className="rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-brand"
      >
        {children}
      </div>
      <div data-annotation-overlay-slot />
    </div>
  );
}
