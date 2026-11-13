"use client";

import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { chatKeys } from "@multica/core/chat/queries";
import type { ChatSupplement } from "@multica/core/types";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../../i18n";

/**
 * Delivery state of a follow-up steered into the running reply (CHE-2820).
 * `uncertain` is never worded as "not received" and never resends without the
 * user accepting the duplicate warning: the agent may already have the message.
 */
export function ChatSupplementStatus({ supplement }: { supplement: ChatSupplement }) {
  const { t } = useT("chat");
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);

  const resend = async (confirmPossibleDuplicate: boolean) => {
    setBusy(true);
    try {
      await api.resendChatSupplement(supplement.chat_session_id, supplement.message_id, {
        confirmPossibleDuplicate,
      });
    } catch {
      toast.error(t(($) => $.supplement.resend_failed_toast));
    } finally {
      setBusy(false);
      setConfirmOpen(false);
      qc.invalidateQueries({ queryKey: chatKeys.messages(supplement.chat_session_id) });
      qc.invalidateQueries({ queryKey: chatKeys.messagesPage(supplement.chat_session_id) });
      qc.invalidateQueries({ queryKey: chatKeys.pendingTask(supplement.chat_session_id) });
    }
  };

  const resent = supplement.resent_task_id !== null;
  let label: string;
  let action: "none" | "resend" | "confirm" = "none";
  switch (supplement.status) {
    case "pending":
    case "delivering":
      label = t(($) => $.supplement.added);
      break;
    case "delivered":
      label = supplement.acked_after_resend_at
        ? t(($) => $.supplement.read_twice)
        : t(($) => $.supplement.read);
      break;
    case "failed":
      label = t(($) => $.supplement.not_received);
      action = resent ? "none" : "resend";
      break;
    default:
      label = t(($) => $.supplement.unconfirmed);
      action = resent ? "none" : "confirm";
  }

  return (
    <div
      data-testid="chat-supplement-status"
      className="mt-1 flex flex-wrap items-center justify-end gap-x-2 text-caption text-muted-foreground"
    >
      <span>{label}</span>
      {resent && (supplement.status === "failed" || supplement.status === "uncertain") ? (
        <span>{t(($) => $.supplement.resent)}</span>
      ) : null}
      {action === "resend" ? (
        <Button variant="link" size="xs" disabled={busy} onClick={() => void resend(false)}>
          {t(($) => $.supplement.send_new_run)}
        </Button>
      ) : null}
      {action === "confirm" ? (
        <>
          <Button variant="link" size="xs" disabled={busy} onClick={() => setConfirmOpen(true)}>
            {t(($) => $.supplement.send_again_anyway)}
          </Button>
          <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
            <AlertDialogContent data-testid="chat-supplement-resend-dialog">
              <AlertDialogHeader>
                <AlertDialogTitle>{t(($) => $.supplement.confirm_title)}</AlertDialogTitle>
                <AlertDialogDescription>
                  {t(($) => $.supplement.confirm_body)}
                </AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel>{t(($) => $.supplement.cancel)}</AlertDialogCancel>
                <Button disabled={busy} onClick={() => void resend(true)}>
                  {t(($) => $.supplement.confirm_send)}
                </Button>
              </AlertDialogFooter>
            </AlertDialogContent>
          </AlertDialog>
        </>
      ) : null}
    </div>
  );
}
