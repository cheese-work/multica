import { ISSUE_CHIP_BASE_CLASS } from "./issue-chip-shell";

/**
 * Box shown for a bare issue key while its lookup is in flight. It wears the
 * resolved chip's shell, so the line keeps the chip's height when the lookup
 * lands; only the title segment is added to the width.
 *
 * Deliberately not a link: the key is unverified until the lookup settles, and
 * a miss (404) turns back into the plain text the author typed.
 */
export function IssueChipLoading({ identifier }: { identifier: string }) {
  return (
    <span className="inline align-middle">
      <span className="issue-mention align-middle">
        <span
          data-issue-chip-loading=""
          aria-busy="true"
          className={ISSUE_CHIP_BASE_CLASS}
        >
          <span className="min-w-0 truncate font-medium text-muted-foreground">
            {identifier}
          </span>
        </span>
      </span>
    </span>
  );
}
