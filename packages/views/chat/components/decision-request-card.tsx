import { Suspense, useEffect, useId, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Ban, Check, ChevronDown, Loader2, MessageSquareQuote, Undo2 } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import { ApiError } from "@multica/core/api";
import { useAnswerDecisionRequest } from "@multica/core/issues/decision-answer";
import {
  type DecisionAnswerDraft as AnswerDraft,
  EMPTY_DECISION_ANSWER_DRAFT as EMPTY_DRAFT,
  decisionAnswerDraftKey,
  useDecisionAnswerDraftStore,
} from "@multica/core/issues/stores/decision-answer-draft-store";
import { usePersistedDraft, useUploadCancelledNotice } from "@multica/core/drafts";
import { approvableConfigChange, approvablePreview, isConfigChangeActionType } from "@multica/core/issues/decision-action-preview";
import { useCurrentMember } from "@multica/core/permissions";
import { chatKeys } from "@multica/core/chat/queries";
import { useCurrentWorkspace, useWorkspacePaths } from "@multica/core/paths";
import { formatShortcut, useShortcut } from "@multica/core/shortcuts";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import type { ContentEditorRef } from "../../editor/content-editor";
import { useEditorUpload } from "../../editor/use-editor-upload";
import { useUploadGate } from "../../editor/use-upload-gate";
import { LazyContentEditor as ContentEditor } from "./lazy-content-editor";
import { RichContent } from "../../rich-content";
import { CompactIssueChipsProvider } from "../../issues/compact-issue-chips-context";
import { useT } from "../../i18n";
import { DecisionActionApproval } from "../../common/decision-action-approval";
import { AppLink, useOptionalNavigation } from "../../navigation";
import { openExternal } from "../../platform";
import { isDesktopShell } from "../../platform/local-directory";
import { useOnline } from "../../settings/components/use-online";
import { AnnotatableAnswer } from "./annotatable-answer";
import { ConfigChangeViewer } from "./config-change-viewer";
import { DecisionArtifacts } from "./decision-artifacts";
import { DecisionRecentRuns } from "./decision-recent-runs";
import { DECISION_REQUEST_SOURCE_PREFIX } from "./chat-annotation-context";

// Mirrors the server's `maxDecisionCardTextBytes`; it counts the trimmed text, image links included.
const MAX_ANSWER_BYTES = 8 << 10;
const answerBytes = (text: string) => new TextEncoder().encode(text.trim()).length;

const isConflict = (error: unknown) => error instanceof ApiError && error.status === 409;

// The server prefixes a refusal with its code ("stale_answer: ..."); a 409 with one of these codes leaves the card open.
const refusalCode = (error: unknown) => (error instanceof ApiError ? error.message.split(":")[0] ?? "" : "");
const KEEPS_CARD_OPEN = ["stale_answer", "no_passkey", "busy_retry"];
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
  needsPasskey,
  onOpenWeb,
}: {
  card: RequestCard;
  draft: AnswerDraft;
  answer: AnswerMutation;
  onDraftChange: (patch: Partial<AnswerDraft>) => void;
  onSend: (text: string, option?: string) => void;
  inIssue: boolean;
  /** The server refused a bare answer: the next send runs the passkey ceremony. */
  needsPasskey: boolean;
  /** Desktop cannot run the ceremony; it opens the web page instead. */
  onOpenWeb: () => void;
}) {
  const { t } = useT("chat");
  const online = useOnline();
  const handoff = needsPasskey && isDesktopShell();
  const sendShortcut = formatShortcut(useShortcut("send"));
  const { text } = draft;
  const draftKey = decisionAnswerDraftKey(card.id);
  // Bound to the workspace this card mounted in, so cleanup after a workspace switch still files the notice there.
  const [uploadCancelled, setUploadCancelled] = useUploadCancelledNotice(draftKey);
  // What this editor last reported. A stored text that differs came from the other surface
  // showing the same request, and is adopted so a send from here cannot overwrite it.
  const emitted = useRef(text);
  // Uploads started here and still running. Leaving the card cancels them (Cheese decision,
  // CHE-3521): nothing is filed into the draft later, so no late write can outlive the card.
  const uploads = useRef(new Set<AbortController>());
  // The editor's unmount flush (the last note edit) is applied. What it reports after that, such as
  // the placeholder of a cancelled upload going away, is dropped. The flush runs synchronously
  // in this unmount; the microtask flips `closed` before any async report can arrive.
  const closed = useRef(false);
  useEffect(() => {
    closed.current = false;
    const running = uploads.current;
    return () => {
      queueMicrotask(() => { closed.current = true; });
      if (running.size === 0) return;
      running.forEach((controller) => controller.abort());
      running.clear();
      // In memory only: a storage write here could bring back a draft another tab just cleared.
      setUploadCancelled(true);
    };
  }, [setUploadCancelled]);
  const onEditorUpdate = (next: string) => { if (!closed.current) setText(next); };
  const setText = (next: string) => {
    emitted.current = next;
    onDraftChange({ text: next });
  };
  // A tap selects; a second tap on the selected option clears it. Nothing is sent until Send.
  const toggleOption = (label: string) => onDraftChange({ option: draft.option === label ? "" : label });
  const textId = useId();
  const editorRef = useRef<ContentEditorRef>(null);
  const uploadGate = useUploadGate(editorRef);
  const { uploadWithToast } = useEditorUpload();
  const uploadFile = async (file: File) => {
    const controller = new AbortController();
    uploads.current.add(controller);
    setUploadCancelled(false);
    try {
      return await uploadWithToast(file, { issueId: card.issue_id, signal: controller.signal });
    } finally {
      uploads.current.delete(controller);
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
  const canSubmit = !handoff && online && !answer.isPending && !uploadGate.uploading && !tooLong;
  const canSend = canSubmit && (draft.option !== "" || text.trim() !== "");
  // Send posts the selected option with whatever note is in the box. A retry keeps the kept choice.
  const send = () => {
    const option = draft.option;
    // onUpdate reaches `text` through a timer, so an Enter or click right after the last edit
    // (or a finished upload) would send stale text. Read the editor and send exactly that.
    const live = editorRef.current?.getMarkdown() ?? text;
    // Enter can arrive while an upload placeholder is still in the document.
    const sendable = !handoff && online && !answer.isPending && !uploadGate.uploading && !uploadGate.isBlocked()
      && answerBytes(live) <= MAX_ANSWER_BYTES && (!!option || live.trim() !== "");
    if (!sendable) return;
    emitted.current = live;
    onSend(live, option);
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
        <div role="radiogroup" aria-label={t(($) => $.decision.answer_options)} className="space-y-1">
          {card.options.map((item, index) => (
            <button
              key={item.label}
              type="button"
              role="radio"
              aria-checked={draft.option === item.label}
              disabled={!canSubmit}
              onClick={() => toggleOption(item.label)}
              className={cn(
                "flex min-h-9 w-full items-start gap-2 rounded-lg border border-surface-border px-2 py-1.5 text-left hover:bg-accent focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring disabled:opacity-70",
                draft.option === item.label && "border-primary bg-primary/10 ring-1 ring-primary",
              )}
            >
              <span aria-hidden className={cn("mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-full border", draft.option === item.label ? "border-primary bg-primary text-primary-foreground" : "border-surface-border")}>
                {draft.option === item.label && <Check className="size-3" />}
              </span>
              <span className="min-w-0 space-y-0.5">
                <span className="flex flex-wrap items-center gap-2">
                  <span className="font-medium">{item.label}</span>
                  {card.recommended_option_index === index && (
                    <span className="rounded-full bg-primary/10 px-2 py-0.5 text-micro font-medium text-primary">{t(($) => $.decision.option_recommended)}</span>
                  )}
                </span>
                {item.what_happens && <span className="block text-caption text-muted-foreground">{item.what_happens}</span>}
                {!!item.pros?.length && <span className="block text-caption text-muted-foreground">{t(($) => $.decision.option_pros)}: {item.pros.join("; ")}</span>}
                {!!item.cons?.length && <span className="block text-caption text-muted-foreground">{t(($) => $.decision.option_cons)}: {item.cons.join("; ")}</span>}
                {item.cost_or_time && <span className="block text-caption text-muted-foreground">{t(($) => $.decision.option_cost)}: {item.cost_or_time}</span>}
              </span>
            </button>
          ))}
        </div>
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
            onSubmit={() => send()}
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
        <Button type="submit" size="sm" disabled={!canSend} className="disabled:opacity-70 max-sm:h-11">
          {answer.isPending && <Loader2 className="size-4 animate-spin" aria-hidden />}
          {t(($) => (needsPasskey ? $.decision.answer_send_passkey : $.decision.answer_send))}
        </Button>
        {draft.option !== "" && (
          <Button type="button" size="sm" variant="ghost" disabled={!canSubmit} className="max-sm:h-11" onClick={() => onDraftChange({ option: "" })}>
            {t(($) => $.decision.answer_clear)}
          </Button>
        )}
        {handoff && (
          <Button type="button" size="sm" variant="outline" onClick={onOpenWeb}>{t(($) => $.decision.answer_open_browser)}</Button>
        )}
        {!online && <span role="status" className="text-caption text-warning">{t(($) => $.decision.answer_offline)}</span>}
        {answer.isError && (!isConflict(answer.error) || KEEPS_CARD_OPEN.includes(refusalCode(answer.error))) && (
          <span role="alert" className="text-caption text-destructive">
            {answerErrorText(answer.error, needsPasskey, handoff, t)}
          </span>
        )}
      </div>
    </form>
  );
}

type CardAction = NonNullable<RequestCard["action"]>;

// Approve for an action only the workspace owner may approve. Fails closed: with no workspace, no
// member list yet or any other role the button is absent. The server still refuses a non-owner.
function OwnerGatedApproval({ action, previewSha256, approveDisabled }: { action: CardAction; previewSha256?: string; approveDisabled: boolean }) {
  const workspace = useCurrentWorkspace();
  return workspace
    ? <MemberGatedApproval wsId={workspace.id} action={action} previewSha256={previewSha256} approveDisabled={approveDisabled} />
    : <DecisionActionApproval actionId={action.id} payloadSha256={action.payload_sha256} previewSha256={previewSha256} canApprove={false} state={action.state} />;
}

function MemberGatedApproval({ wsId, action, previewSha256, approveDisabled }: { wsId: string; action: CardAction; previewSha256?: string; approveDisabled: boolean }) {
  const { role } = useCurrentMember(wsId);
  return <DecisionActionApproval actionId={action.id} payloadSha256={action.payload_sha256} previewSha256={previewSha256} canApprove={role === "owner"} approveDisabled={approveDisabled} state={action.state} />;
}

// What the approval would change, shown before Approve exists. No usable preview
// (missing, empty, malformed, bad digest, unknown action type) means the server
// could not vouch for the action: say so, keep Approve disabled for who may approve,
// show it to nobody else, and leave Reject to everyone.
function ActionPreview({ action, issueId }: { action: CardAction; issueId: string }) {
  const { t } = useT("chat");
  // Once the action left pending there is nothing to preview: only its live outcome shows.
  const pending = !action.state || action.state === "pending";
  const configAction = isConfigChangeActionType(action.action_type);
  const ownerOnly = configAction || action.owner_only === true;
  const configChange = pending && configAction ? approvableConfigChange(action) : undefined;
  const fields = pending && !configAction ? approvablePreview(action)?.fields : undefined;
  const unavailable = pending && !fields && !configChange;
  // Same tree before and after approval, so the approval component keeps its state when the card turns answered.
  return (
    <div className="space-y-2">
      {unavailable && <p role="status" className="text-label text-warning">{t(($) => $.decision.action_unavailable)}</p>}
      {configChange && <ConfigChangeViewer change={configChange} />}
      {pending && action.action_type.startsWith("issue.run_budget.") && <DecisionRecentRuns issueId={issueId} />}
      {fields && (
        <dl data-testid="decision-request-action-preview" aria-label={t(($) => $.decision.action_preview)} className="space-y-1 rounded-lg border border-surface-border px-2 py-1.5 text-label">
          {fields.map((field) => (
            <div key={field.label} className="flex flex-wrap items-baseline gap-x-2">
              <dt className="font-medium">{field.label}</dt>
              <dd className="text-muted-foreground">{field.current} → <span className="text-foreground">{field.proposed}</span></dd>
            </div>
          ))}
        </dl>
      )}
      {ownerOnly
        ? <OwnerGatedApproval action={action} previewSha256={configChange?.sha256} approveDisabled={unavailable} />
        : <DecisionActionApproval actionId={action.id} payloadSha256={action.payload_sha256} approveDisabled={unavailable} state={action.state} />}
    </div>
  );
}

// The server appends the agent's free text to the ask as a "> "-quoted block under this
// heading. Show it apart, as plain text: it is the agent's claim, never a fact.
const AGENT_NOTE_HEADING = "\n\n**Agent's note (not verified)**\n";

function splitAgentNote(ask: string): { facts: string; note: string } {
  const at = ask.indexOf(AGENT_NOTE_HEADING);
  if (at < 0) return { facts: ask, note: "" };
  const note = ask.slice(at + AGENT_NOTE_HEADING.length).split("\n").map((line) => line.replace(/^> ?/, "")).join("\n");
  return { facts: ask.slice(0, at), note };
}

function AgentNote({ note }: { note: string }) {
  const { t } = useT("chat");
  const label = t(($) => $.decision.agent_note);
  return (
    <div role="group" aria-label={label} data-testid="decision-request-agent-note" className="space-y-0.5 rounded-lg border border-dashed border-surface-border px-2 py-1.5 text-label">
      <span className="font-medium text-muted-foreground">{label}</span>
      <p className="whitespace-pre-wrap break-words">{note}</p>
    </div>
  );
}

function answerErrorText(
  error: unknown,
  needsPasskey: boolean,
  handoff: boolean,
  t: ReturnType<typeof useT<"chat">>["t"],
) {
  const code = refusalCode(error);
  if (code === "assertion_required") return t(($) => (handoff ? $.decision.answer_passkey_desktop : $.decision.answer_passkey_needed));
  if (code === "stale_answer") return t(($) => $.decision.answer_stale);
  if (code === "no_passkey") return t(($) => $.decision.answer_no_passkey);
  // Not an ApiError once the ceremony ran: the person cancelled it or the browser refused.
  if (needsPasskey && !(error instanceof ApiError)) return t(($) => $.decision.answer_passkey_failed);
  return t(($) => $.decision.answer_error);
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

// An answer nobody needs to act on any more: the card folds to one row (design `decision-card-fold`, direction A).
const FOLDS = new Set(["picked_up", "withdrawn", "disclaimed"]);
const FOLD_ICONS = { picked_up: Check, withdrawn: Undo2, disclaimed: Ban } as const;

// The folded row names the question: the first line of the ask, markdown marks dropped.
const questionOf = (ask: string, fallback: string) => {
  const line = ask.split("\n").map((l) => l.replace(/^[\s#>*_-]+|[*_`]+/g, "").trim()).find(Boolean);
  return line || fallback;
};

const ictFormat = new Intl.DateTimeFormat("en-GB", {
  timeZone: "Asia/Bangkok",
  day: "2-digit",
  month: "short",
  year: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});

// `inIssue`: shown on the source issue page, where there is no DM message box.
export function DecisionRequestCard({ card, inIssue = false }: { card: RequestCard; inIssue?: boolean }) {
  const { t } = useT("chat");
  const paths = useWorkspacePaths();
  const { facts, note } = splitAgentNote(card.ask);
  const shown = displayState(card, useNow(card));
  // A batch answer must not be sent blind: with any covered decision unloaded the form stays away.
  const coveredCardsMissing = (card.covers_request_ids?.length ?? 0) > (card.covered_cards?.length ?? 0);
  const state = t(($) => $.decision.request_state[shown]);
  const timestamp = card.state === "withdrawn" ? card.withdrawn_at || card.created_at : card.created_at;
  const date = new Date(timestamp);
  const validDate = !Number.isNaN(date.getTime());
  const ictText = validDate ? `${ictFormat.format(date)} ICT` : "";
  // Expand state is local and never saved. A card already done at load starts folded, so its
  // first paint has nothing to animate; a later change of state runs the CSS transition.
  const [expanded, setExpanded] = useState(false);
  const folded = FOLDS.has(shown) && !expanded;
  const bodyId = useId();
  const issueHref = paths.issueDetail(card.issue_id);
  const [draft, setDraft, clearDraft] = usePersistedDraft(useDecisionAnswerDraftStore, decisionAnswerDraftKey(card.id), EMPTY_DRAFT);
  const [, setUploadCancelled] = useUploadCancelledNotice(decisionAnswerDraftKey(card.id));
  const [refused, setRefused] = useState(false);
  const [needsPasskey, setNeedsPasskey] = useState(false);
  const navigation = useOptionalNavigation();
  // Remounting the form empties the editor after a send: the editor takes its draft once at mount.
  const [formKey, setFormKey] = useState(0);
  const answer = useAnswerDecisionRequest(card.issue_id);
  const sendAnswer = (text: string, option = "") => {
    // Keep what is sent as the draft, so a refused send shows back the same choice and text.
    setDraft({ option, text });
    // mutateAsync, not mutate's callbacks: those are dropped when the card unmounts mid-send,
    // which would leave a sent answer in the store.
    answer
      .mutateAsync({ requestId: card.id, option: option || undefined, text: text.trim() || undefined, passkey: needsPasskey, covers: card.covers_request_ids })
      .then(() => {
        setNeedsPasskey(false);
        clearDraft();
        setUploadCancelled(false);
        setFormKey((key) => key + 1);
      })
      .catch((error: unknown) => {
        const code = refusalCode(error);
        if (code === "assertion_required") setNeedsPasskey(true);
        else if (isConflict(error) && !KEEPS_CARD_OPEN.includes(code)) setRefused(true);
      });
  };
  const discardDraft = () => {
    clearDraft();
    setUploadCancelled(false);
    setRefused(false);
  };

  if (card.answered_by_batch_request_id && !inIssue) {
    return (
      <p data-testid="decision-request-folded" className="flex min-w-0 flex-wrap items-center gap-x-2 rounded-xl border border-surface-border bg-surface px-3 py-2 text-label text-muted-foreground">
        <span>{t(($) => $.decision.answered_by_batch, { issue: card.issue_identifier || card.issue_id })}</span>
        <AppLink href={issueHref} className="font-medium hover:underline focus-visible:outline-2 focus-visible:outline-ring">
          {card.issue_title}
        </AppLink>
      </p>
    );
  }

  const StateIcon = FOLD_ICONS[shown as keyof typeof FOLD_ICONS] ?? MessageSquareQuote;
  return (
    <section
      aria-label={t(($) => $.decision.request_label, { state })}
      data-testid="decision-request-card"
      data-state={shown}
      className="min-w-0 rounded-xl border border-surface-border bg-surface text-body"
    >
      {FOLDS.has(shown) && (
        <button
          type="button"
          aria-expanded={!folded}
          aria-controls={bodyId}
          onClick={() => setExpanded((open) => !open)}
          data-testid="decision-request-fold-toggle"
          className="flex min-h-9 w-full min-w-0 items-center gap-2 px-3 py-1.5 text-left text-label focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
        >
          <StateIcon className="size-4 shrink-0 text-muted-foreground" aria-hidden />
          <span className="sr-only">{state}: </span>
          <span className="min-w-0 flex-1 truncate font-medium">{questionOf(facts, card.issue_title || card.issue_identifier || card.issue_id)}</span>
          {card.answer?.chosen_option_index !== undefined && shown !== "disclaimed" && (
            <span className="max-w-[40%] shrink-0 truncate rounded-xs border px-1.5 text-micro">
              {t(($) => $.decision.receipt_option, { number: card.answer.chosen_option_index + 1 })}
              {card.answer.option_label && <span className="max-sm:hidden"> · {card.answer.option_label}</span>}
            </span>
          )}
          {ictText && <time dateTime={timestamp} className="shrink-0 text-caption text-muted-foreground max-sm:hidden">{ictText}</time>}
          <ChevronDown className={cn("size-4 shrink-0 text-muted-foreground transition-transform duration-200 ease-out motion-reduce:transition-none", !folded && "rotate-180")} aria-hidden />
        </button>
      )}
      <div
        id={bodyId}
        data-fold-body
        data-folded={folded}
        inert={folded}
        className={cn(
          "grid transition-[grid-template-rows,opacity] duration-200 ease-out motion-reduce:transition-none",
          folded ? "grid-rows-[0fr] opacity-0" : "grid-rows-[1fr] opacity-100",
        )}
      >
      <div className={cn("min-h-0", FOLDS.has(shown) && "overflow-hidden")}>
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
          className="inline-flex min-h-9 max-w-full items-center break-all rounded-lg border border-surface-border px-2 font-medium hover:underline focus-visible:outline-2 focus-visible:outline-ring"
          data-testid="decision-request-source-issue"
        >
          {card.issue_identifier || card.issue_id}
        </AppLink>
        {card.covers_request_ids && card.covers_request_ids.length > 0 && (
          <span className="text-muted-foreground" data-testid="decision-request-covers">
            {t(($) => $.decision.covers_batch, { count: card.covers_request_ids.length })}
          </span>
        )}
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
      {card.covered_cards && card.covered_cards.length > 0 && (
        <ul className="min-w-0 space-y-1 break-words border-b px-3 py-2 text-xs" data-testid="decision-request-covered-list">
          {card.covered_cards.map((covered) => (
            <li key={covered.id} data-testid="decision-request-covered-item">
              <AppLink href={paths.issueDetail(covered.issue_id)} className="font-medium hover:underline focus-visible:outline-2 focus-visible:outline-ring">
                {covered.issue_identifier || covered.issue_id}
              </AppLink>
              {covered.issue_title && <span className="text-muted-foreground">{` ${covered.issue_title}`}</span>}
              {covered.decision && <span>{`: ${covered.decision}`}</span>}
              {covered.options && covered.options.length > 0 && (
                <ul className="ml-4 list-disc text-muted-foreground">
                  {covered.options.map((option, index) => (
                    <li key={`${index}:${option.label}`}>{option.what_happens ? `${option.label}: ${option.what_happens}` : option.label}</li>
                  ))}
                </ul>
              )}
            </li>
          ))}
        </ul>
      )}
      <div className="min-w-0 space-y-3 break-words px-3 py-3">
        {card.state === "withdrawn" && card.reason && (
          <p className="whitespace-pre-wrap text-muted-foreground" data-testid="decision-request-reason">{card.reason}</p>
        )}
        <DecisionArtifacts requestId={card.id} answered={card.state === "answered"} />
        <div data-testid="decision-request-ask">
          <AnnotatableAnswer messageId={`${DECISION_REQUEST_SOURCE_PREFIX}${card.id}`} readOnly={card.state !== "open"}>
            <CompactIssueChipsProvider>
              <RichContent content={facts} density="compact" phase="settled" />
            </CompactIssueChipsProvider>
          </AnnotatableAnswer>
        </div>
        {note && <AgentNote note={note} />}
        {card.action && (card.state === "open" || (card.action.state && card.action.state !== "pending")) && <ActionPreview action={card.action} issueId={card.issue_id} />}
        {(refused || card.state !== "open") && hasDraft(draft) && !answer.isPending ? (
          <KeptDraft draft={draft} onDiscard={discardDraft} />
        ) : (
          card.state === "open" && (coveredCardsMissing ? (
            <p role="alert" className="text-destructive" data-testid="decision-request-covers-unavailable">{t(($) => $.decision.covers_unavailable)}</p>
          ) : (
            <DecisionAnswerForm key={formKey} card={card} draft={draft} answer={answer} onDraftChange={setDraft} onSend={sendAnswer} inIssue={inIssue} needsPasskey={needsPasskey} onOpenWeb={() => navigation && openExternal(navigation.getShareableUrl(issueHref))} />
          ))
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
          <time dateTime={timestamp} className="block text-caption text-muted-foreground">{ictText}</time>
        )}
      </div>
      </div>
      </div>
    </section>
  );
}
