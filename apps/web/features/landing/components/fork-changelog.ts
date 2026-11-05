export type ForkEntry = {
  id: string;
  number: number;
  mergedAt: string;
  date: string;
  section: string;
  text: string;
  group?: "week"; // backfilled entries (CHE-2196) group by ISO week instead of by day
  deploy?: string; // sha of the first C00 deploy containing the entry, or "live" (CHE-4015)
};

type ForkDeploy = { sha: string; version: string; deployedAt: string; hiddenCount: number; live?: boolean };
export type DeployGroup = ForkDeploy & {
  rank: number; // 0 = Live, 1 = Previous, 2 = the one before, ... (across all days)
  live: boolean;
  features: string[];
  improvements: string[];
  fixes: string[];
  changeKeys: Record<Category, string[]>;
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
  deploys?: DeployGroup[];
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

const ictDay = (iso: string) => new Date(Date.parse(iso) + 7 * 60 * 60 * 1000).toISOString().slice(0, 10);

// Valid deploys: first is the one Live build, shas are unique, every dated and counted.
function validDeploys(value: unknown, entries: ForkEntry[]): ForkDeploy[] | null {
  if (!Array.isArray(value) || !value.length || !entries.length) return null;
  const shas = new Set<string>();
  for (const [index, deploy] of value.entries()) {
    if (!deploy || typeof deploy.sha !== "string" || !deploy.sha || shas.has(deploy.sha) ||
      typeof deploy.version !== "string" || !deploy.version.trim() ||
      typeof deploy.deployedAt !== "string" || !Number.isFinite(Date.parse(deploy.deployedAt)) ||
      !Number.isSafeInteger(deploy.hiddenCount) || deploy.hiddenCount < 0 || Boolean(deploy.live) !== (index === 0)) return null;
    shas.add(deploy.sha);
  }
  const known = new Set([...shas, "live"]);
  return entries.every((entry) => entry.group === "week" || known.has(entry.deploy ?? "")) ? value : null;
}

// One release per ICT day of a deploy, holding that day's deploys newest first.
function deployReleases(entries: ForkEntry[], deploys: ForkDeploy[]): ChangelogRelease[] {
  const groups = deploys.map((deploy, rank): DeployGroup => ({
    ...deploy, live: rank === 0, rank, features: [], improvements: [], fixes: [],
    changeKeys: { features: [], improvements: [], fixes: [] },
  }));
  for (const entry of [...entries].sort(entryOrder)) {
    if (entry.group === "week") continue;
    const group = groups.find((candidate) => candidate.sha === entry.deploy || (entry.deploy === "live" && candidate.live))!;
    const category = sectionCategory[entry.section]!;
    group[category].push(entry.text);
    group.changeKeys[category].push(entry.id);
  }
  const days = new Map<string, ChangelogRelease>();
  for (const group of groups) {
    const date = ictDay(group.deployedAt);
    const release = days.get(date) ?? { version: `fork-${date}`, date, title: "Fork", changes: [], mergedAt: group.deployedAt, number: 0, deploys: [] };
    release.deploys!.push(group);
    days.set(date, release);
  }
  return [...days.values()];
}

export function forkReleases(entries: ForkEntry[], deploys?: ForkDeploy[]): ChangelogRelease[] {
  if (deploys) {
    const rest = entries.filter((entry) => entry.group === "week");
    return [...deployReleases(entries, deploys), ...forkReleases(rest)].sort((a, b) => b.date.localeCompare(a.date));
  }
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
        entry.date !== ictDay(entry.mergedAt) ||
        !Object.hasOwn(sectionCategory, entry.section) || typeof entry.text !== "string" || !entry.text.trim()) {
        return unavailableFork;
      }
      ids.add(entry.id);
    }
    return { status: parsed.entries.length ? "populated" : "empty", releases: forkReleases(parsed.entries, validDeploys(parsed.deploys, parsed.entries) ?? undefined) };
  } catch {
    return unavailableFork;
  }
}

// The build stamp shows the same name as the Live divider.
export const liveVersion = (fork: ForkChangelog) => fork.releases.flatMap((release) => release.deploys ?? []).find((deploy) => deploy.live)?.version;

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
