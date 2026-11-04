/**
 * New issue creation modal — manual only.
 *
 * Layout follows Apple Reminders / Linear iOS / Things 3: one vertical
 * scrolling form (title → description → property chips), no sticky bottom
 * toolbar. Property chips are part of the form, not pinned above keyboard.
 * MentionSuggestionBar floats above keyboard only when the user is mid-@.
 *
 * No markdown toolbar / upload buttons in v1: mobile users creating an
 * issue rarely format markdown, and attachment upload is deferred to a
 * later release (see plan-issue-majestic-rabin.md "skip uploads").
 *
 * Mention pipeline shares `useMentionInput` with `issue/[id]/new-comment.tsx`
 * — both surfaces produce canonical `[@name](mention://type/id)` markdown
 * recognised by util.ParseMentions on the server.
 */
import { useCallback, useEffect, useState } from "react";
import {
  Alert,
  KeyboardAvoidingView,
  Platform,
  ScrollView,
  TextInput,
} from "react-native";
import { Stack, router } from "expo-router";
import type { IssuePriority, IssueStatus, Project } from "@multica/core/types";
import type { AssigneeValue } from "@/components/issue/pickers/assignee-picker-body";
import { SubmitIssueButton } from "@/components/issue/submit-issue-button";
import { CreateFormAttributeRow } from "@/components/issue/create-form-attribute-row";
import { MentionSuggestionBar } from "@/components/issue/mention-suggestion-bar";
import { DescriptionField } from "@/components/issue/description-field";
import { MOBILE_PLACEHOLDER_COLOR } from "@/components/ui/input-tokens";
import { useCreateIssue } from "@/data/mutations/issues";
import {
  NEW_ISSUE_KEY,
  useFormDraftsStore,
} from "@/data/stores/form-drafts-store";
import { useNewIssueDraftStore } from "@/data/stores/new-issue-draft-store";
import { useRestoredFormDraft } from "@/data/stores/use-form-draft";
import { useMentionInput } from "@/lib/use-mention-input";
import { useT } from "@/lib/i18n";

type NewIssueFormDraft = {
  title: string;
  description: string;
  status: IssueStatus;
  priority: IssuePriority;
  assignee: AssigneeValue;
  dueDate: string | null;
  project: Project | null;
};

export default function NewIssueModal() {
  const { t } = useT("issues");
  const [title, setTitle] = useState("");
  const description = useMentionInput();
  // Attribute chips (status / priority / assignee / due date / project)
  // live in `useNewIssueDraftStore` so the new-issue-picker/* formSheet
  // routes can read and write the same values without a parent-child
  // React relationship. The unsent form (title, description, chips) is saved
  // as a draft, so closing the modal keeps it; only a successful create
  // clears it.
  const status = useNewIssueDraftStore((s) => s.status);
  const priority = useNewIssueDraftStore((s) => s.priority);
  const assignee = useNewIssueDraftStore((s) => s.assignee);
  const dueDate = useNewIssueDraftStore((s) => s.dueDate);
  const project = useNewIssueDraftStore((s) => s.project);
  const resetDraft = useNewIssueDraftStore((s) => s.reset);

  const restored = useRestoredFormDraft<NewIssueFormDraft>(
    NEW_ISSUE_KEY,
    useCallback(
      (draft) => {
        setTitle(draft.title);
        description.setText(draft.description);
        useNewIssueDraftStore.setState({
          status: draft.status,
          priority: draft.priority,
          assignee: draft.assignee,
          dueDate: draft.dueDate,
          project: draft.project,
        });
      },
      // eslint-disable-next-line react-hooks/exhaustive-deps -- setText is stable
      [description.setText],
    ),
  );
  const descriptionText = description.serialize();

  useEffect(() => {
    if (!restored) return;
    const { setDraft, clearDraft } = useFormDraftsStore.getState();
    const empty =
      !title &&
      !descriptionText &&
      status === "todo" &&
      priority === "none" &&
      !assignee &&
      !dueDate &&
      !project;
    if (empty) clearDraft(NEW_ISSUE_KEY);
    else
      setDraft(NEW_ISSUE_KEY, {
        title,
        description: descriptionText,
        status,
        priority,
        assignee,
        dueDate,
        project,
      } satisfies NewIssueFormDraft);
  }, [restored, title, descriptionText, status, priority, assignee, dueDate, project]);

  const createIssue = useCreateIssue();
  const isSubmitting = createIssue.isPending;

  const canSubmit = !isSubmitting && title.trim().length > 0;

  const onSubmit = useCallback(async () => {
    const trimmedTitle = title.trim();
    if (trimmedTitle.length === 0) return;
    const finalDescription = description.serialize().trim();
    try {
      await createIssue.mutateAsync({
        title: trimmedTitle,
        description: finalDescription || undefined,
        status,
        priority,
        ...(assignee
          ? { assignee_type: assignee.type, assignee_id: assignee.id }
          : {}),
        ...(dueDate ? { due_date: dueDate } : {}),
        ...(project ? { project_id: project.id } : {}),
      });
      resetDraft();
      useFormDraftsStore.getState().clearDraft(NEW_ISSUE_KEY);
      router.back();
    } catch (err) {
      Alert.alert(
        t("errors.create_failed"),
        err instanceof Error ? err.message : t("common:states.error"),
      );
    }
  }, [
    title,
    description,
    status,
    priority,
    assignee,
    dueDate,
    project,
    createIssue,
    resetDraft,
    t,
  ]);

  const headerRight = useCallback(
    () => (
      <SubmitIssueButton
        disabled={!canSubmit}
        loading={isSubmitting}
        onPress={onSubmit}
      />
    ),
    [canSubmit, isSubmitting, onSubmit],
  );

  return (
    <>
      <Stack.Screen options={{ headerRight }} />
      <KeyboardAvoidingView
        className="flex-1 bg-background"
        behavior={Platform.OS === "ios" ? "padding" : undefined}
      >
        <ScrollView
          className="flex-1"
          contentContainerClassName="px-4 pt-4 pb-6 gap-4"
          keyboardShouldPersistTaps="handled"
        >
          <TextInput
            value={title}
            onChangeText={setTitle}
            placeholder={t("new.title_placeholder")}
            placeholderTextColor={MOBILE_PLACEHOLDER_COLOR}
            className="text-2xl font-semibold text-foreground py-2"
            autoFocus
            returnKeyType="next"
            editable={!isSubmitting}
          />
          <DescriptionField
            description={description}
            disabled={isSubmitting}
          />
          <CreateFormAttributeRow />
        </ScrollView>

        {/* Mention suggestions float above the keyboard only when the user
            types `@`. Self-hides via `if (!visible) return null` so it
            doesn't take space at rest. */}
        <MentionSuggestionBar {...description.suggestionBar} />
      </KeyboardAvoidingView>
    </>
  );
}
