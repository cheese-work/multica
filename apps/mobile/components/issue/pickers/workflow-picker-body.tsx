/**
 * Pure picker body for an issue's workflow override (CHE-2822): "Follow
 * project" (null) or one of the workspace's workflows.
 */
import { Pressable, ScrollView, View } from "react-native";
import { Ionicons } from "@expo/vector-icons";
import { useColorScheme } from "nativewind";
import type { IssueWorkflow } from "@multica/core/types";
import { Text } from "@/components/ui/text";
import { THEME } from "@/lib/theme";
import { useT } from "@/lib/i18n";

interface Props {
  /** The override's id; null follows the project's workflow. */
  value: string | null;
  workflows: readonly IssueWorkflow[];
  onChange: (next: string | null) => void;
}

export function WorkflowPickerBody({ value, workflows, onChange }: Props) {
  const { colorScheme } = useColorScheme();
  const checkColor = colorScheme === "dark" ? THEME.dark.primary : THEME.light.primary;
  const { t } = useT("issues");
  const rows = [
    { id: null, name: t("workflow.follow_project") },
    ...workflows.map((w) => ({ id: w.id, name: w.name })),
  ];

  return (
    <ScrollView showsVerticalScrollIndicator={false}>
      <View className="px-4 pt-3 pb-2">
        <Text className="text-lg font-semibold text-foreground">{t("workflow.title")}</Text>
      </View>
      <View className="px-2">
        {rows.map((row) => (
          <Pressable
            key={row.id ?? "follow-project"}
            onPress={() => onChange(row.id)}
            className="flex-row items-center gap-3 rounded-lg px-3 py-3 active:bg-secondary"
          >
            <Text className="flex-1 text-base text-foreground">{row.name}</Text>
            {row.id === value ? <Ionicons name="checkmark" size={20} color={checkColor} /> : null}
          </Pressable>
        ))}
      </View>
    </ScrollView>
  );
}
