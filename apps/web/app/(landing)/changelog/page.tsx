import type { Metadata } from "next";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { pageMetadata } from "@/features/landing/utils/page-metadata";
import { ChangelogPageClient } from "@/features/landing/components/changelog-page-client";
import { forkBuildStamp, liveVersion, parseForkChangelog, unavailableFork } from "@/features/landing/components/fork-changelog";

const fork = (() => {
  try {
    return parseForkChangelog(readFileSync(join(process.cwd(), ".fork-changelog/entries.json"), "utf8"));
  } catch {
    return unavailableFork;
  }
})();

export function generateMetadata(): Promise<Metadata> {
  return pageMetadata("changelog", "/changelog");
}

export default function ChangelogPage() {
  return (
    <ChangelogPageClient
      fork={fork}
      buildStamp={liveVersion(fork) ?? forkBuildStamp(
        process.env.NEXT_PUBLIC_APP_COMMIT || process.env.NEXT_PUBLIC_APP_VERSION,
        process.env.NEXT_PUBLIC_BUILD_DATE,
      )}
    />
  );
}
