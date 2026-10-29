export type ForkEntry = {
  id: string;
  number: number;
  mergedAt: string;
  date: string;
  section: string;
  text: string;
  group?: "week"; // backfilled entries (CHE-2196) group by ISO week instead of by day
};

type Category = "features" | "improvements" | "fixes";
export type ChangelogRelease = {
  version: string;
  date: string;
  title: string;
  changes: string[];
  features?: string[];
  improvements?: string[];
  fixes?: string[];
  changeKeys?: Partial<Record<Category, string[]>>;
  mergedAt?: string;
  number?: number;
};

export type ForkChangelog = {
  status: "populated" | "empty" | "unavailable";
  releases: ChangelogRelease[];
};

const sectionCategory: Record<string, Category> = {
  "New Features": "features",
  Improvements: "improvements",
  "Bug Fixes": "fixes",
};
export const forkCategoryLabels = {
  features: "New Features",
  improvements: "Improvements",
  fixes: "Bug Fixes",
};
export const unavailableFork: ForkChangelog = { status: "unavailable", releases: [] };
export const forkStatusMessage = (status: ForkChangelog["status"]) =>
  status === "unavailable" ? "Fork entries unavailable for this build" : status === "empty" ? "No fork entries yet" : null;
export const releaseVersionLabel = (release: ChangelogRelease) =>
  release.version.startsWith("fork-") ? "Fork" : `v${release.version}`;

function entryOrder(left: ForkEntry, right: ForkEntry) {
  return Date.parse(right.mergedAt) - Date.parse(left.mergedAt) || right.number - left.number;
}

// ISO week of a YYYY-MM-DD date: the Monday it starts on and the "YYYY-Www" label.
function isoWeek(date: string) {
  const day = new Date(`${date}T00:00:00Z`);
  const monday = new Date(day.getTime() - ((day.getUTCDay() + 6) % 7) * 86_400_000);
  const thursday = new Date(monday.getTime() + 3 * 86_400_000);
  const jan1 = Date.UTC(thursday.getUTCFullYear(), 0, 1);
  const week = Math.floor((thursday.getTime() - jan1) / (7 * 86_400_000)) + 1;
  return { monday: monday.toISOString().slice(0, 10), label: `${thursday.getUTCFullYear()}-W${String(week).padStart(2, "0")}` };
}

export function forkReleases(entries: ForkEntry[]): ChangelogRelease[] {
  const releases = new Map<string, ChangelogRelease>();
  for (const entry of [...entries].sort(entryOrder)) {
    const week = entry.group === "week" ? isoWeek(entry.date) : null;
    const version = `fork-${week?.label ?? entry.date}`;
    let release = releases.get(version);
    if (!release) {
      release = { version, date: week?.monday ?? entry.date, title: "Fork", changes: [],
        features: [], improvements: [], fixes: [], changeKeys: { features: [], improvements: [], fixes: [] },
        mergedAt: entry.mergedAt, number: entry.number };
      releases.set(version, release);
    }
    const category = sectionCategory[entry.section];
    if (!category) throw new Error("Unknown fork changelog section");
    release[category]?.push(entry.text);
    release.changeKeys?.[category]?.push(entry.id);
  }
  return [...releases.values()];
}

export function parseForkChangelog(contents: string): ForkChangelog {
  try {
    const parsed = JSON.parse(contents);
    if (!Array.isArray(parsed.entries)) return unavailableFork;
    const ids = new Set<string>();
    for (const entry of parsed.entries) {
      if (!entry || !Number.isSafeInteger(entry.number) || entry.number <= 0 ||
        entry.id !== `fork-pr-${entry.number}` || ids.has(entry.id) ||
        typeof entry.mergedAt !== "string" || !Number.isFinite(Date.parse(entry.mergedAt)) ||
        entry.date !== new Date(Date.parse(entry.mergedAt) + 7 * 60 * 60 * 1000).toISOString().slice(0, 10) ||
        !Object.hasOwn(sectionCategory, entry.section) || typeof entry.text !== "string" || !entry.text.trim()) {
        return unavailableFork;
      }
      ids.add(entry.id);
    }
    return { status: parsed.entries.length ? "populated" : "empty", releases: forkReleases(parsed.entries) };
  } catch {
    return unavailableFork;
  }
}

export function mergeChangelogReleases(upstream: readonly ChangelogRelease[], fork: ForkChangelog) {
  return [...upstream, ...fork.releases].sort((left, right) =>
    right.date.localeCompare(left.date) ||
    Date.parse(right.mergedAt ?? `${right.date}T00:00:00+07:00`) - Date.parse(left.mergedAt ?? `${left.date}T00:00:00+07:00`) ||
    (right.number ?? 0) - (left.number ?? 0));
}

export function forkBuildStamp(version?: string, buildDate?: string) {
  if (!version || version === "dev") return "dev";
  const sha = version.replace(/^sha-/, "").slice(0, 7);
  const date = Date.parse(buildDate ?? "");
  return Number.isFinite(date) ? `${sha} · ${new Date(date + 7 * 60 * 60 * 1000).toISOString().slice(0, 16).replace("T", " ")} ICT` : sha;
}
