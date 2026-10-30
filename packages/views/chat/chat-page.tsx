"use client";

import { useEffect, useMemo, useRef, useSyncExternalStore } from "react";
import { useDefaultLayout } from "react-resizable-panels";
import { ArrowLeft, MessageSquare } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@multica/ui/components/ui/button";
import {
  ResizablePanelGroup,
  ResizablePanel,
  ResizableHandle,
} from "@multica/ui/components/ui/resizable";
import { useIsCompact } from "@multica/ui/hooks/use-mobile";
import { useCurrentWorkspace, useRequiredWorkspaceSlug, useWorkspacePaths } from "@multica/core/paths";
import { resolveAutoCompact } from "@multica/core/workspace/auto-compact";
import {
  getCurrentSlug,
  getWorkspaceRehydrationGeneration,
  subscribeToWorkspaceRehydration,
} from "@multica/core/platform";
import { useChatStore } from "@multica/core/chat";
import { chatQuickActionsPendingOptions } from "@multica/core/chat/queries";
import { useRegenerateChatQuickActions } from "@multica/core/chat/mutations";
import { useQuickActionsPendingTimeout } from "@multica/core/chat/use-quick-actions-pending-timeout";
import { useQuickActionsFailureToast } from "./components/use-quick-actions-failure-toast";
import { useQuery } from "@tanstack/react-query";
import type { Agent, ChatSearchHit, ChatSession } from "@multica/core/types";
import { PageHeader } from "../layout/page-header";
import { useBackOrReplace, useNavigation } from "../navigation";
import { useT } from "../i18n";
import { ChatMessageList, ChatMessageSkeleton } from "./components/chat-message-list";
import { ChatInput } from "./components/chat-input";
import { ChatQueue } from "./components/chat-queue";
import { ChatThreadList } from "./components/chat-thread-list";
import { ChatSessionHeader } from "./components/chat-session-header";
import { useChatRename } from "./components/use-chat-rename";
import { useChatFork } from "./components/use-chat-fork";
import { useDmSessionSurface } from "./components/use-dm-session-surface";
import { canForkSession } from "./lib/can-fork-session";
import { EmptyState } from "./components/chat-empty-state";
import { NewChatButton } from "./components/new-chat-button";
import { useChatController } from "./components/use-chat-controller";
import { OfflineBanner } from "./components/offline-banner";
import { NoAgentBanner } from "./components/no-agent-banner";
import { ArchivedAgentBanner } from "./components/archived-agent-banner";
import { AgentAccessRevokedBanner } from "./components/agent-access-revoked-banner";
import { RuntimeRequiredBanner } from "./components/runtime-required-banner";
import { InteractiveLoginBanner } from "./components/interactive-login-banner";
import { ChatAnnotationContext } from "./components/chat-annotation-context";
import { useChatAnnotations } from "./components/use-chat-annotations";
import { useQuoteRequest } from "./components/use-quote-request";
import { useRegisterCommandBarContext } from "../search/register-context";

/**
 * Chat tab — the first-class two-pane surface (thread list on the left,
 * conversation on the right), mirroring the Inbox page layout. Shares all
 * conversation logic with the floating FAB via `useChatController`; the
 * left rail reuses `ChatThreadList`.
 *
 * Selection is URL-addressable via `?session=<id>` so a thread can be
 * deep-linked, opened from a notification, and survive refresh. The chat
 * store's `activeSessionId` stays the source of truth (both surfaces read
 * it); the URL is kept in sync in both directions. One store → URL effect is
 * the only writer: user selection and "new chat" record a `push` intent in the
 * store first so Back walks list > A > B, and every other change (a new chat
 * receiving its server ID, archive, delete, realtime) replaces the entry.
 * An unsent new chat is `?new=1`. `?agent=<id>` is the
 * complementary one-shot deep link for a NEW chat: it starts a fresh compose
 * bound to that agent and is then stripped from the URL.
 *
 * Starting a chat is where the agent is chosen: the header ⊕ opens an agent
 * picker (see NewChatButton), so the compose box no longer needs its own
 * agent selector. Unlike the FAB, this page passes no `contextItems` to
 * `ChatInput`, so its `@` mentions fall back to manual search (issue-comment
 * style).
 */
export function ChatPage() {
  const { t } = useT("chat");
  const { pathname, searchParams, push, replace } = useNavigation();
  const backOrReplace = useBackOrReplace();
  const workspaceSlug = useRequiredWorkspaceSlug();
  const wsPaths = useWorkspacePaths();
  const workspace = useCurrentWorkspace();
  // App Router can retain this page after navigation. Once the URL belongs
  // to another route, this instance must stop reconciling shared chat state.
  // Also check the live workspace mirror in each effect: the incoming layout
  // can rehydrate the store before this page observes the destination URL.
  const isCurrentChatRoute = pathname === wsPaths.chat();
  const isCompact = useIsCompact();

  const c = useChatController({
    isActive: isCurrentChatRoute && getCurrentSlug() === workspaceSlug,
  });
  const { data: quickActionsPending = null } = useQuery(
    chatQuickActionsPendingOptions(c.activeSessionId ?? ""),
  );
  // Drop a stuck pending marker (dead daemon / failed supplement) so the pill
  // spinner stops and a later refresh starts clean (MUL-5149).
  useQuickActionsPendingTimeout(c.activeSessionId ?? null, quickActionsPending);
  // Toast when an accepted refresh later fails in the daemon (async half).
  useQuickActionsFailureToast(c.activeSessionId ?? null);
  const regenerateQuickActions = useRegenerateChatQuickActions();
  const urlSession = searchParams.get("session") || null;
  const urlAgent = searchParams.get("agent") || null;
  // `?new=1` is an unsent new chat, so Back from it and a refresh behave.
  const urlNew = searchParams.get("new") === "1";
  // `?message=` is a one-shot jump from a decision card; it only applies once
  // the list shows the session named beside it.
  const urlMessage = searchParams.get("message") || null;
  const settleJump = (outcome: "landed" | "missing") => {
    if (outcome === "missing") toast.error(t(($) => $.decision.message_unavailable));
    replace(urlSession ? `${wsPaths.chat()}?session=${urlSession}` : wsPaths.chat());
  };

  const showsMessageList =
    c.hasMessages ||
    !!c.currentSession?.continued_from_session_id ||
    !!c.currentSession?.continued_into_session_id ||
    !!c.currentSession?.forked_from_session_id;
  // The list reports a missing target; an empty chat never mounts it, so the
  // jump settles here once the chat has loaded and has nothing to scroll to.
  const emptyChatLoaded =
    !!c.currentSession && c.messagesLoaded && !c.showSkeleton && !showsMessageList;
  const jumpTargetsEmptyChat = !!urlMessage && urlSession === c.activeSessionId && emptyChatLoaded;
  const settledEmptyJump = useRef<string | null>(null);
  useEffect(() => {
    // Forget the target once the URL drops it, so the same hit can settle again.
    if (!urlMessage) settledEmptyJump.current = null;
    if (!jumpTargetsEmptyChat || settledEmptyJump.current === urlMessage) return;
    settledEmptyJump.current = urlMessage;
    settleJump("missing");
    // eslint-disable-next-line react-hooks/exhaustive-deps -- settle once per target
  }, [jumpTargetsEmptyChat, urlMessage]);

  // A user navigation asks for a history entry; anything else replaces the
  // current one. The intent is one-shot: the store → URL effect consumes it.
  // A change to the URL the user is already on records nothing, so the intent
  // cannot outlive the effect that would consume it.
  const requestNav = (mode: "push" | "replace", newChat: boolean) =>
    useChatStore.getState().requestNav({ mode, newChat });
  const navIntent = useChatStore((s) => s.navIntent);
  // The write this page made and the router has not published yet. The store
  // updates in the same tick, so the effect runs again (the consumed intent
  // is itself a store change) before the URL shows the target. That run must
  // not read the stale URL as a request to change it back.
  const pendingWrite = useRef<{ liveSession: string | null } | null>(null);

  // "Composing a brand-new chat" — the user hit ⊕ but hasn't sent yet, so no
  // session exists. The URL owns it (`?new=1`), so Back, Forward and refresh
  // agree with the pane. At compact widths it decides list-vs-conversation;
  // on desktop the conversation pane is always mounted.
  const composingNew = urlNew;
  const { handleRename, renameRequest } = useChatRename(c.currentSession);

  // Two-way sync between the URL (`?session=`) and the chat store's
  // activeSessionId. Both effects read the LIVE store value via
  // `useChatStore.getState()` rather than the render-captured `c.activeSessionId`.
  // That is what keeps them from fighting on mount: a naive mirror effect fires
  // with the stale (null) snapshot and "corrects" the URL by stripping the
  // session before the URL→store effect has applied — breaking deep links and
  // making selection / new-chat feel unresponsive. Reading getState() sees the
  // value the sibling effect just wrote, so the reconciliation converges in one
  // pass and is idempotent under StrictMode's double-invoke.

  // URL → store: deep link, refresh, notification click, back/forward.
  useEffect(() => {
    if (!isCurrentChatRoute || getCurrentSlug() !== workspaceSlug) return;
    pendingWrite.current = null;
    if (urlSession !== useChatStore.getState().activeSessionId) {
      c.setActiveSession(urlSession);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- reconcile URL/route changes, not store updates
  }, [isCurrentChatRoute, workspaceSlug, urlSession, urlNew]);

  // The workspace layout publishes the workspace in an effect, and the chat
  // store rehydrates from that workspace one microtask later. A page that
  // mounts in the same commit syncs the URL before either has happened (or is
  // skipped by the guard above), and the rehydration then replaces the
  // selection the URL names; the store → URL effect would write that back and
  // strip a deep link. The URL stays authoritative when it names a thread, so
  // re-apply it after every rehydration. A URL without a thread leaves the
  // workspace's persisted selection to the store → URL effect as before.
  const rehydrations = useSyncExternalStore(
    subscribeToWorkspaceRehydration,
    getWorkspaceRehydrationGeneration,
    () => 0,
  );
  useEffect(() => {
    if (!isCurrentChatRoute || getCurrentSlug() !== workspaceSlug) return;
    if (urlSession && urlSession !== useChatStore.getState().activeSessionId) {
      c.setActiveSession(urlSession);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- re-apply the URL after a rehydration, not on every URL change
  }, [rehydrations]);

  // store → URL: the single URL writer. A user selection or new chat arrives
  // with a `push` intent; a session created by sending, archive advancing the
  // selection, a delete and realtime changes carry none and replace.
  useEffect(() => {
    const intent = useChatStore.getState().consumeNavIntent();
    if (!isCurrentChatRoute || getCurrentSlug() !== workspaceSlug) return;
    const base = wsPaths.chat();
    const live = useChatStore.getState().activeSessionId;
    const keepNewChat = intent ? intent.newChat : urlNew;
    const target = live ? `${base}?session=${live}` : keepNewChat ? `${base}?new=1` : base;
    const current = urlSession
      ? `${base}?session=${urlSession}`
      : urlNew
        ? `${base}?new=1`
        : base;
    if (target === current) {
      pendingWrite.current = null;
      return;
    }
    if (!intent && pendingWrite.current?.liveSession === live) return;
    pendingWrite.current = { liveSession: live };
    if (intent?.mode === "push") push(target);
    else replace(target);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- reconcile store/route changes, not URL updates
  }, [isCurrentChatRoute, workspaceSlug, c.activeSessionId, navIntent]);

  const { defaultLayout, onLayoutChanged } = useDefaultLayout({
    id: "multica_chat_layout",
  });

  // `?agent=` intent bookkeeping. The ref holds the param value already
  // consumed (or superseded) so the effect below fires at most once per deep
  // link — it also bridges the async window between replace() and the
  // searchParams actually dropping the param. Any explicit user action must
  // supersede a still-pending intent: agent/member queries can resolve late,
  // and a deferred intent firing after the user picked a thread (or started
  // another chat) would clobber that choice.
  const consumedAgentIntent = useRef<string | null>(null);
  const supersedeAgentIntent = () => {
    if (urlAgent) consumedAgentIntent.current = urlAgent;
  };

  const handleSelect = (session: ChatSession) => {
    supersedeAgentIntent();
    if (session.id !== urlSession) requestNav("push", false);
    c.handleSelectSession(session);
  };
  const { handleFork } = useChatFork(c.currentSession, handleSelect);

  // Single archive path for both entry points (thread-list row + conversation
  // header). The server accepts the archive first; a failure keeps the session
  // open. Then, when the archived chat is the one in view, move the pane off
  // it: on desktop advance to the next chat (Inbox-style); when compact go back
  // through history. Archiving any other chat leaves the view put.
  const dm = useDmSessionSurface({
    session: c.currentSession,
    sessions: c.sessions,
    send: c.handleSend,
    onOpenSession: handleSelect,
    onArchived: (session) => {
      if (session.id !== useChatStore.getState().activeSessionId) return;
      if (isCompact) backOrReplace(wsPaths.chat());
      else c.advanceSelectionAfterArchive(session);
    },
    onRename: handleRename,
    onFork: canForkSession(c.currentSession) ? handleFork : undefined,
  });
  const handleArchive = (session: ChatSession) => {
    supersedeAgentIntent();
    return dm.archive(session);
  };

  const startNewChat = (agent: Agent | null, { fromIntent = false } = {}) => {
    // A manual ⊕ pick outranks a pending deep link; when called FROM the
    // intent effect the ref is already set to this param, so this is a no-op.
    supersedeAgentIntent();
    // The deep link already owns the current entry: it becomes `?new=1`.
    if (fromIntent) requestNav("replace", true);
    else if (urlSession || !urlNew) requestNav("push", true);
    if (agent) c.handleStartNewChat(agent);
    else c.handleNewChat();
  };

  const changeProjectContext = (projectId: string | null) => {
    if (projectId === c.activeProjectId) return;
    c.handleProjectChange(projectId);
    // Removing a project stays in the current conversation. Choosing a
    // project for an existing conversation starts a clean session, and a
    // compact layout must stay in the compose pane after activeSessionId is
    // cleared.
    if ((!c.currentSession || projectId !== null) && (urlSession || !urlNew)) {
      requestNav(c.currentSession ? "push" : "replace", true);
    }
  };

  // The command bar calls the page's own handlers so archiving or re-scoping
  // from it moves the pane exactly like the header controls do.
  const commandBarHandlersRef = useRef({ handleArchive, changeProjectContext });
  useEffect(() => {
    commandBarHandlersRef.current = { handleArchive, changeProjectContext };
  });
  const commandBarSession = c.currentSession ?? null;
  useRegisterCommandBarContext(
    useMemo(
      () =>
        commandBarSession
          ? {
              kind: "chat",
              session: commandBarSession,
              archive: (session: ChatSession) => commandBarHandlersRef.current.handleArchive(session),
              changeProject: (projectId: string | null) =>
                commandBarHandlersRef.current.changeProjectContext(projectId),
            }
          : null,
      [commandBarSession],
    ),
  );

  // URL → new chat: `?agent=<id>` is the deep link used by "DM" entry points
  // (e.g. the agent detail page) to land on a fresh compose bound to that
  // agent. The permission-filtered agent list loads async, so the intent is
  // consumed on the render where the agent resolves, then the param is
  // stripped so refresh / the session sync above don't replay it. The ref
  // resets once the param is gone so a later identical deep link fires again.
  // A settled miss (access revoked, agent archived, bad id) is a denial: it
  // explains itself with a toast and consumes the intent so a later refetch
  // that surfaces the agent cannot start a chat without a fresh click. While
  // the queries are still loading the intent simply stays pending.
  useEffect(() => {
    if (!isCurrentChatRoute || getCurrentSlug() !== workspaceSlug) return;
    if (!urlAgent) {
      consumedAgentIntent.current = null;
      return;
    }
    if (consumedAgentIntent.current === urlAgent) return;
    const agent = c.availableAgents.find((a) => a.id === urlAgent);
    if (agent) {
      consumedAgentIntent.current = urlAgent;
      startNewChat(agent, { fromIntent: true });
      return;
    }
    if (c.agentsSettled) {
      consumedAgentIntent.current = urlAgent;
      toast.error(t(($) => $.page.agent_link_no_access));
      replace(wsPaths.chat());
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- consume when the URL param or the resolving agent list changes
  }, [isCurrentChatRoute, workspaceSlug, urlAgent, c.availableAgents, c.agentsSettled]);

  const newChatButton = (
    <NewChatButton
      agents={c.availableAgents}
      userId={c.user?.id}
      onStart={startNewChat}
      side="bottom"
    />
  );

  const listHeader = (
    <PageHeader>
      <h1 className="flex-1 text-body font-semibold">{t(($) => $.page.title)}</h1>
      {newChatButton}
    </PageHeader>
  );

  // A message hit pushes the same `?session=&message=` entry a decision card
  // links to, so the existing one-shot jump scrolls to the message and Back
  // returns to the list with the search still in the field. A title hit is a
  // plain selection.
  const handleOpenSearchHit = (hit: ChatSearchHit) => {
    const session = c.sessions.find((s) => s.id === hit.session_id);
    if (!hit.message_id) {
      if (session) handleSelect(session);
      return;
    }
    supersedeAgentIntent();
    push(`${wsPaths.chat()}?session=${hit.session_id}&message=${hit.message_id}`);
  };

  const listBody = (
    <div className="px-2 py-1">
      <ChatThreadList
        sessions={c.sessions}
        agents={c.agents}
        activeSessionId={c.activeSessionId}
        onSelectSession={handleSelect}
        onArchive={handleArchive}
        onOpenSearchHit={handleOpenSearchHit}
      />
    </div>
  );

  // The conversation pane: message list / skeleton / empty above a persistent
  // banner + input. Identical composition to the floating window's body, so a
  // brand-new chat (no active session) shows the agent-aware empty state + input.
  // No compose-box agent selector — the agent is fixed when the chat starts.
  // `@container`: the conversation column's gutter (CHAT_GUTTER) widens with
  // THIS pane, which the user resizes independently of the browser window.
  const queuedTasks = c.pendingTask?.queued_tasks ?? [];
  const autoCompact = resolveAutoCompact(workspace?.settings, c.activeAgent?.runtime_config);
  const autoCompactThresholdPct = autoCompact.enabled ? autoCompact.threshold_pct : null;
  const { quoteRequest, quote, handleQuoteApplied } = useQuoteRequest();
  const annotation = useChatAnnotations({
    sessionId: c.activeSessionId,
    messages: c.messages,
    agentName: c.activeAgent?.name,
    enabled:
      !c.isSessionArchived &&
      !c.isAgentArchived &&
      !c.isAgentAccessRevoked &&
      c.isAgentRuntimeBound &&
      !c.noAgent,
  });
  const conversation = (
    <ChatAnnotationContext.Provider value={annotation.scope}>
    <div
      ref={annotation.cardRef}
      {...annotation.captureProps}
      className="flex flex-1 flex-col min-h-0 @container"
    >
      {annotation.popup}
      {c.currentSession && (
        <ChatSessionHeader
          session={c.currentSession}
          agent={c.activeAgent}
          onArchive={handleArchive}
          onUnarchive={dm.unarchive}
          onTogglePin={dm.togglePin}
          onOpenStatistics={() => dm.openStatistics(c.currentSession?.id ?? "")}
          renameRequest={renameRequest}
          autoCompactThresholdPct={autoCompactThresholdPct}
          onCompact={() => void c.handleSend("/compact")}
        />
      )}
      {c.showSkeleton ? (
        <ChatMessageSkeleton />
      ) : showsMessageList ? (
        <ChatMessageList
          key={c.activeSessionId}
          onQuoteMessage={quote}
          messages={c.messages}
          continuedFromSessionId={c.currentSession?.continued_from_session_id}
          continuedIntoSessionId={c.currentSession?.continued_into_session_id}
          sessionId={c.activeSessionId}
          forkedFromSessionId={c.currentSession?.forked_from_session_id}
          forkLastMessageId={c.currentSession?.fork_last_message_id}
          forkMessageCount={c.currentSession?.fork_message_count}
          sessions={c.sessionsLoaded ? c.sessions : undefined}
          pendingTask={c.pendingTask}
          availability={c.availability}
          firstItemIndex={c.firstItemIndex}
          hasOlderMessages={c.hasOlderMessages}
          isFetchingOlderMessages={c.isFetchingOlderMessages}
          onLoadOlderMessages={() => void c.fetchOlderMessages()}
          jumpToMessageId={urlSession === c.activeSessionId ? urlMessage : null}
          onJumpSettled={settleJump}
          onQuickAction={(action) => c.handleSend(action.prompt)}
          quickActionsDisabled={
            !!c.pendingTaskId ||
            c.isSessionArchived ||
            c.isAgentArchived ||
            c.isAgentAccessRevoked ||
            !c.isAgentRuntimeBound ||
            c.noAgent
          }
          onRegenerateQuickActions={(message) =>
            c.activeSessionId
              ? regenerateQuickActions.mutateAsync({
                  sessionId: c.activeSessionId,
                  messageId: message.id,
                })
              : undefined
          }
          quickActionsPendingMessageId={quickActionsPending?.message_id ?? null}
        />
      ) : (
        <EmptyState
          agent={c.activeAgent}
          hasSessions={c.sessions.length > 0}
          onPickPrompt={c.sendConversationStarter}
          customizeHref={c.customizeConversationStartersHref}
        />
      )}

      {c.isAgentAccessRevoked ? (
        <AgentAccessRevokedBanner agentName={c.activeAgent?.name} />
      ) : c.noAgent ? (
        <NoAgentBanner />
      ) : c.isAgentArchived ? (
        <ArchivedAgentBanner agentName={c.activeAgent?.name} />
      ) : !c.isAgentRuntimeBound && c.activeAgent ? (
        <RuntimeRequiredBanner
          agentId={c.activeAgent.id}
          agentName={c.activeAgent.name}
        />
      ) : (
        <OfflineBanner agentName={c.activeAgent?.name} availability={c.availability} />
      )}

      <ChatQueue
        tasks={queuedTasks}
        headStatus={c.pendingTask?.status}
        onSendNow={c.handleSendQueuedTaskNow}
        sendNowDisabled={c.isAgentAccessRevoked}
        onEdit={c.handleEditQueuedTask}
        onRemove={c.handleRemoveQueuedTask}
        onClear={c.handleClearQueuedTasks}
      />

      <ChatInput
        {...dm.inputProps}
        restoreDraftRequest={c.restoreDraftRequest}
        quoteRequest={quoteRequest}
        onQuoteApplied={handleQuoteApplied}
        conversationStarterRequest={c.conversationStarterRequest}
        onConversationStarterApplied={c.handleConversationStarterApplied}
        onRestoreDraftApplied={c.handleRestoreDraftApplied}
        uploadEnabled={c.uploadEnabled && !c.isAgentAccessRevoked}
        onStop={c.handleStop}
        isRunning={!!c.pendingTaskId}
        allowSubmitWhileRunning={c.pendingTask?.supports_queue === true}
        disabled={
          c.isAgentArchived ||
          c.isAgentAccessRevoked ||
          !c.isAgentRuntimeBound ||
          c.currentSession?.compacting === true
        }
        compacting={c.currentSession?.compacting === true}
        noAgent={c.noAgent}
        agentArchived={c.isAgentArchived}
        agentAccessRevoked={c.isAgentAccessRevoked}
        agentRuntimeRequired={!c.isAgentRuntimeBound}
        agentName={c.activeAgent?.name}
        projects={c.projects}
        projectId={c.activeProjectId}
        projectContextUnsupported={c.projectContextUnsupported}
        onProjectChange={changeProjectContext}
        isProjectUpdating={c.isProjectUpdating}
        focusRequest={c.focusInputRequest}
        annotationScope={annotation.input}
      />
    </div>
    {dm.dialogs}
    </ChatAnnotationContext.Provider>
  );

  if (c.interactiveLoginRequired) return <InteractiveLoginBanner />;

  // -- Compact: list / conversation toggle -----------------------------------
  if (isCompact) {
    if (c.activeSessionId || composingNew) {
      return (
        <div className="flex flex-1 flex-col min-h-0">
          <div className="flex h-12 shrink-0 items-center border-b px-2">
            <Button
              variant="ghost"
              size="sm"
              onClick={() => {
                // Back follows real history; the list is only the fallback.
                if (c.activeSessionId || urlNew) backOrReplace(wsPaths.chat());
              }}
              className="gap-1.5 text-muted-foreground"
            >
              <ArrowLeft className="h-4 w-4" />
              {t(($) => $.page.title)}
            </Button>
          </div>
          {conversation}
        </div>
      );
    }
    return (
      <div className="flex flex-1 flex-col min-h-0">
        {listHeader}
        <div className="flex-1 min-h-0 overflow-y-auto">{listBody}</div>
      </div>
    );
  }

  // -- Desktop: resizable two-panel. The conversation pane appears only once
  // there is a chat target — an open thread or a new chat whose agent was just
  // picked via ⊕. With nothing selected there is no agent, so we show a neutral
  // prompt instead of an orphaned compose box. -------------------------------
  const hasTarget = !!c.activeSessionId || composingNew;
  return (
    <ResizablePanelGroup
      orientation="horizontal"
      className="flex-1 min-h-0"
      defaultLayout={defaultLayout}
      onLayoutChanged={onLayoutChanged}
    >
      <ResizablePanel
        id="list"
        defaultSize={260}
        minSize={240}
        maxSize={480}
        groupResizeBehavior="preserve-pixel-size"
      >
        <div className="flex flex-col border-r h-full">
          {listHeader}
          <div className="flex-1 min-h-0 overflow-y-auto">{listBody}</div>
        </div>
      </ResizablePanel>
      <ResizableHandle />
      <ResizablePanel id="detail" minSize="40%">
        <div className="flex flex-col min-h-0 h-full">
          {hasTarget ? (
            conversation
          ) : (
            <div className="flex h-full flex-col items-center justify-center gap-3 text-muted-foreground">
              <MessageSquare className="h-10 w-10 text-faint-foreground" />
              <p className="text-body">{t(($) => $.page.select_prompt)}</p>
            </div>
          )}
        </div>
      </ResizablePanel>
    </ResizablePanelGroup>
  );
}
