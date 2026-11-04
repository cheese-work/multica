/**
 * Edit project title / description / icon. Modal presentation, configured
 * in `[workspace]/_layout.tsx`. Save button in the header runs an
 * optimistic `useUpdateProject`; the modal dismisses on success.
 *
 * Cancel/dismiss flow: header Cancel + iOS drag-down gesture both check
 * dirty state and pop an Alert if there are unsaved edits.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
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
import { AutosizeTextArea } from "@/components/ui/autosize-textarea";
import {
  MIN_BODY_INPUT_HEIGHT_PX,
  MOBILE_PLACEHOLDER_COLOR,
} from "@/components/ui/input-tokens";
import { projectDetailOptions } from "@/data/queries/projects";
import { useUpdateProject } from "@/data/mutations/projects";
import {
  projectEditKey,
  readFormDraft,
  useFormDraftsStore,
  type EditDraft,
} from "@/data/stores/form-drafts-store";
import { useWorkspaceStore } from "@/data/workspace-store";
import { useT } from "@/lib/i18n";

export default function EditProject() {
  const { id } = useLocalSearchParams<{ id: string }>();
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const { t } = useT("projects");
  const detail = useQuery(projectDetailOptions(wsId, id));
  const update = useUpdateProject(id);

  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [icon, setIcon] = useState("");
  const [seeded, setSeeded] = useState(false);
  const draftKey = projectEditKey(id);
  const draftsReady = useFormDraftsStore((s) => s.ready);
  // Set once a save succeeded, so the persist effect cannot re-save the draft.
  const settled = useRef(false);

  // Seed once when data arrives, before committing the form. Background
  // refetches must not overwrite edits already in progress. Waits for the
  // saved draft to load; it wins over the server values.
  if (detail.data && draftsReady && !seeded) {
    const draft = readFormDraft<EditDraft>(draftKey);
    setTitle(draft?.title ?? detail.data.title);
    setDescription(draft?.description ?? detail.data.description ?? "");
    setIcon(draft?.icon ?? detail.data.icon ?? "");
    setSeeded(true);
  }

  const dirty = useMemo(() => {
    if (!detail.data) return false;
    return (
      title.trim() !== detail.data.title ||
      description.trim() !== (detail.data.description ?? "") ||
      icon.trim() !== (detail.data.icon ?? "")
    );
  }, [detail.data, title, description, icon]);

  const canSave =
    seeded && title.trim().length > 0 && dirty && !update.isPending;

  useEffect(() => {
    if (!seeded || settled.current) return;
    const { setDraft, clearDraft } = useFormDraftsStore.getState();
    if (dirty) setDraft(draftKey, { title, description, icon });
    else clearDraft(draftKey);
  }, [seeded, dirty, title, description, icon, draftKey]);

  const onCancel = useCallback(() => {
    if (!dirty) {
      router.back();
      return;
    }
    Alert.alert(
      t("form.discard_title"),
      t("form.discard_message"),
      [
        { text: t("form.keep_editing"), style: "cancel" },
        {
          text: t("common:actions.discard"),
          style: "destructive",
          onPress: () => {
            settled.current = true;
            useFormDraftsStore.getState().clearDraft(draftKey);
            router.back();
          },
        },
      ],
    );
  }, [dirty, draftKey, t]);

  const onSave = useCallback(() => {
    if (!canSave) return;
    const patch = {
      title: title.trim(),
      description: description.trim() || null,
      icon: icon.trim() || null,
    };
    update.mutate(patch, {
      onSuccess: () => {
        settled.current = true;
        useFormDraftsStore.getState().clearDraft(draftKey);
        router.back();
      },
      onError: (err) => {
        Alert.alert(
          t("form.save_failed_edit"),
          err instanceof Error ? err.message : t("common:states.error"),
        );
      },
    });
  }, [canSave, title, description, icon, update, draftKey, t]);

  const headerLeft = useCallback(() => {
    return (
      <Pressable onPress={onCancel} className="px-1 py-1">
        <Text className="text-base text-brand">{t("common:actions.cancel")}</Text>
      </Pressable>
    );
  }, [onCancel, t]);

  const headerRight = useCallback(() => {
    return (
      <Pressable
        onPress={onSave}
        disabled={!canSave}
        className={canSave ? "px-1 py-1" : "px-1 py-1 opacity-40"}
      >
        <Text className="text-base text-brand font-semibold">
          {update.isPending ? t("actions.saving") : t("common:actions.save")}
        </Text>
      </Pressable>
    );
  }, [canSave, onSave, update.isPending, t]);

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
          {!detail.data ? (
            <Text className="text-sm text-muted-foreground">
              {t("common:states.loading")}
            </Text>
          ) : (
            <>
              <Field label={t("form.icon_emoji")}>
                <TextInput
                  value={icon}
                  onChangeText={(v) => {
                    // Cap at two characters — emoji are usually 1-2 UTF-16
                    // code units. Prevents the user typing a full sentence
                    // by accident.
                    setIcon(v.slice(0, 4));
                  }}
                  placeholder="📦"
                  placeholderTextColor={MOBILE_PLACEHOLDER_COLOR}
                  className="text-2xl text-foreground bg-secondary/50 rounded-md px-3 py-2 self-start min-w-[60px] text-center"
                  maxLength={4}
                />
              </Field>

              <Field label={t("form.title")}>
                <TextInput
                  value={title}
                  onChangeText={setTitle}
                  placeholder={t("form.title_placeholder")}
                  placeholderTextColor={MOBILE_PLACEHOLDER_COLOR}
                  className="text-base text-foreground bg-secondary/50 rounded-md px-3 py-2"
                  autoFocus={!detail.data?.title}
                  returnKeyType="next"
                />
              </Field>

              <Field label={t("form.description")}>
                <AutosizeTextArea
                  value={description}
                  onChangeText={setDescription}
                  placeholder={t("form.description_placeholder")}
                  className="bg-secondary/50 rounded-md px-3 py-2"
                  minHeight={MIN_BODY_INPUT_HEIGHT_PX}
                />
              </Field>
            </>
          )}
        </ScrollView>
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
