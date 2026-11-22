import { useState, type ReactNode } from "react";
import { Pressable, View } from "react-native";
import { Text } from "@/components/ui/text";
import { detailsBodyFor } from "./details-disclosure";
import type { DetailsSegment, MarkdownSegment } from "./split-markdown";

/**
 * Collapsed-by-default disclosure for a `<details>` block. The header is a
 * button (role, expanded state, summary as label) with a 44 pt hit target.
 */
export function DetailsDisclosure({
  segment,
  renderBody,
}: {
  segment: DetailsSegment;
  renderBody: (body: MarkdownSegment[]) => ReactNode;
}) {
  const [expanded, setExpanded] = useState(false);
  const body = detailsBodyFor(segment, expanded);
  return (
    <View>
      <Pressable
        onPress={() => setExpanded((v) => !v)}
        accessibilityRole="button"
        accessibilityLabel={segment.summary}
        accessibilityState={{ expanded }}
        style={{ minHeight: 44, flexDirection: "row", alignItems: "center", gap: 6 }}
      >
        <Text className="text-muted-foreground">{expanded ? "▾" : "▸"}</Text>
        <Text className="font-medium">{segment.summary}</Text>
      </Pressable>
      {body ? <View className="gap-3">{renderBody(body)}</View> : null}
    </View>
  );
}
