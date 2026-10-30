import { useT } from "../../i18n";
import { RichContent } from "../../rich-content";

/**
 * The note typed beside a chosen option. The server stores a card answer as
 * `<option label>` or `<option label>\n\n<note>`; `optionLabel` comes from the
 * structured answer, so a plain text answer is never split.
 */
export function noteAfterOption(content: string, optionLabel: string): string | undefined {
  if (!content.startsWith(optionLabel)) return undefined;
  const rest = content.slice(optionLabel.length);
  if (rest !== "" && !rest.startsWith("\n")) return undefined;
  return rest.trim();
}

/** A card answer in the DM: the choice as one chip, the note under it. */
export function DecisionAnswerContent({ content, optionLabel }: { content: string; optionLabel: string }) {
  const { t } = useT("chat");
  const note = noteAfterOption(content, optionLabel);
  if (note === undefined) return null;
  return (
    <div className="space-y-1.5">
      <span data-testid="decision-answer-chip" className="inline-block rounded-full bg-primary/10 px-2 py-0.5 text-label font-medium">
        {t(($) => $.decision.answer_chose, { option: optionLabel })}
      </span>
      {note !== "" && (
        <div data-testid="decision-answer-note">
          <RichContent content={note} density="compact" phase="settled" />
        </div>
      )}
    </div>
  );
}
