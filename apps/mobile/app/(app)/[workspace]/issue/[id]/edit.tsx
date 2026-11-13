/**
 * Edit issue title / description. Modal presentation, configured in
 * `[workspace]/_layout.tsx`. Save runs the optimistic `useUpdateIssue`
 * mutation; modal dismisses on success.
 *
 * Mirrors `project/[id]/edit.tsx` so users get the same gesture on both
 * record types (cancel/save in header, dirty Alert on dismiss-while-dirty).
 *
 * Description uses `useMentionInput` + `<DescriptionField>` so the @-mention
 * pipeline matches `new-issue.tsx`. v1 note: existing mentions in the
 * server-side description render as raw markdown text while editing because
 * there's no markdown-to-marker deserializer yet — `serialize()` still
 * produces a valid round-trip since unparsed `[@name](mention://...)` literals
 * pass through unchanged. New @-mentions added during the edit get serialized
 * normally via the marker pipeline.
 *
 * Properties (status / priority / assignee / labels / project / due_date)
 * are NOT edited here — they have dedicated chip pickers on the detail page.
 * This screen only owns the two free-text fields.
 */
import { useCallback } from "react";
import {
  Alert,
  KeyboardAvoidingView,
  Platform,
  Pressable,
  ScrollView,
  TextInput,
  View,
} from "react-native";
import { Stack, router, useLocalSearchParams } from "expo-router";
import { useQuery } from "@tanstack/react-query";
import { Text } from "@/components/ui/text";
import { DescriptionField } from "@/components/issue/description-field";
import { MentionSuggestionBar } from "@/components/issue/mention-suggestion-bar";
import { MOBILE_PLACEHOLDER_COLOR } from "@/components/ui/input-tokens";
import { issueDetailOptions } from "@/data/queries/issues";
import { useUpdateIssue } from "@/data/mutations/issues";
import { buildIssueTextUpdate } from "@/data/issue-edit";
import { useIssueEditDraft } from "@/data/stores/use-form-drafts";
import { useWorkspaceStore } from "@/data/workspace-store";
import { useT } from "@/lib/i18n";

export default function EditIssue() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const { t } = useT("issues");
  const detail = useQuery(issueDetailOptions(wsId, id));
  const update = useUpdateIssue(id);

  const {
    title,
    setTitle,
    description,
    currentDescription,
    restored,
    dirty,
    settle,
  } = useIssueEditDraft(id, detail.data);

  const canSave =
    restored && title.trim().length > 0 && dirty && !update.isPending;

  const onCancel = useCallback(() => {
    if (!dirty) {
      router.back();
      return;
    }
    Alert.alert(
      t("form.discard_title"),
      t("form.discard_message_issue"),
      [
        { text: t("form.keep_editing"), style: "cancel" },
        {
          text: t("common:actions.discard"),
          style: "destructive",
          onPress: () => {
            settle();
            router.back();
          },
        },
      ],
    );
  }, [dirty, settle, t]);

  const onSave = useCallback(() => {
    if (!canSave) return;
    // `UpdateIssueRequest.description` is `string | undefined` — server
    // treats empty string as "clear the description", which is what we
    // want when the user wipes the field. Mobile deliberately omits strict
    // text baselines until this screen has a conflict reconciliation flow;
    // otherwise a real 409 would leave the draft in an unrecoverable retry
    // loop. Web/desktop keep baseline protection with their compare UI.
    const patch = buildIssueTextUpdate(title, currentDescription);
    update.mutate(patch, {
      onSuccess: () => {
        settle();
        router.back();
      },
      onError: (err) => {
        Alert.alert(
          t("form.save_failed"),
          err instanceof Error ? err.message : t("common:states.error"),
        );
      },
    });
  }, [canSave, title, currentDescription, update, settle, t]);

  const headerLeft = useCallback(
    () => (
      <Pressable onPress={onCancel} className="px-1 py-1">
      <Text className="text-base text-brand">{t("common:actions.cancel")}</Text>
      </Pressable>
    ),
    [onCancel, t],
  );

  const headerRight = useCallback(
    () => (
      <Pressable
        onPress={onSave}
        disabled={!canSave}
        className={canSave ? "px-1 py-1" : "px-1 py-1 opacity-40"}
      >
        <Text className="text-base text-brand font-semibold">
          {update.isPending ? t("edit.saving") : t("edit.save")}
        </Text>
      </Pressable>
    ),
    [canSave, onSave, update.isPending, t],
  );

  return (
    <>
      <Stack.Screen options={{ headerLeft, headerRight }} />
      <KeyboardAvoidingView
        className="flex-1 bg-background"
        behavior={Platform.OS === "ios" ? "padding" : undefined}
      >
        <ScrollView
          className="flex-1"
          contentContainerClassName="px-4 pt-4 pb-6 gap-4"
          keyboardShouldPersistTaps="handled"
        >
          {!detail.data || !restored ? (
            <Text className="text-sm text-muted-foreground">
              {t("common:states.loading")}
            </Text>
          ) : (
            <>
              <Field label={t("common:fields.title")}>
                <TextInput
                  value={title}
                  onChangeText={setTitle}
                  placeholder={t("new.title_placeholder")}
                  placeholderTextColor={MOBILE_PLACEHOLDER_COLOR}
                  className="text-base text-foreground bg-secondary/50 rounded-md px-3 py-2"
                  returnKeyType="next"
                  editable={!update.isPending}
                />
              </Field>

              <Field label={t("common:fields.description")}>
                <DescriptionField
                  description={description}
                  disabled={update.isPending}
                />
              </Field>
            </>
          )}
        </ScrollView>
        {/* Mention suggestion bar floats above the keyboard while the user
            is mid-@. Outside the ScrollView so it doesn't scroll with the
            form body. */}
        <MentionSuggestionBar {...description.suggestionBar} />
      </KeyboardAvoidingView>
    </>
  );
}

function Field({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <View className="gap-1.5">
      <Text className="text-xs uppercase tracking-wider text-muted-foreground">
        {label}
      </Text>
      {children}
    </View>
  );
}
