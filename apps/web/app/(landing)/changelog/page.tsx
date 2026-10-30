import type { Metadata } from "next";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { ChangelogPageClient } from "@/features/landing/components/changelog-page-client";
import { forkBuildStamp, parseForkChangelog, unavailableFork } from "@/features/landing/components/fork-changelog";

const fork = (() => {
  try {
    return parseForkChangelog(readFileSync(join(process.cwd(), ".fork-changelog/entries.json"), "utf8"));
  } catch {
    return unavailableFork;
  }
})();

export const metadata: Metadata = {
  title: "Changelog",
  description:
    "See what's new in Multica — latest features, improvements, and fixes.",
  openGraph: {
    title: "Changelog | Multica",
    description: "Latest updates and releases from Multica.",
    url: "/changelog",
  },
  alternates: {
    canonical: "/changelog",
  },
};

export default function ChangelogPage() {
  return (
    <ChangelogPageClient
      fork={fork}
      buildStamp={forkBuildStamp(
        process.env.NEXT_PUBLIC_APP_COMMIT || process.env.NEXT_PUBLIC_APP_VERSION,
        process.env.NEXT_PUBLIC_BUILD_DATE,
      )}
    />
  );
}
