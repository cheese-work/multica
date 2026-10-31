interface SupersessionLinked {
  id: string;
  superseded_by_id?: string | null;
  supersedes?: readonly { comment_id: string }[];
}

/**
 * Revision number of each comment in a supersession chain: a comment that
 * supersedes nothing is rev-1, otherwise one past the highest revision it
 * supersedes. The server stores links only, so the "rev-N" labels are derived
 * here from the loaded comments. A link to a comment that is not loaded
 * counts as no link.
 */
export function supersessionRevisions(entries: readonly SupersessionLinked[]): Map<string, number> {
  const byId = new Map(entries.map((entry) => [entry.id, entry]));
  const revisions = new Map<string, number>();
  const visiting = new Set<string>();

  const revisionOf = (id: string): number => {
    const known = revisions.get(id);
    if (known !== undefined) return known;
    // A cyclic link cannot come from the server; stop rather than recurse forever.
    if (visiting.has(id)) return 0;
    visiting.add(id);
    let previous = 0;
    for (const link of byId.get(id)?.supersedes ?? []) {
      if (byId.has(link.comment_id)) previous = Math.max(previous, revisionOf(link.comment_id));
    }
    visiting.delete(id);
    revisions.set(id, previous + 1);
    return previous + 1;
  };

  for (const entry of entries) revisionOf(entry.id);
  return revisions;
}
