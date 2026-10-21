"use client";

import { useCallback, useRef, useState, type ReactNode } from "react";
import { Button } from "@multica/ui/components/ui/button";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import type { ChatSession } from "@multica/core/types";
import { useT } from "../../i18n";

type SendResult = void | boolean | Promise<void | boolean>;

interface ArchivedSendOptions<Args extends unknown[]> {
  /** The open DM. Only an archived one is intercepted. */
  session: ChatSession | null | undefined;
  /** Resolves the continuation target's title and opens it. */
  sessions: ChatSession[];
  send: (...args: Args) => SendResult;
  unarchive: (session: ChatSession) => Promise<boolean>;
  onOpenSession: (session: ChatSession) => void;
}

/**
 * A DM that was archived still has a composer. Sending from it asks first:
 * confirming unarchives the DM, then sends the held message exactly once.
 * Cancel and a failed unarchive send nothing and leave the draft where it was,
 * because the held send never reaches `commitInput`. A DM that was continued
 * offers its newer DM as the primary action.
 */
export function useArchivedSend<Args extends unknown[]>({
  session,
  sessions,
  send,
  unarchive,
  onOpenSession,
}: ArchivedSendOptions<Args>): { guardedSend: (...args: Args) => SendResult; dialog: ReactNode } {
  const { t } = useT("chat");
  const [held, setHeld] = useState<{ session: ChatSession; args: Args } | null>(null);
  const confirming = useRef(false);

  const guardedSend = useCallback(
    (...args: Args): SendResult => {
      if (session?.status !== "archived") return send(...args);
      setHeld({ session, args });
      // The message was not sent; the composer keeps it for the retry.
      return false;
    },
    [send, session],
  );

  const confirm = async () => {
    if (!held || confirming.current) return;
    confirming.current = true;
    const { session: target, args } = held;
    setHeld(null);
    try {
      if (!(await unarchive(target))) return;
      await send(...args);
    } finally {
      confirming.current = false;
    }
  };

  const newer = held?.session.continued_into_session_id
    ? sessions.find((s) => s.id === held.session.continued_into_session_id) ?? null
    : null;

  const dialog = (
    <AlertDialog open={!!held} onOpenChange={(open) => !open && setHeld(null)}>
      <AlertDialogContent data-testid="chat-archived-send-dialog">
        <AlertDialogHeader>
          <AlertDialogTitle>{t(($) => $.archived_send.title)}</AlertDialogTitle>
          <AlertDialogDescription>
            {t(($) => $.archived_send.question)}{" "}
            {newer
              ? t(($) => $.archived_send.newer, {
                  title: newer.title?.trim() || t(($) => $.window.untitled),
                })
              : t(($) => $.archived_send.kept)}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>{t(($) => $.archived_send.cancel)}</AlertDialogCancel>
          <Button
            variant={newer ? "outline" : "default"}
            data-primary={newer ? undefined : "true"}
            data-testid="chat-archived-send-confirm"
            onClick={() => void confirm()}
          >
            {t(($) => $.archived_send.confirm)}
          </Button>
          {newer ? (
            <Button
              data-primary="true"
              data-testid="chat-open-newer-dm"
              onClick={() => {
                setHeld(null);
                onOpenSession(newer);
              }}
            >
              {t(($) => $.archived_send.open_newer)}
            </Button>
          ) : null}
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );

  return { guardedSend, dialog };
}
