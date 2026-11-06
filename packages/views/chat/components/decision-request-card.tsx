import { Suspense, useEffect, useId, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Loader2, MessageSquareQuote } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import { ApiError } from "@multica/core/api";
import { useAnswerDecisionRequest } from "@multica/core/issues/decision-answer";
import {
  type DecisionAnswerDraft as AnswerDraft,
  EMPTY_DECISION_ANSWER_DRAFT as EMPTY_DRAFT,
  appendDecisionAttachment,
  decisionAnswerDraftKey,
  useDecisionAnswerDraftStore,
} from "@multica/core/issues/stores/decision-answer-draft-store";
import {
  beginPendingUpload,
  invalidatePendingUploads,
  settlePendingUpload,
  usePendingUploadCount,
  usePersistedDraft,
  useUploadCancelledNotice,
} from "@multica/core/drafts";
import { chatKeys } from "@multica/core/chat/queries";
import { useWorkspacePaths } from "@multica/core/paths";
import { formatShortcut, useShortcut } from "@multica/core/shortcuts";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import type { ContentEditorRef } from "../../editor/content-editor";
import { attachmentMarkdown } from "../../editor/use-coordinated-uploads";
import { useEditorUpload } from "../../editor/use-editor-upload";
import { useUploadGate } from "../../editor/use-upload-gate";
import { LazyContentEditor as ContentEditor } from "./lazy-content-editor";
import { RichContent } from "../../rich-content";
import { CompactIssueChipsProvider } from "../../issues/compact-issue-chips-context";
import { useT } from "../../i18n";
import { AppLink } from "../../navigation";
import { useOnline } from "../../settings/components/use-online";
import { AnnotatableAnswer } from "./annotatable-answer";
import { DecisionArtifacts } from "./decision-artifacts";
import { DECISION_REQUEST_SOURCE_PREFIX } from "./chat-annotation-context";

// Mirrors the server's `maxDecisionCardTextBytes`; it counts the trimmed text, image links included.
const MAX_ANSWER_BYTES = 8 << 10;
const answerBytes = (text: string) => new TextEncoder().encode(text.trim()).length;

const isConflict = (error: unknown) => error instanceof ApiError && error.status === 409;

const hasDraft = (draft: AnswerDraft) => draft.option !== "" || draft.text.trim() !== "";

type AnswerMutation = ReturnType<typeof useAnswerDecisionRequest>;

// The parent owns the draft (persisted per request, so it outlives an unmount) and the send, so a card that turns answered under
// the reader (another tab, the DM, our own realtime event arriving before the
// HTTP 201) does not unmount the form and drop the draft or the send callbacks.
function DecisionAnswerForm({
  card,
  draft,
  answer,
  onDraftChange,
  onSend,
  inIssue,
}: {
  card: RequestCard;
  draft: AnswerDraft;
  answer: AnswerMutation;
  onDraftChange: (patch: Partial<AnswerDraft>) => void;
  onSend: (text: string) => void;
  inIssue: boolean;
}) {
  const { t } = useT("chat");
  const online = useOnline();
  const sendShortcut = formatShortcut(useShortcut("send"));
  const { option, text } = draft;
  const draftKey = decisionAnswerDraftKey(card.id);
  // What this editor last reported. A stored text that differs came from the other surface
  // showing the same request, and is adopted so a send from here cannot overwrite it.
  const emitted = useRef(text);
  // The editor's unmount flush (the last note edit) is applied. What it reports after that is an
  // upload settling; `uploadFile` below owns that, so it is dropped here. `closing` is set by this
  // cleanup, which runs before the editor's, so the flush is not mistaken for a late report; the
  // microtask flips `closed` before any async report from the editor can arrive.
  const closing = useRef(false);
  const closed = useRef(false);
  useEffect(() => {
    closing.current = closed.current = false;
    return () => {
      closing.current = true;
      queueMicrotask(() => { closed.current = true; });
    };
  }, []);
  const onEditorUpdate = (next: string) => { if (!closed.current) setText(next); };
  const setOption = (next: string) => onDraftChange({ option: next });
  const setText = (next: string) => {
    emitted.current = next;
    onDraftChange({ text: next });
  };
  const group = useId();
  const textId = useId();
  const editorRef = useRef<ContentEditorRef>(null);
  const uploadGate = useUploadGate(editorRef);
  const { uploadWithToast } = useEditorUpload();
  const uploadsPending = usePendingUploadCount(draftKey) > 0;
  const uploadCancelled = useUploadCancelledNotice(draftKey);
  // The upload outlives this card. If the card closed first, the attachment goes straight into
  // the stored note (append only), unless the draft was sent, discarded or logged out meanwhile.
  const uploadFile = async (file: File) => {
    const ticket = beginPendingUpload(draftKey);
    let result: Awaited<ReturnType<typeof uploadWithToast>> = null;
    try {
      result = await uploadWithToast(file, { issueId: card.issue_id, signal: ticket.signal });
      return result;
    } finally {
      settlePendingUpload(ticket, () => {
        if (result && closing.current) appendDecisionAttachment(ticket.origin, card.id, attachmentMarkdown(result));
      });
    }
  };
  useEffect(() => {
    // Read the store, not the render's `text`: a render can lag behind a newer edit.
    const stored = useDecisionAnswerDraftStore.getState().getDraft(draftKey)?.text ?? "";
    const editor = editorRef.current;
    if (stored === emitted.current || !editor || editor.hasActiveUploads()) return;
    emitted.current = stored;
    editor.adoptContent(stored);
  }, [text, draftKey]);
  const tooLong = answerBytes(text) > MAX_ANSWER_BYTES;
  const canSend = online && !answer.isPending && !uploadGate.uploading && !uploadsPending && !tooLong && (option !== "" || text.trim() !== "");
  const send = () => {
    // onUpdate reaches `text` through a timer, so an Enter or click right after the last edit
    // (or a finished upload) would send stale text. Read the editor and send exactly that.
    const live = editorRef.current?.getMarkdown() ?? text;
    // Enter can arrive while an upload placeholder is still in the document.
    const sendable = online && !answer.isPending && !uploadGate.uploading && !uploadsPending && !uploadGate.isBlocked()
      && answerBytes(live) <= MAX_ANSWER_BYTES && (option !== "" || live.trim() !== "");
    if (!sendable) return;
    emitted.current = live;
    onSend(live);
  };

  return (
    <form
      data-testid="decision-request-answer"
      className="space-y-2"
      onSubmit={(event) => {
        event.preventDefault();
        send();
      }}
    >
      <p className="text-caption text-muted-foreground">
        {t(($) => (inIssue ? $.decision.answer_helper_issue : $.decision.answer_helper), { issue: card.issue_identifier || card.issue_id })}
      </p>
      {!!card.options?.length && (
        <fieldset role="radiogroup" className="space-y-1">
          <legend className="sr-only">{t(($) => $.decision.answer_options)}</legend>
          {card.options.map((item) => (
            <label
              key={item.label}
              className={cn(
                "flex min-h-9 cursor-pointer items-start gap-2 rounded-lg border border-surface-border px-2 py-1.5 has-[:focus-visible]:outline-2 has-[:focus-visible]:outline-offset-2 has-[:focus-visible]:outline-ring",
                option === item.label && "border-primary bg-primary/5",
              )}
            >
              <input
                type="radio"
                name={group}
                value={item.label}
                checked={option === item.label}
                onChange={() => setOption(item.label)}
                className="mt-1"
              />
              <span className="min-w-0">
                <span className="font-medium">{item.label}</span>
                {item.what_happens && <span className="block text-caption text-muted-foreground">{item.what_happens}</span>}
              </span>
            </label>
          ))}
        </fieldset>
      )}
      <span className="block text-label font-medium">
        {card.options?.length ? t(($) => $.decision.answer_note) : t(($) => $.decision.answer_text)}
      </span>
      <div className="rounded-lg border border-surface-border px-2 py-1.5 focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-ring">
        <Suspense fallback={null}>
          <ContentEditor
            ref={editorRef}
            defaultValue={text}
            ariaLabel={card.options?.length ? t(($) => $.decision.answer_note) : t(($) => $.decision.answer_text)}
            ariaDescribedBy={`${textId}-hint`}
            onUpdate={onEditorUpdate}
            onSubmit={send}
            onUploadFile={uploadFile}
            onUploadingChange={uploadGate.onUploadingChange}
            debounceMs={0}
            flushPendingOnUnmount
            showBubbleMenu={false}
            className="max-h-60 overflow-y-auto"
          />
        </Suspense>
      </div>
      <p id={`${textId}-hint`} className="text-caption text-muted-foreground">
        {t(($) => $.decision.answer_hint, { shortcut: sendShortcut })}
      </p>
      {uploadCancelled && <p role="status" className="text-caption text-destructive">{t(($) => $.decision.answer_upload_cancelled)}</p>}
      {tooLong && <p role="alert" className="text-caption text-destructive">{t(($) => $.decision.answer_too_long)}</p>}
      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="sm" disabled={!canSend} className="disabled:opacity-70">
          {answer.isPending && <Loader2 className="size-4 animate-spin" aria-hidden />}
          {t(($) => $.decision.answer_send)}
        </Button>
        {!online && <span role="status" className="text-caption text-warning">{t(($) => $.decision.answer_offline)}</span>}
        {answer.isError && !isConflict(answer.error) && (
          <span role="alert" className="text-caption text-destructive">{t(($) => $.decision.answer_error)}</span>
        )}
      </div>
    </form>
  );
}

/** The card can no longer take the answer (a 409, or it turned answered first): the draft is shown back, not sent. */
function KeptDraft({ draft, onDiscard }: { draft: AnswerDraft; onDiscard: () => void }) {
  const { t } = useT("chat");
  return (
    <div data-testid="decision-request-kept-draft" role="status" className="space-y-2 rounded-lg border border-surface-border px-2 py-2">
      <p className="text-label font-medium">{t(($) => $.decision.answer_conflict)}</p>
      <p className="whitespace-pre-wrap break-words text-caption text-muted-foreground">
        {[draft.option, draft.text].filter(Boolean).join("\n\n")}
      </p>
      <Button type="button" size="sm" variant="outline" onClick={onDiscard}>
        {t(($) => $.decision.answer_discard)}
      </Button>
    </div>
  );
}

// An answer with no run this long after the record shows Not picked up.
const PICKUP_GRACE_MS = 2 * 60 * 1000;

// Open → Recorded → Delivered → Picked up (or Not picked up after the grace),
// or Withdrawn / Disclaimed. A disclaimed answer can leave the request open
// again, so the receipt wins over the request state.
function displayState(card: RequestCard, now: number) {
  if (card.state === "withdrawn") return "withdrawn";
  if (card.answer?.state === "disclaimed") return "disclaimed";
  if (card.state !== "answered") return "open";
  const answer = card.answer;
  if (!answer) return "recorded";
  if (answer.state !== "picked_up" && now >= new Date(answer.answered_at).getTime() + PICKUP_GRACE_MS) return "not_picked_up";
  return answer.state;
}

// Re-renders once when the pickup grace runs out, and asks for fresh data then:
// a run that started in the meantime has no push of its own.
function useNow(card: RequestCard) {
  const qc = useQueryClient();
  const [now, setNow] = useState(() => Date.now());
  const waiting = card.state === "answered" && !!card.answer && card.answer.state !== "picked_up" && card.answer.state !== "disclaimed";
  const deadline = waiting ? new Date(card.answer!.answered_at).getTime() + PICKUP_GRACE_MS : NaN;
  useEffect(() => {
    if (!(deadline > Date.now())) {
      // A receipt that arrives after its grace: the clock kept from mount is stale.
      if (deadline <= Date.now()) setNow(Date.now());
      return;
    }
    const timer = setTimeout(() => {
      qc.invalidateQueries({ queryKey: chatKeys.messagesAll() });
      qc.invalidateQueries({ queryKey: chatKeys.messagesPageAll() });
      setNow(Date.now());
    }, deadline - Date.now());
    return () => clearTimeout(timer);
  }, [deadline, qc]);
  return now;
}

// `inIssue`: shown on the source issue page, where there is no DM message box.
export function DecisionRequestCard({ card, inIssue = false }: { card: RequestCard; inIssue?: boolean }) {
  const { t } = useT("chat");
  const paths = useWorkspacePaths();
  const shown = displayState(card, useNow(card));
  const state = t(($) => $.decision.request_state[shown]);
  const timestamp = card.state === "withdrawn" ? card.withdrawn_at || card.created_at : card.created_at;
  const date = new Date(timestamp);
  const validDate = !Number.isNaN(date.getTime());
  const issueHref = paths.issueDetail(card.issue_id);
  const [draft, setDraft, clearDraft] = usePersistedDraft(useDecisionAnswerDraftStore, decisionAnswerDraftKey(card.id), EMPTY_DRAFT);
  const [refused, setRefused] = useState(false);
  // Remounting the form empties the editor after a send: the editor takes its draft once at mount.
  const [formKey, setFormKey] = useState(0);
  const answer = useAnswerDecisionRequest(card.issue_id);
  const sendAnswer = (text: string) => {
    // Keep what is sent as the draft, so a refused send shows back the same text.
    setDraft({ text });
    // mutateAsync, not mutate's callbacks: those are dropped when the card unmounts mid-send,
    // which would leave a sent answer in the store.
    answer
      .mutateAsync({ requestId: card.id, option: draft.option || undefined, text: text.trim() || undefined })
      .then(() => {
        invalidatePendingUploads(decisionAnswerDraftKey(card.id));
        clearDraft();
        setFormKey((key) => key + 1);
      })
      .catch((error: unknown) => {
        if (isConflict(error)) setRefused(true);
      });
  };
  const discardDraft = () => {
    invalidatePendingUploads(decisionAnswerDraftKey(card.id));
    clearDraft();
    setRefused(false);
  };

  return (
    <section
      aria-label={t(($) => $.decision.request_label, { state })}
      data-testid="decision-request-card"
      data-state={shown}
      className="min-w-0 rounded-xl border border-surface-border bg-surface text-body"
    >
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-surface-border px-3 py-2 text-label">
        <MessageSquareQuote className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        <span className="font-medium">{t(($) => $.decision.request_title)}</span>
        {card.asking_agent_name && (
          <span className="text-muted-foreground" data-testid="decision-request-asker">
            {t(($) => $.decision.asked_by, { agent: card.asking_agent_name })}
          </span>
        )}
        <span className={cn(
          "rounded-full px-2 py-0.5 text-micro font-medium",
          shown === "open" && "bg-warning/10 text-warning",
          (shown === "recorded" || shown === "delivered" || shown === "picked_up") && "bg-success/10 text-success",
          shown === "not_picked_up" && "bg-warning/10 text-warning",
          (shown === "withdrawn" || shown === "disclaimed") && "bg-muted text-muted-foreground",
        )}>
          {state}
        </span>
        {card.answer?.chosen_option_index !== undefined && shown !== "disclaimed" && (
          <span className="rounded-xs border px-1.5 text-micro" title={card.answer.option_label}>
            {t(($) => $.decision.receipt_option, { number: card.answer.chosen_option_index + 1 })}
          </span>
        )}
        <AppLink
          href={issueHref}
          className="inline-flex min-h-9 max-w-full items-center break-all rounded-full border border-surface-border px-2 font-medium hover:underline focus-visible:outline-2 focus-visible:outline-ring"
          data-testid="decision-request-source-issue"
        >
          {card.issue_identifier || card.issue_id}
        </AppLink>
        {card.issue_title && (
          <span className="min-w-0 break-words text-muted-foreground" data-testid="decision-request-source-title">
            {card.issue_title}
          </span>
        )}
        {card.source_comment_id && (
          <AppLink
            href={`${issueHref}#comment-${card.source_comment_id}`}
            className="inline-flex min-h-9 items-center hover:underline focus-visible:outline-2 focus-visible:outline-ring"
            data-testid="decision-request-source-comment"
          >
            {t(($) => $.decision.source_comment)}
          </AppLink>
        )}
        {card.state === "answered" && card.record_comment_id && (
          <AppLink
            href={`${issueHref}#comment-${card.record_comment_id}`}
            className="inline-flex min-h-9 items-center hover:underline focus-visible:outline-2 focus-visible:outline-ring"
            data-testid="decision-request-record"
          >
            {t(($) => $.decision.recorded_on, { issue: card.issue_identifier || card.issue_id })}
          </AppLink>
        )}
      </div>
      <div className="min-w-0 space-y-3 break-words px-3 py-3">
        {card.state === "withdrawn" && card.reason && (
          <p className="whitespace-pre-wrap text-muted-foreground" data-testid="decision-request-reason">{card.reason}</p>
        )}
        <DecisionArtifacts requestId={card.id} answered={card.state === "answered"} />
        <div data-testid="decision-request-ask">
          <AnnotatableAnswer messageId={`${DECISION_REQUEST_SOURCE_PREFIX}${card.id}`} readOnly={card.state !== "open"}>
            <CompactIssueChipsProvider>
              <RichContent content={card.ask} density="compact" phase="settled" />
            </CompactIssueChipsProvider>
          </AnnotatableAnswer>
        </div>
        {(refused || card.state !== "open") && hasDraft(draft) && !answer.isPending ? (
          <KeptDraft draft={draft} onDiscard={discardDraft} />
        ) : (
          card.state === "open" && (
            <DecisionAnswerForm key={formKey} card={card} draft={draft} answer={answer} onDraftChange={setDraft} onSend={sendAnswer} inIssue={inIssue} />
          )
        )}
        {shown === "picked_up" && card.answer?.run && (
          <AppLink href={`${issueHref}?run=${encodeURIComponent(card.answer.run.id)}`} data-testid="decision-request-run" className="inline-flex min-h-9 items-center text-label hover:underline focus-visible:outline-2 focus-visible:outline-ring">
            {t(($) => $.decision.picked_up_by, { agent: card.answer.run.agent_name })}
          </AppLink>
        )}
        {shown === "not_picked_up" && (
          <p role="status" className="text-label text-warning">
            {t(($) => $.decision.not_picked_up)}{" "}
            <AppLink href={issueHref} data-testid="decision-request-not-picked-up" className="font-medium underline focus-visible:outline-2 focus-visible:outline-ring">
              {t(($) => $.decision.open_issue, { issue: card.issue_identifier || card.issue_id })}
            </AppLink>
          </p>
        )}
        {validDate && (
          <time dateTime={timestamp} className="block text-caption text-muted-foreground">
            {new Intl.DateTimeFormat("en-GB", {
              timeZone: "Asia/Bangkok",
              day: "2-digit",
              month: "short",
              year: "numeric",
              hour: "2-digit",
              minute: "2-digit",
              hourCycle: "h23",
            }).format(date)} ICT
          </time>
        )}
      </div>
    </section>
  );
}
