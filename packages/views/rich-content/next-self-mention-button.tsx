import { ArrowDown } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../i18n";

/** Floating jump to the viewer's next mention; renders nothing when none is left below. */
export function NextSelfMentionButton({
  count,
  onJump,
  className,
}: {
  count: number;
  onJump: () => void;
  className?: string;
}) {
  const { t } = useT("common");
  if (count <= 0) return null;
  return (
    <Button
      type="button"
      size="sm"
      variant="outline"
      onClick={onJump}
      className={cn("rounded-full shadow-md", className)}
    >
      <ArrowDown aria-hidden />
      {t(($) => $.next_self_mention.label)}
      {count > 1 && <span className="tabular-nums text-muted-foreground">{count}</span>}
    </Button>
  );
}
