/**
 * Chat DM Statistics sheet — `formSheet` modal at `/[workspace]/chat-statistics`.
 *
 * Scope: single DM session only (`CHE-1760`; chain totals are `CHE-1782`).
 * Reuses `@multica/core/chat/usage`'s `summarizeChatSessionUsage` via
 * `deriveChatStatisticsState`. Never renders `$0` or `0` for unknown/failed
 * usage.
 */
import { useMemo } from "react";
import { Pressable, ScrollView, View } from "react-native";
import { useLocalSearchParams, useRouter } from "expo-router";
import { useQuery } from "@tanstack/react-query";
import { SafeAreaView } from "react-native-safe-area-context";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Text } from "@/components/ui/text";
import {
  chatSessionsOptions,
  chatSessionUsageOptions,
} from "@/data/queries/chat";
import { useChatSessionPickerStore } from "@/data/stores/chat-session-picker-store";
import { useWorkspaceStore } from "@/data/workspace-store";
import {
  deriveChatStatisticsState,
  formatStatsDuration,
  formatStatsTokens,
  formatStatsUsd,
  resolveRelatedSessions,
} from "@/lib/chat-dm-stats-archive";
import { useT } from "@/lib/i18n";
import { cn } from "@/lib/utils";

function isInteractiveLoginError(err: unknown): boolean {
  if (!err || typeof err !== "object") return false;
  if ("body" in err) {
    const body = (err as { body?: unknown }).body;
    if (
      body &&
      typeof body === "object" &&
      "code" in body &&
      (body as { code?: unknown }).code === "interactive_login_required"
    ) {
      return true;
    }
  }
  if (err instanceof Error) {
    return err.message.includes("interactive_login_required");
  }
  return false;
}

export default function ChatStatisticsSheet() {
  const router = useRouter();
  const { t } = useT("chat");
  const params = useLocalSearchParams<{ sessionId?: string }>();
  const storeSessionId = useChatSessionPickerStore((s) => s.activeSessionId);
  const requestSelect = useChatSessionPickerStore((s) => s.requestSelect);
  const sessionId = params.sessionId ?? storeSessionId;

  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const usage = useQuery(chatSessionUsageOptions(wsId, sessionId));
  const { data: sessions = [] } = useQuery(chatSessionsOptions(wsId));

  const currentSession = useMemo(
    () => sessions.find((s) => s.id === sessionId) ?? null,
    [sessions, sessionId],
  );

  const state = useMemo(
    () =>
      deriveChatStatisticsState({
        sessionId,
        data: usage.data,
        isFetched: usage.isFetched,
        isError: usage.isError,
        error: usage.error,
        dataUpdatedAt: usage.dataUpdatedAt,
        noValueLabel: t("stats.no_value"),
      }),
    [
      sessionId,
      usage.data,
      usage.isFetched,
      usage.isError,
      usage.error,
      usage.dataUpdatedAt,
      t,
    ],
  );

  const validUsage =
    usage.data && sessionId && usage.data.session_id === sessionId
      ? usage.data
      : null;

  const relatedItems = useMemo(
    () =>
      resolveRelatedSessions(
        validUsage ?? currentSession,
        sessions,
        t("stats.previous_dm"),
      ),
    [validUsage, currentSession, sessions, t],
  );

  const sortedModels = useMemo(() => {
    if (!state.summary) return [];
    return [...state.summary.models].sort(
      (a, b) => (b.costUsd ?? -1) - (a.costUsd ?? -1),
    );
  }, [state.summary]);

  const costBadgeLabel = useMemo(() => {
    switch (state.costKind) {
      case "none":
        return t("stats.no_cost");
      case "reported":
        return t("stats.reported");
      case "estimated_partial":
        return `${t("stats.estimated")} · ${t("stats.partial")}`;
      case "estimated":
        return t("stats.estimated");
    }
  }, [state.costKind, t]);

  const runsLabel = useMemo(() => {
    if (!state.summary) return "";
    const parts: string[] = [];
    if (state.summary.runs.completed > 0) {
      parts.push(
        t("stats.runs_completed", { count: state.summary.runs.completed }),
      );
    }
    if (state.summary.runs.failed > 0) {
      parts.push(t("stats.runs_failed", { count: state.summary.runs.failed }));
    }
    if (state.summary.runs.cancelled > 0) {
      parts.push(
        t("stats.runs_cancelled", { count: state.summary.runs.cancelled }),
      );
    }
    if (parts.length === 0) {
      parts.push(t("stats.runs_completed", { count: 0 }));
    }
    return parts.join(" · ");
  }, [state.summary, t]);

  const handleOpenRelated = (targetId: string) => {
    requestSelect(targetId);
    router.back();
  };

  return (
    <SafeAreaView
      edges={["bottom"]}
      testID="mobile-stats-sheet"
      className="flex-1 bg-background"
    >
      {/* Sheet header */}
      <View className="flex-row items-center justify-between border-b border-border px-4 pt-5 pb-3">
        <View className="flex-1 pr-3">
          <Text className="text-base font-semibold">{t("stats.title")}</Text>
          <Text className="text-xs text-muted-foreground">
            {t("stats.scope")}
          </Text>
        </View>
        <Button
          variant="ghost"
          size="sm"
          onPress={() => router.back()}
          accessibilityLabel={t("stats.close")}
          className="min-h-[44px] px-3"
        >
          <Text className="text-sm font-medium">{t("stats.close")}</Text>
        </Button>
      </View>

      <ScrollView
        className="flex-1"
        contentContainerStyle={{ padding: 16, gap: 16 }}
      >
        {state.kind === "loading" ? (
          <View testID="mobile-stats-state-loading" className="gap-3">
            <Skeleton className="h-9 w-32" />
            <Skeleton className="h-4 w-56" />
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-24 w-full" />
          </View>
        ) : state.kind === "error" ? (
          <View
            testID="mobile-stats-state-error"
            accessibilityRole="alert"
            className="items-start gap-3 rounded-lg border border-border bg-card p-4"
          >
            <Text className="text-sm text-foreground">
              {isInteractiveLoginError(usage.error)
                ? t("stats.interactive_login_required")
                : t("stats.error")}
            </Text>
            <Button
              variant="outline"
              size="sm"
              onPress={() => void usage.refetch()}
              accessibilityLabel={t("stats.retry")}
              className="min-h-[44px] px-4"
            >
              <Text>{t("stats.retry")}</Text>
            </Button>
          </View>
        ) : state.summary ? (
          <View className="gap-4">
            {state.refreshFailed ? (
              <View
                testID="mobile-stats-state-offline"
                accessibilityRole="alert"
                className="flex-row items-center justify-between gap-3 rounded-lg bg-muted p-3"
              >
                <Text className="flex-1 text-xs text-muted-foreground">
                  {t("stats.offline", { time: state.updatedAtIct ?? "" })}
                </Text>
                <Button
                  variant="outline"
                  size="sm"
                  onPress={() => void usage.refetch()}
                  accessibilityLabel={t("stats.retry")}
                  className="min-h-[44px] px-3"
                >
                  <Text className="text-xs">{t("stats.retry")}</Text>
                </Button>
              </View>
            ) : null}

            <View className={cn("gap-4", state.refreshFailed && "opacity-60")}>
              {/* Headline Cost + Tokens */}
              <View className="gap-1 rounded-lg border border-border bg-card p-4">
                <View className="flex-row items-baseline gap-2">
                  <Text
                    testID="mobile-stats-cost"
                    className="text-2xl font-semibold text-foreground"
                  >
                    {state.costDisplay}
                  </Text>
                  <Text className="text-xs text-muted-foreground">
                    {costBadgeLabel}
                  </Text>
                </View>

                {state.summary.hasUsage ? (
                  <Text className="text-xs text-muted-foreground">
                    {t("stats.tokens", {
                      tokens: formatStatsTokens(state.summary.totalTokens),
                    })}
                    {state.summary.unreportedRuns > 0
                      ? ` · ${t("stats.usage_missing", {
                          count: state.summary.unreportedRuns,
                        })}`
                      : ""}
                  </Text>
                ) : null}

                {state.summary.runs.inProgress > 0 ? (
                  <Text className="text-xs text-muted-foreground">
                    {t("stats.in_progress", {
                      count: state.summary.runs.inProgress,
                    })}
                  </Text>
                ) : null}
              </View>

              {/* Runs & Duration or Empty State */}
              {state.kind === "empty" ? (
                <View
                  testID="mobile-stats-state-empty"
                  className="rounded-lg border border-border bg-card p-4"
                >
                  <Text className="text-sm text-muted-foreground">
                    {t("stats.no_runs")}
                  </Text>
                </View>
              ) : (
                <View className="gap-2 rounded-lg border border-border bg-card p-4">
                  <View className="flex-row items-center justify-between gap-4">
                    <Text className="text-sm text-muted-foreground">
                      {t("stats.runs")}
                    </Text>
                    <Text className="text-sm font-medium text-foreground">
                      {runsLabel}
                    </Text>
                  </View>
                  <View className="flex-row items-center justify-between gap-4">
                    <Text className="text-sm text-muted-foreground">
                      {t("stats.run_time")}
                    </Text>
                    <Text className="text-sm font-medium text-foreground">
                      {formatStatsDuration(state.summary.totalRunMs)}
                    </Text>
                  </View>
                </View>
              )}

              {/* Messages */}
              <View className="flex-row items-center justify-between gap-4 rounded-lg border border-border bg-card p-4">
                <Text className="text-sm text-muted-foreground">
                  {t("stats.messages")}
                </Text>
                <Text className="text-sm font-medium text-foreground">
                  {t("stats.messages_value", {
                    user: state.summary.messages.user,
                    agent: state.summary.messages.assistant,
                  })}
                </Text>
              </View>

              {/* By Model */}
              {sortedModels.length > 0 ? (
                <View className="gap-2 rounded-lg border border-border bg-card p-4">
                  <Text className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                    {t("stats.by_model")}
                  </Text>
                  {sortedModels.map((row) => {
                    const rawRow = validUsage?.by_model[row.model];
                    return (
                      <View
                        key={row.model}
                        className="gap-1 border-t border-border pt-2"
                      >
                        <View className="flex-row items-center justify-between gap-2">
                          <Text className="flex-1 text-sm font-medium text-foreground">
                            {row.model}
                          </Text>
                          <Text className="text-sm text-muted-foreground">
                            {formatStatsTokens(row.tokens)}
                          </Text>
                          <Text className="min-w-[56px] text-right text-sm font-medium text-foreground">
                            {row.costUsd === null
                              ? t("stats.no_value")
                              : formatStatsUsd(row.costUsd)}
                          </Text>
                        </View>
                        {rawRow ? (
                          <Text className="text-xs text-muted-foreground">
                            {t("stats.tokens_breakdown", {
                              input: formatStatsTokens(rawRow.tokens.input),
                              output: formatStatsTokens(rawRow.tokens.output),
                              cacheRead: formatStatsTokens(
                                rawRow.tokens.cache_read,
                              ),
                              cacheWrite: formatStatsTokens(
                                rawRow.tokens.cache_write,
                              ),
                            })}
                          </Text>
                        ) : null}
                      </View>
                    );
                  })}
                </View>
              ) : null}

              {/* Related DMs */}
              {relatedItems.length > 0 ? (
                <View className="gap-2 rounded-lg border border-border bg-card p-4">
                  <Text className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                    {t("stats.related")}
                  </Text>
                  {relatedItems.map((item) => {
                    const label =
                      item.kind === "continued_from"
                        ? t("stats.continued_from")
                        : item.kind === "continued_into"
                          ? t("stats.continued_into")
                          : t("stats.forked_from");
                    return (
                      <Pressable
                        key={`${item.kind}-${item.id}`}
                        onPress={() => handleOpenRelated(item.id)}
                        accessibilityRole="button"
                        accessibilityLabel={`${label} ${item.title}`}
                        className="min-h-[44px] flex-row items-center justify-between rounded-md px-2 py-2 active:bg-accent"
                      >
                        <Text className="text-sm text-muted-foreground">
                          {label}
                        </Text>
                        <Text
                          numberOfLines={1}
                          className="max-w-[60%] text-sm font-medium text-primary"
                        >
                          {item.title}
                        </Text>
                      </Pressable>
                    );
                  })}
                </View>
              ) : null}
            </View>
          </View>
        ) : null}
      </ScrollView>
    </SafeAreaView>
  );
}
