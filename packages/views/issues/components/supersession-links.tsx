import { createContext, useContext, useMemo, type ReactNode } from "react";
import { ArrowRight } from "@multica/ui/icons";
import { supersessionRevisions } from "@multica/core/issues/supersession";
import type { TimelineEntry } from "@multica/core/types";
import { useT } from "../../i18n";

const NO_REVISIONS: ReadonlyMap<string, number> = new Map();
const RevisionsContext = createContext(NO_REVISIONS);

/**
 * Supplies the "rev-N" number of every comment in a supersession chain. The
 * server stores links only; the numbers derive from the loaded timeline, so one
 * provider above the thread cards serves every link without prop drilling.
 */
export function SupersessionRevisionsProvider({
  entries,
  children,
}: {
  entries: readonly TimelineEntry[];
  children: ReactNode;
}) {
  const revisions = useMemo(() => supersessionRevisions(entries), [entries]);
  return <RevisionsContext.Provider value={revisions}>{children}</RevisionsContext.Provider>;
}

const linkClass =
  "inline-flex items-center gap-1 text-label text-muted-foreground transition-colors hover:text-foreground";

/** "Superseded by rev-N →" for a resolved thread; null until the new revision is loaded. */
export function SupersededByLink({ entry }: { entry: TimelineEntry }) {
  const { t } = useT("issues");
  const revisions = useContext(RevisionsContext);
  const byId = entry.superseded_by_id;
  const revision = byId ? revisions.get(byId) : undefined;
  if (!byId || revision === undefined) return null;
  return (
    <a href={`#comment-${byId}`} className={linkClass}>
      {t(($) => $.comment.supersession.superseded_by, { revision })}
      <ArrowRight className="h-3 w-3" />
    </a>
  );
}

/** "Supersedes rev-N" for the new revision, one link per loaded older revision. */
export function SupersedesLine({ entry }: { entry: TimelineEntry }) {
  const { t } = useT("issues");
  const revisions = useContext(RevisionsContext);
  const links = (entry.supersedes ?? []).flatMap((link) => {
    const revision = revisions.get(link.comment_id);
    return revision === undefined ? [] : [{ id: link.comment_id, revision }];
  });
  if (links.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-x-3">
      {links.map(({ id, revision }) => (
        <a key={id} href={`#comment-${id}`} className={linkClass}>
          {t(($) => $.comment.supersession.supersedes, { revision })}
        </a>
      ))}
    </div>
  );
}
