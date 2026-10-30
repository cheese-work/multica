/**
 * Chat tab — single-screen IA.
 *
 * Layout:
 *   View ─ Header(center: ChatTitleButton, right: ChatSessionActions)
 *        ─ (NoAgentBanner?)
 *        ─ KeyboardAvoidingView ─ ChatMessageList (includes live status
 *                                                  + timeline in its
 *                                                  ListFooterComponent)
 *                                ─ OfflineBanner
 *                                ─ ChatComposer
 *
 * Session switching, agent selection, and session deletion all happen
 * inside this screen via Modal sheets — there is no `/chat/[id]` sub-route.
 *
 * State (all local, none in Zustand):
 *   - activeSessionId   — which session is being viewed (null = new chat blank)
 *   - selectedAgentId   — overrides currentSession.agent_id when set (used
 *                         when starting a new chat with a freshly-picked agent)
 *   - sessionSheetOpen  — bottom modal visibility
 *   - agentPickerOpen   — bottom modal visibility
 *
 * Side effects:
 *   - useChatSessionRealtime(activeSessionId) for per-record WS events
 *   - auto markRead when entering a session with has_unread
 *   - ensureSession dedupe ref for concurrent first-message sends
 *
 * Optimistic send burst mirrors web's chat-window.tsx send sequence
 * (packages/views/chat/components/chat-window.tsx ~262-345):
 *   seed messages → seed pendingTask → flip activeSessionId → POST →
 *   patch pendingTask with server task_id + created_at.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Alert,
  KeyboardAvoidingView,
  Platform,
  Pressable,
  View,
} from "react-native";
import { router, useFocusEffect, useIsFocused } from "expo-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  Agent,
  ChatMessage,
  ChatPendingTask,
  ChatSession,
} from "@multica/core/types";
import {
  enqueuePendingChatTask,
  hideQueuedChatMessages,
  removePendingChatTask,
} from "@multica/core/chat/pending";
import { canAssignAgentToIssue } from "@multica/core/permissions";
import { api } from "@/data/api";
import { useAuthStore } from "@/data/auth-store";
import { useWorkspaceStore } from "@/data/workspace-store";
import { agentListOptions } from "@/data/queries/agents";
import { memberListOptions } from "@/data/queries/members";
import {
  chatKeys,
  chatMessagesOptions,
  chatSessionsOptions,
  pendingChatTaskOptions,
  taskMessagesOptions,
} from "@/data/queries/chat";
import {
  useCreateChatSession,
  useDeleteChatSession,
  useMarkChatSessionRead,
  useSetChatSessionArchived,
  useSetChatSessionPinned,
} from "@/data/mutations/chat";
import {
  DRAFT_NEW_SESSION,
  useChatDraftsStore,
} from "@/data/stores/chat-drafts-store";
import { useChatSessionActionsStore } from "@/data/stores/chat-session-actions-store";
import { useChatSessionPickerStore } from "@/data/stores/chat-session-picker-store";
import { useChatSessionRealtime } from "@/data/realtime/use-chat-session-realtime";
import {
  invalidatePendingTask,
  seedAcceptedPendingTask,
} from "@/data/realtime/chat-ws-updaters";
import {
  ArchivedSendInterceptedError,
  buildArchivedSendAlertButtons,
  splitChatSessions,
  unarchiveThenSendOnce,
} from "@/lib/chat-dm-stats-archive";
import { useWorkspaceAgentAvailability } from "@/lib/workspace-agent-availability";
import {
  dispatchReasonCode,
} from "@/lib/dispatch-reason";
import { useT } from "@/lib/i18n";
import { useAgentPresence } from "@/lib/use-agent-presence";
import { Header } from "@/components/ui/header";
import { Text } from "@/components/ui/text";
import { ChatTitleButton } from "@/components/chat/chat-title-button";
import { ChatSessionActions } from "@/components/chat/chat-session-actions";
import { ChatMessageList } from "@/components/chat/chat-message-list";
import { ChatComposer } from "@/components/chat/chat-composer";
import { AgentPickerSheet } from "@/components/chat/agent-picker-sheet";
import { NoAgentBanner } from "@/components/chat/no-agent-banner";
import { OfflineBanner } from "@/components/chat/offline-banner";
import { RuntimeRequiredBanner } from "@/components/chat/runtime-required-banner";
import { useChatSelectStore } from "@/data/chat-select-store";
import { isAgentRuntimeBound } from "@/lib/is-agent-runtime-bound";

export default function ChatTab() {
  const qc = useQueryClient();
  const { t } = useT("chat");
  const sendFailureMessage = useCallback((err: unknown) => {
    switch (dispatchReasonCode(err)) {
      case "invocation_not_allowed":
        return t("failure.invocation_not_allowed");
      case "agent_runtime_required":
        return t("failure.agent_runtime_required");
      case "runtime_access_denied":
        return t("failure.runtime_access_denied", {
          detail: t("failure.runtime_access_recovery"),
        });
      default:
        return t("failure.default");
    }
  }, [t]);
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const wsSlug = useWorkspaceStore((s) => s.currentWorkspaceSlug);
  const userId = useAuthStore((s) => s.user?.id);

  const [activeSessionId, setActiveSessionId] = useState<string | null>(null);
  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null);
  const [agentPickerOpen, setAgentPickerOpen] = useState(false);

  // Bridge to the chat-sessions formSheet route. Mirror local
  // activeSessionId into the store so the picker can render the current
  // selection's check mark; consume the picker's one-shot select request
  // via useEffect.
  const setStoreActiveSessionId = useChatSessionPickerStore(
    (s) => s.setActiveSessionId,
  );
  const selectRequest = useChatSessionPickerStore((s) => s.selectRequest);
  const consumeSelect = useChatSessionPickerStore((s) => s.consumeSelect);
  useEffect(() => {
    setStoreActiveSessionId(activeSessionId);
  }, [activeSessionId, setStoreActiveSessionId]);

  // ── Server state ───────────────────────────────────────────────────────
  const { data: sessions = [] } = useQuery(chatSessionsOptions(wsId));
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: members = [] } = useQuery(memberListOptions(wsId));

  const { active: activeSessions } = useMemo(
    () => splitChatSessions(sessions),
    [sessions],
  );

  // ── Auto-hydrate active session on first Chat tab entry ────────────────
  // Mobile-only deviation from web: web's chat-window opens to an empty
  // state when no `activeSessionId` is persisted; on a phone, picking
  // a session is 4 taps, so jump straight to the most recent active session.
  // Hydration is one-shot per workspace.
  const [hydratedWs, setHydratedWs] = useState<string | null>(null);
  if (wsId && hydratedWs !== wsId) {
    setHydratedWs(wsId);
    if (sessions.length > 0) setActiveSessionId((activeSessions[0] ?? sessions[0]).id);
  }
  const { data: messages = [], isLoading: messagesLoading } = useQuery(
    chatMessagesOptions(activeSessionId),
  );
  const { data: pendingTask } = useQuery(
    pendingChatTaskOptions(activeSessionId),
  );
  const visibleMessages = hideQueuedChatMessages(messages, pendingTask);
  // Live execution trace for the in-flight task. `task:message` WS events
  // append rows to this same cache key via `appendTaskMessage`, so the
  // list/pill stay in sync without a polling fetch. `enabled` is gated by
  // `isTaskMessageTaskId` inside taskMessagesOptions — optimistic ids
  // never hit the network.
  const { data: liveTaskMessages = [] } = useQuery(
    taskMessagesOptions(pendingTask?.task_id),
  );

  // ── Derived ────────────────────────────────────────────────────────────
  const memberRole = useMemo(
    () => members.find((m) => m.user_id === userId)?.role ?? null,
    [members, userId],
  );

  // The picker must list only agents this user can actually TRIGGER — sending
  // a message enqueues a run, so it clears the server's invoke gate
  // (`canInvokeAgent`), which has no admin bypass. Shared rule, not a mobile
  // copy: a local mirror drifted from it and let admins pick a teammate's
  // personal agent only to be 403'd on send (MUL-6380 / GH #7180).
  const availableAgents = useMemo(
    () =>
      agents.filter(
        (a) =>
          !a.archived_at &&
          canAssignAgentToIssue(a, { userId: userId ?? null, role: memberRole })
            .allowed,
      ),
    [agents, userId, memberRole],
  );

  const activeSession = useMemo(
    () => sessions.find((s) => s.id === activeSessionId) ?? null,
    [sessions, activeSessionId],
  );

  // Active agent: explicit selection wins; otherwise inherit from the
  // active session; otherwise pick the first available agent.
  const currentAgent: Agent | null = useMemo(() => {
    if (selectedAgentId) {
      return availableAgents.find((a) => a.id === selectedAgentId) ?? null;
    }
    if (activeSession) {
      return agents.find((a) => a.id === activeSession.agent_id) ?? null;
    }
    return availableAgents[0] ?? null;
  }, [selectedAgentId, availableAgents, activeSession, agents]);

  // A session outlives the permission that created it: the agent can be flipped
  // to personal, change owner, or drop this member from its allow-list, and the
  // server then refuses every send with `invocation_not_allowed` while still
  // serving the transcript (MUL-4525 — read uses the view gate, send re-runs the
  // invoke gate). `currentAgent` deliberately resolves an open session's agent
  // from the FULL list so the header stays honest, which means the picker filter
  // above cannot cover this case — judge the bound agent too (MUL-6380).
  const accessRevoked =
    currentAgent !== null &&
    !canAssignAgentToIssue(currentAgent, {
      userId: userId ?? null,
      role: memberRole,
    }).allowed;

  const availability = useWorkspaceAgentAvailability();
  const presenceDetail = useAgentPresence(wsId, currentAgent?.id);
  const presenceAvailability =
    presenceDetail === "loading" ? undefined : presenceDetail.availability;
  const isArchived = activeSession?.status === "archived";
  const runtimeBound =
    currentAgent !== null && isAgentRuntimeBound(currentAgent);
  const sending = !!pendingTask?.task_id;

  // ── Drafts ─────────────────────────────────────────────────────────────
  const draftKey = activeSessionId ?? DRAFT_NEW_SESSION;
  const draft = useChatDraftsStore((s) => s.drafts[draftKey] ?? "");
  const setDraft = useChatDraftsStore((s) => s.setDraft);
  const clearDraft = useChatDraftsStore((s) => s.clearDraft);
  const promoteNewDraft = useChatDraftsStore((s) => s.promoteNewDraft);

  // ── Realtime ───────────────────────────────────────────────────────────
  useChatSessionRealtime(activeSessionId, () => {
    setActiveSessionId(null);
  });

  // Exit text-selection mode whenever the chat tab loses focus. Expo
  // Router bottom tabs stay mounted across tab switches, so a plain
  // useEffect cleanup wouldn't fire — useFocusEffect is the navigation-
  // aware equivalent.
  useFocusEffect(
    useCallback(() => () => useChatSelectStore.getState().clear(), []),
  );

  // ── Auto markRead while viewing a session with unread state ──────────
  const isFocused = useIsFocused();
  const markRead = useMarkChatSessionRead();
  useEffect(() => {
    if (!isFocused) return;
    if (!activeSessionId) return;
    if (!activeSession?.has_unread) return;
    markRead.mutate(activeSessionId);
  }, [isFocused, activeSessionId, activeSession?.has_unread, markRead]);

  // ── Mutations ──────────────────────────────────────────────────────────
  const createSession = useCreateChatSession();
  const deleteSession = useDeleteChatSession();
  const setPinned = useSetChatSessionPinned();
  const setArchived = useSetChatSessionArchived();

  const actionPending = useChatSessionActionsStore((s) =>
    activeSession ? (s.pendingBySession[activeSession.id] ?? null) : null,
  );
  const undoToast = useChatSessionActionsStore((s) => s.undoToast);
  const beginSessionAction = useChatSessionActionsStore(
    (s) => s.beginSessionAction,
  );
  const endSessionAction = useChatSessionActionsStore(
    (s) => s.endSessionAction,
  );
  const showArchiveUndo = useChatSessionActionsStore((s) => s.showArchiveUndo);
  const dismissArchiveUndo = useChatSessionActionsStore(
    (s) => s.dismissArchiveUndo,
  );

  const handleUnarchive = useCallback(
    async (session: ChatSession): Promise<boolean> => {
      if (!beginSessionAction(session.id, "unarchive")) return false;
      try {
        await setArchived.mutateAsync({
          sessionId: session.id,
          archived: false,
        });
        dismissArchiveUndo();
        return true;
      } catch (err) {
        Alert.alert(
          t("sessions.unarchive_failed"),
          err instanceof Error ? err.message : undefined,
        );
        return false;
      } finally {
        endSessionAction(session.id);
      }
    },
    [beginSessionAction, endSessionAction, setArchived, dismissArchiveUndo, t],
  );

  const handleArchive = useCallback(
    async (session: ChatSession): Promise<boolean> => {
      if (!beginSessionAction(session.id, "archive")) return false;
      try {
        await setArchived.mutateAsync({
          sessionId: session.id,
          archived: true,
        });
        if (activeSessionId === session.id) {
          const nextActive = activeSessions.find((s) => s.id !== session.id);
          setActiveSessionId(nextActive?.id ?? null);
        }
        showArchiveUndo({
          sessionId: session.id,
          title: session.title || t("sessions.new_chat"),
        });
        return true;
      } catch (err) {
        Alert.alert(
          t("sessions.archive_failed"),
          err instanceof Error ? err.message : undefined,
        );
        return false;
      } finally {
        endSessionAction(session.id);
      }
    },
    [
      beginSessionAction,
      endSessionAction,
      setArchived,
      activeSessionId,
      activeSessions,
      showArchiveUndo,
      t,
    ],
  );

  const handleTogglePin = useCallback(
    async (session: ChatSession) => {
      try {
        await setPinned.mutateAsync({
          sessionId: session.id,
          pinned: !session.pinned,
        });
      } catch (err) {
        Alert.alert(
          t("sessions.pin_failed"),
          err instanceof Error ? err.message : undefined,
        );
      }
    },
    [setPinned, t],
  );

  const handleOpenStatistics = useCallback(
    (session: ChatSession) => {
      if (!wsSlug) return;
      router.push({
        pathname: "/[workspace]/chat-statistics",
        params: { workspace: wsSlug, sessionId: session.id },
      });
    },
    [wsSlug],
  );

  // ── Send burst ─────────────────────────────────────────────────────────
  const sessionPromiseRef = useRef<Promise<string | null> | null>(null);

  const ensureSession = useCallback(
    async (titleSeed: string): Promise<string | null> => {
      if (activeSessionId) return activeSessionId;
      if (!currentAgent) return null;
      if (sessionPromiseRef.current) return sessionPromiseRef.current;

      const promise = (async () => {
        try {
          const session = await createSession.mutateAsync({
            agent_id: currentAgent.id,
            title: titleSeed.slice(0, 50),
          });
          return session.id;
        } finally {
          sessionPromiseRef.current = null;
        }
      })();
      sessionPromiseRef.current = promise;
      return promise;
    },
    [activeSessionId, currentAgent, createSession],
  );

  const executeSend = useCallback(
    async (
      content: string,
      attachmentIds: string[] = [],
      options: { clearDraft?: boolean } = {},
    ) => {
      if (!currentAgent) return;
      if (accessRevoked) {
        Alert.alert(
          t("alerts.no_permission_title"),
          t("alerts.no_permission_message"),
        );
        return;
      }
      if (!runtimeBound) {
        Alert.alert(
          t("alerts.runtime_title"),
          t("alerts.runtime_message"),
        );
        return;
      }

      const isNewSession = !activeSessionId;
      let sessionId: string | null;
      try {
        sessionId = await ensureSession(content);
      } catch (err) {
        Alert.alert(t("alerts.not_sent"), sendFailureMessage(err));
        throw err;
      }
      if (!sessionId) return;

      const sentAt = new Date().toISOString();
      const optimistic: ChatMessage = {
        id: `optimistic-${Date.now()}`,
        chat_session_id: sessionId,
        role: "user",
        content,
        task_id: null,
        created_at: sentAt,
      };
      const optimisticTaskId = `optimistic-${optimistic.id}`;
      qc.setQueryData<ChatMessage[]>(chatKeys.messages(sessionId), (old) =>
        old ? [...old, optimistic] : [optimistic],
      );
      qc.setQueryData<ChatPendingTask>(
        chatKeys.pendingTask(sessionId),
        (old) =>
          enqueuePendingChatTask(
            old,
            {
              task_id: optimisticTaskId,
              status: "queued",
              created_at: sentAt,
              message_id: optimistic.id,
              content,
            },
            Boolean(old?.task_id),
          ),
      );
      if (isNewSession) {
        promoteNewDraft(sessionId);
        setActiveSessionId(sessionId);
      }

      try {
        const result = await api.sendChatMessage(sessionId, content, {
          attachmentIds: attachmentIds.length > 0 ? attachmentIds : undefined,
        });
        qc.setQueryData<ChatMessage[]>(chatKeys.messages(sessionId), (old) =>
          old?.map((message) =>
            message.id === optimistic.id
              ? {
                  ...message,
                  id: result.message_id,
                  task_id: result.task_id,
                  created_at: result.created_at,
                }
              : message,
          ),
        );
        seedAcceptedPendingTask(qc, {
          chat_session_id: sessionId,
          task_id: result.task_id,
          created_at: result.created_at,
          message_id: result.message_id,
          content,
          optimistic_task_id: optimisticTaskId,
          supports_queue: result.supports_queue,
          queued: result.queued,
        });
        qc.invalidateQueries({ queryKey: chatKeys.messages(sessionId) });
        if (options.clearDraft !== false) {
          clearDraft(sessionId);
        }
      } catch (err) {
        qc.setQueryData<ChatMessage[]>(chatKeys.messages(sessionId), (old) =>
          old ? old.filter((m) => m.id !== optimistic.id) : old,
        );
        qc.setQueryData<ChatPendingTask>(
          chatKeys.pendingTask(sessionId),
          (old) => removePendingChatTask(old, optimisticTaskId),
        );
        Alert.alert(t("alerts.not_sent"), sendFailureMessage(err));
        throw err;
      }
    },
    [
      activeSessionId,
      currentAgent,
      accessRevoked,
      runtimeBound,
      ensureSession,
      qc,
      sendFailureMessage,
      t,
      promoteNewDraft,
      clearDraft,
    ],
  );

  const handleSend = useCallback(
    async (
      content: string,
      attachmentIds: string[] = [],
      options: { clearDraft?: boolean } = {},
    ) => {
      if (activeSession?.status === "archived") {
        const archivedSession = activeSession;
        const newerSession = archivedSession.continued_into_session_id
          ? (sessions.find(
              (s) => s.id === archivedSession.continued_into_session_id,
            ) ?? null)
          : null;

        const messageParts = [
          t("archived_send.question"),
          newerSession
            ? t("archived_send.newer", {
                title: newerSession.title || t("sessions.new_chat"),
              })
            : null,
          t("archived_send.kept"),
        ].filter(Boolean);

        return new Promise<void>((resolve, reject) => {
          const confirmUnarchiveAndSend = () => {
            unarchiveThenSendOnce(
              () => handleUnarchive(archivedSession),
              () => executeSend(content, attachmentIds, options),
            ).then(resolve, reject);
          };

          const cancelIntercept = () => {
            reject(new ArchivedSendInterceptedError());
          };

          const buttons = buildArchivedSendAlertButtons({
            platform: Platform.OS,
            hasNewer: newerSession !== null,
            labels: {
              openNewer: t("archived_send.open_newer"),
              confirm: t("archived_send.confirm"),
              cancel: t("archived_send.cancel"),
            },
            onOpenNewer: () => {
              if (!newerSession) return;
              setSelectedAgentId(null);
              setActiveSessionId(newerSession.id);
              reject(new ArchivedSendInterceptedError());
            },
            onConfirm: confirmUnarchiveAndSend,
            onCancel: cancelIntercept,
          });

          Alert.alert(
            t("archived_send.title"),
            messageParts.join("\n\n"),
            buttons,
            { cancelable: true, onDismiss: cancelIntercept },
          );
        });
      }

      return executeSend(content, attachmentIds, options);
    },
    [activeSession, sessions, t, handleUnarchive, executeSend],
  );

  // ── Cancel in-flight ───────────────────────────────────────────────────
  const handleStop = useCallback(() => {
    if (!pendingTask?.task_id || !activeSessionId) return;
    if (pendingTask.status === "queued") return;
    const taskId = pendingTask.task_id;
    const sessionId = activeSessionId;
    qc.setQueryData<ChatPendingTask>(chatKeys.pendingTask(sessionId), (old) =>
      removePendingChatTask(old, taskId),
    );
    void api.cancelTaskById(taskId)
      .catch(() => {
        // Silent — task may have already terminated server-side.
      })
      .finally(() => invalidatePendingTask(qc, sessionId));
  }, [pendingTask, activeSessionId, qc]);

  // ── Header / sheet actions ─────────────────────────────────────────────
  const handleNewChat = useCallback(() => {
    if (availableAgents.length > 1) {
      setAgentPickerOpen(true);
      return;
    }
    setSelectedAgentId(null);
    setActiveSessionId(null);
  }, [availableAgents.length]);

  const handlePickAgent = useCallback((agent: Agent) => {
    setSelectedAgentId(agent.id);
    setActiveSessionId(null);
  }, []);

  // Apply the user's pick from the chat-sessions route (or "no session"
  // when they delete the active one in the sheet).
  const [appliedSelect, setAppliedSelect] = useState<typeof selectRequest>(null);
  if (selectRequest && selectRequest !== appliedSelect) {
    setAppliedSelect(selectRequest);
    setSelectedAgentId(null);
    setActiveSessionId(selectRequest.id);
  }
  useEffect(() => {
    if (selectRequest && selectRequest === appliedSelect) consumeSelect();
  }, [selectRequest, appliedSelect, consumeSelect]);

  const handleDeleteSession = useCallback(
    (session: ChatSession) => {
      if (session.status !== "archived") return;
      Alert.alert(
        t("alerts.delete_title"),
        session.title || t("sessions.new_chat"),
        [
          { text: t("common:actions.cancel"), style: "cancel" },
          {
            text: t("alerts.delete_confirm"),
            style: "destructive",
            onPress: () => {
              const id = session.id;
              if (activeSessionId === id) {
                setActiveSessionId(null);
              }
              deleteSession.mutate(id);
            },
          },
        ],
        { cancelable: true },
      );
    },
    [activeSessionId, deleteSession, t],
  );

  // ── Composer disabled-state ────────────────────────────────────────────
  const isArchiveActionPending = actionPending !== null;
  const disabled =
    !currentAgent ||
    accessRevoked ||
    availability === "none" ||
    isArchiveActionPending ||
    !runtimeBound;
  const disabledReason = !currentAgent
    ? t("composer.no_agent")
    : accessRevoked
      ? t("composer.revoked")
      : availability === "none"
        ? t("composer.no_agents")
        : actionPending === "archive"
          ? t("header.archiving")
          : actionPending === "unarchive"
            ? t("header.unarchiving")
            : !runtimeBound
              ? t("composer.runtime_required")
              : undefined;

  return (
    <View className="flex-1 bg-background">
      <Header
        center={
          <ChatTitleButton
            currentSession={activeSession}
            currentAgent={currentAgent}
            onPress={() => {
              if (!wsSlug) return;
              router.push({
                pathname: "/[workspace]/chat-sessions",
                params: { workspace: wsSlug },
              });
            }}
          />
        }
        right={
          <ChatSessionActions
            session={activeSession}
            onOpenStatistics={handleOpenStatistics}
            onTogglePin={(session) => void handleTogglePin(session)}
            onArchive={(session) => void handleArchive(session)}
            onUnarchive={(session) => void handleUnarchive(session)}
            onDelete={handleDeleteSession}
            onNewPress={handleNewChat}
          />
        }
      />
      {availability === "none" ? <NoAgentBanner /> : null}
      <KeyboardAvoidingView
        behavior={Platform.OS === "ios" ? "padding" : undefined}
        className="flex-1"
      >
        <ChatMessageList
          messages={visibleMessages}
          loading={messagesLoading}
          hasSessions={sessions.length > 0}
          agent={currentAgent}
          onPickPrompt={(text) => setDraft(draftKey, text)}
          onQuickAction={(action) =>
            handleSend(action.prompt, [], { clearDraft: false })
          }
          quickActionsDisabled={sending || disabled}
          pendingTask={pendingTask}
          liveTaskMessages={liveTaskMessages}
          availability={presenceAvailability}
        />
        {undoToast ? (
          <View className="mx-3 mb-2 flex-row items-center justify-between rounded-lg border border-border bg-card px-3 py-2 shadow-sm">
            <Text
              numberOfLines={1}
              className="flex-1 pr-3 text-xs text-foreground"
            >
              {t("sessions.archived_toast")} · {undoToast.title}
            </Text>
            <Pressable
              onPress={() => {
                const target = sessions.find(
                  (s) => s.id === undoToast.sessionId,
                );
                const fallbackSession: ChatSession = target ?? {
                  id: undoToast.sessionId,
                  workspace_id: wsId ?? "",
                  agent_id: currentAgent?.id ?? "",
                  creator_id: userId ?? "",
                  title: undoToast.title,
                  status: "archived",
                  has_unread: false,
                  created_at: "",
                  updated_at: "",
                };
                void handleUnarchive(fallbackSession).then((ok) => {
                  if (ok) {
                    setSelectedAgentId(null);
                    setActiveSessionId(undoToast.sessionId);
                  }
                });
              }}
              accessibilityRole="button"
              accessibilityLabel={t("sessions.undo")}
              className="min-h-[36px] justify-center rounded-md px-2 active:bg-accent"
            >
              <Text className="text-xs font-semibold text-primary">
                {t("sessions.undo")}
              </Text>
            </Pressable>
          </View>
        ) : null}
        {runtimeBound ? (
          <OfflineBanner
            agentName={currentAgent?.name}
            availability={presenceAvailability}
          />
        ) : currentAgent ? (
          <RuntimeRequiredBanner agentName={currentAgent.name} />
        ) : null}
        <ChatComposer
          value={draft}
          onChangeText={(next) => setDraft(draftKey, next)}
          onSend={handleSend}
          onStop={handleStop}
          sending={sending}
          allowStop={pendingTask?.status !== "queued"}
          disabled={disabled}
          disabledReason={disabledReason}
          archived={isArchived}
        />
      </KeyboardAvoidingView>

      <AgentPickerSheet
        visible={agentPickerOpen}
        agents={availableAgents}
        currentAgentId={currentAgent?.id ?? null}
        onPick={handlePickAgent}
        onClose={() => setAgentPickerOpen(false)}
      />
    </View>
  );
}
