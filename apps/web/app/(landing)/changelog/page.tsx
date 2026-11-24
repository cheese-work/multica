import type { Metadata } from "next";
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
import { readFileSync } from "node:fs";
import { join } from "node:path";
||||||| 2ae2f08005
=======
import { pageMetadata } from "@/features/landing/utils/page-metadata";
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
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
