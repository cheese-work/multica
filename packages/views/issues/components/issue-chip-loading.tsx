import { ISSUE_CHIP_BASE_CLASS } from "./issue-chip-shell";

/**
 * Box shown for a bare issue key while its lookup is in flight. It wears the
 * resolved chip's shell, including an empty slot where the status icon goes
 * (the icon is the chip's first flex item and sets its baseline), so the line
 * keeps the chip's height and vertical position when the lookup lands; only
 * the title segment is added to the width.
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
          <span aria-hidden className="h-3.5 w-3.5 shrink-0" />
          <span className="font-medium text-muted-foreground shrink-0">
            {identifier}
          </span>
        </span>
      </span>
    </span>
  );
}
