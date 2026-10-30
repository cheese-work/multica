import { useT } from "../../i18n";

/** The agent-maintained one-line summary kept in `metadata.status_summary`. */
export function statusSummaryOf(metadata: Record<string, unknown> | undefined): string | null {
  const value = metadata?.status_summary;
  if (typeof value !== "string") return null;
  return value.trim() || null;
}

export function StatusSummary({ summary }: { summary: string | null }) {
  const { t } = useT("issues");
  if (!summary) return null;
  return (
    <div className="mt-5 rounded-lg border bg-muted/40 px-3 py-2">
      <div className="text-xs font-medium text-muted-foreground">
        {t(($) => $.detail.status_summary_label)}
      </div>
      <p className="mt-0.5 whitespace-pre-wrap break-words text-sm">{summary}</p>
    </div>
  );
}
