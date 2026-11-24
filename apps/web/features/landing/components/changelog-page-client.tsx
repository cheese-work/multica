"use client";

import {
  type MouseEvent,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { cn } from "@multica/ui/lib/utils";
import { Container, Eyebrow } from "../site/primitives";
import { PageHero, SitePage } from "../site/site-page";
import { useLocale } from "../i18n";
import type { Locale } from "../i18n/types";
import {
  type ChangelogRelease as Release,
  type ForkChangelog,
  forkCategoryLabels,
  forkStatusMessage,
  mergeChangelogReleases,
  releaseVersionLabel,
  unavailableFork,
} from "./fork-changelog";
import { ChangeList, DeploySections } from "./fork-deploys";

type ParsedDate = { year: number; month: number; day: number };

function parseDate(dateStr: string): ParsedDate {
  const parts = dateStr.split("-");
  return {
    year: Number(parts[0]),
    month: Number(parts[1]),
    day: Number(parts[2]),
  };
}

function utcDate(year: number, month: number, day: number) {
  const date = new Date(Date.UTC(year, month - 1, day));
  if (
    date.getUTCFullYear() !== year ||
    date.getUTCMonth() !== month - 1 ||
    date.getUTCDate() !== day
  ) {
    return null;
  }
  return date;
}

export function monthYearLabel(year: number, month: number, locale: Locale) {
  const date = utcDate(year, month, 1);
  if (!date) return "";
  return new Intl.DateTimeFormat(locale, {
    month: "long",
    timeZone: "UTC",
    year: "numeric",
  }).format(date);
}

export function fullDateLabel(dateStr: string, locale: Locale) {
  const { year, month, day } = parseDate(dateStr);
  const date = utcDate(year, month, day);
  if (!date) return dateStr;
  return new Intl.DateTimeFormat(locale, {
    day: "numeric",
    month: "long",
    timeZone: "UTC",
    year: "numeric",
  }).format(date);
}

type MonthGroup = {
  key: string;
  year: number;
  month: number;
  entries: Release[];
};

function groupByMonth(entries: readonly Release[]): MonthGroup[] {
  const groups: MonthGroup[] = [];
  for (const entry of entries) {
    const { year, month } = parseDate(entry.date);
    const key = `${year}-${month}`;
    const last = groups[groups.length - 1];
    if (last && last.key === key) {
      last.entries.push(entry);
    } else {
      groups.push({ key, year, month, entries: [entry] });
    }
  }
  return groups;
}

export function anchorId(version: string) {
  return `release-${version.replace(/\./g, "-")}`;
}

<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
export function ChangelogPageClient({ fork = unavailableFork, buildStamp = "dev" }: { fork?: ForkChangelog; buildStamp?: string }) {
||||||| 2ae2f08005
function ChangeList({ items }: { items: string[] }) {
  return (
    <ul className="mt-2 space-y-2">
      {items.map((change) => (
        <li
          key={change}
          className="flex items-start gap-2.5 text-body leading-[1.7] text-[#0a0d12]/60 sm:text-body-lg"
        >
          <span className="mt-2.5 h-1 w-1 shrink-0 rounded-full bg-[#0a0d12]/30" />
          {change}
        </li>
      ))}
    </ul>
  );
}

export function ChangelogPageClient() {
=======
function ChangeList({ items }: { items: string[] }) {
  return (
    <ul className="mt-3 flex flex-col gap-2.5">
      {items.map((change) => (
        <li key={change} className="flex items-start gap-3 text-site-body text-site-ink-secondary">
          <span aria-hidden className="mt-[0.6em] size-1 shrink-0 rounded-full bg-site-line-strong" />
          {change}
        </li>
      ))}
    </ul>
  );
}

function ChangeGroup({ label, items }: { label: string; items?: string[] }) {
  if (!items || items.length === 0) return null;
  return (
    <div>
      <Eyebrow>{label}</Eyebrow>
      <ChangeList items={items} />
    </div>
  );
}

export function ChangelogPageClient() {
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
  const { t, locale } = useLocale();
  const entries = useMemo(() => mergeChangelogReleases(t.changelog.entries, fork), [t.changelog.entries, fork]);
  const groups = useMemo(() => groupByMonth(entries), [entries]);

  const [activeVersion, setActiveVersion] = useState<string>(
    entries[0]?.version ?? ""
  );
  const navLockRef = useRef<number | null>(null);

  useEffect(() => {
    if (entries.length === 0) return;
    const visible = new Set<string>();

    const observer = new IntersectionObserver(
      (observed) => {
        observed.forEach((e) => {
          const v = (e.target as HTMLElement).dataset.version;
          if (!v) return;
          if (e.isIntersecting) visible.add(v);
          else visible.delete(v);
        });
        // Ignore observer updates while we're programmatically scrolling
        // to a clicked target — otherwise the active indicator flickers
        // through each passing entry.
        if (navLockRef.current !== null) return;

        const firstVisible = entries.find((r) => visible.has(r.version));
        if (firstVisible) {
          setActiveVersion(firstVisible.version);
          return;
        }
        const scrollY = window.scrollY;
        let best = entries[0]?.version ?? "";
        for (const r of entries) {
          const el = document.getElementById(anchorId(r.version));
          if (!el) continue;
          if (el.getBoundingClientRect().top + scrollY <= scrollY + 160) {
            best = r.version;
          }
        }
        setActiveVersion(best);
      },
      { rootMargin: "-20% 0px -70% 0px", threshold: 0 }
    );

    entries.forEach((r) => {
      const el = document.getElementById(anchorId(r.version));
      if (el) observer.observe(el);
    });
    return () => observer.disconnect();
  }, [entries]);

  const jumpTo =
    (version: string) => (e: MouseEvent<HTMLAnchorElement>) => {
      const el = document.getElementById(anchorId(version));
      if (!el) return;
      e.preventDefault();
      el.scrollIntoView({ behavior: "smooth", block: "start" });
      window.history.replaceState(null, "", `#${anchorId(version)}`);
      setActiveVersion(version);
      if (navLockRef.current !== null) {
        window.clearTimeout(navLockRef.current);
      }
      navLockRef.current = window.setTimeout(() => {
        navLockRef.current = null;
      }, 800);
    };

  return (
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
    <>
      <LandingHeader variant="light" />
      <main className="bg-white text-[#0a0d12]">
        <div className="mx-auto max-w-[1080px] px-4 py-16 sm:px-6 sm:py-20 lg:py-24">
          <div className="lg:grid lg:grid-cols-[200px_minmax(0,1fr)] lg:gap-16">
            <aside className="hidden lg:block">
              <nav
                aria-label={t.changelog.toc}
                className="sticky top-28 max-h-[calc(100vh-8rem)] overflow-y-auto pl-2 pb-8 pr-2"
              >
                <h3 className="text-micro font-semibold uppercase tracking-[0.14em] text-[#0a0d12]/50">
                  {t.changelog.toc}
                </h3>

                <div className="relative mt-5">
                  <span
                    aria-hidden="true"
                    className="pointer-events-none absolute left-[4px] top-7 bottom-2 w-px bg-[#0a0d12]/10"
                  />

                  <ol className="space-y-5">
                    {groups.map((group) => (
                      <li key={group.key}>
                        <p className="ml-6 text-micro font-semibold uppercase tracking-[0.12em] text-[#0a0d12]/45">
                          {monthYearLabel(group.year, group.month, locale)}
                        </p>

                        <ol className="mt-1.5">
                          {group.entries.map((release) => {
                            const isActive =
                              release.version === activeVersion;
                            const { day } = parseDate(release.date);
                            return (
                              <li key={release.version}>
                                <a
                                  href={`#${anchorId(release.version)}`}
                                  onClick={jumpTo(release.version)}
                                  aria-current={isActive ? "true" : undefined}
                                  className={[
                                    "group relative flex items-center gap-3 rounded-md py-1 pr-2 text-label transition-colors",
                                    isActive
                                      ? "text-[#0a0d12]"
                                      : "text-[#0a0d12]/55 hover:text-[#0a0d12]/80",
                                  ].join(" ")}
                                >
                                  <span
                                    aria-hidden="true"
                                    className={[
                                      "relative z-10 block size-[9px] shrink-0 rounded-full border transition-all duration-200",
                                      isActive
                                        ? "border-[#0a0d12] bg-[#0a0d12] ring-4 ring-[#0a0d12]/8"
                                        : "border-[#0a0d12]/25 bg-white group-hover:border-[#0a0d12]/60",
                                    ].join(" ")}
                                  />
                                  <span
                                    className={[
                                      "w-[1.25rem] shrink-0 text-right tabular-nums",
                                      isActive
                                        ? "font-semibold"
                                        : "font-medium",
                                    ].join(" ")}
                                  >
                                    {day}
                                  </span>
                                  <span className="tabular-nums text-micro text-[#0a0d12]/35">
                                    {releaseVersionLabel(release)}
                                  </span>
                                </a>
                              </li>
                            );
                          })}
                        </ol>
                      </li>
                    ))}
                  </ol>
                </div>
              </nav>
            </aside>

            <div className="mx-auto min-w-0 max-w-[720px] lg:mx-0">
              <h1 className="landing-serif text-[2.6rem] leading-[1.05] tracking-[-0.03em] sm:text-[3.4rem]">
                {t.changelog.title}
              </h1>
              <p className="mt-4 text-body-lg leading-7 text-[#0a0d12]/60 sm:text-title-sm">
                {t.changelog.subtitle}
              </p>
              <p className="mt-4 text-label text-[#0a0d12]/50">{buildStamp}</p>
              {forkStatusMessage(fork.status) && (
                <p className="mt-2 text-body text-[#0a0d12]/60">{forkStatusMessage(fork.status)}</p>
              )}

              <div className="mt-16 space-y-16">
                {entries.map((release) => {
                  const categoryLabels = release.version.startsWith("fork-") ? forkCategoryLabels : t.changelog.categories;
                  const hasCategorized =
                    release.features || release.improvements || release.fixes;
                  return (
                    <section
                      key={release.version}
                      id={anchorId(release.version)}
                      data-version={release.version}
                      className="relative scroll-mt-28"
                    >
                      <div className="flex items-baseline gap-3">
                        <span className="text-label font-semibold tabular-nums">
                          {releaseVersionLabel(release)}
                        </span>
                        <span className="text-label text-[#0a0d12]/40">
                          {fullDateLabel(release.date, locale)}
                        </span>
                      </div>
                      <h2 className="mt-2 text-title-lg font-semibold leading-snug sm:text-display-sm">
                        {release.title}
                      </h2>

                      {release.deploys ? (
                        <DeploySections release={release} />
                      ) : hasCategorized ? (
                        <div className="mt-4 space-y-5">
                          {release.features && release.features.length > 0 && (
                            <div>
                              <h3 className="text-label font-semibold uppercase tracking-wide text-[#0a0d12]/50">
                                {categoryLabels.features}
                              </h3>
                              <ChangeList items={release.features} itemKeys={release.changeKeys?.features} />
                            </div>
                          )}
                          {release.improvements &&
                            release.improvements.length > 0 && (
                              <div>
                                <h3 className="text-label font-semibold uppercase tracking-wide text-[#0a0d12]/50">
                                  {categoryLabels.improvements}
                                </h3>
                                <ChangeList items={release.improvements} itemKeys={release.changeKeys?.improvements} />
                              </div>
||||||| 2ae2f08005
    <>
      <LandingHeader variant="light" />
      <main className="bg-white text-[#0a0d12]">
        <div className="mx-auto max-w-[1080px] px-4 py-16 sm:px-6 sm:py-20 lg:py-24">
          <div className="lg:grid lg:grid-cols-[200px_minmax(0,1fr)] lg:gap-16">
            <aside className="hidden lg:block">
              <nav
                aria-label={t.changelog.toc}
                className="sticky top-28 max-h-[calc(100vh-8rem)] overflow-y-auto pl-2 pb-8 pr-2"
              >
                <h3 className="text-micro font-semibold uppercase tracking-[0.14em] text-[#0a0d12]/50">
                  {t.changelog.toc}
                </h3>

                <div className="relative mt-5">
                  <span
                    aria-hidden="true"
                    className="pointer-events-none absolute left-[4px] top-7 bottom-2 w-px bg-[#0a0d12]/10"
                  />

                  <ol className="space-y-5">
                    {groups.map((group) => (
                      <li key={group.key}>
                        <p className="ml-6 text-micro font-semibold uppercase tracking-[0.12em] text-[#0a0d12]/45">
                          {monthYearLabel(group.year, group.month, locale)}
                        </p>

                        <ol className="mt-1.5">
                          {group.entries.map((release) => {
                            const isActive =
                              release.version === activeVersion;
                            const { day } = parseDate(release.date);
                            return (
                              <li key={release.version}>
                                <a
                                  href={`#${anchorId(release.version)}`}
                                  onClick={jumpTo(release.version)}
                                  aria-current={isActive ? "true" : undefined}
                                  className={[
                                    "group relative flex items-center gap-3 rounded-md py-1 pr-2 text-label transition-colors",
                                    isActive
                                      ? "text-[#0a0d12]"
                                      : "text-[#0a0d12]/55 hover:text-[#0a0d12]/80",
                                  ].join(" ")}
                                >
                                  <span
                                    aria-hidden="true"
                                    className={[
                                      "relative z-10 block size-[9px] shrink-0 rounded-full border transition-all duration-200",
                                      isActive
                                        ? "border-[#0a0d12] bg-[#0a0d12] ring-4 ring-[#0a0d12]/8"
                                        : "border-[#0a0d12]/25 bg-white group-hover:border-[#0a0d12]/60",
                                    ].join(" ")}
                                  />
                                  <span
                                    className={[
                                      "w-[1.25rem] shrink-0 text-right tabular-nums",
                                      isActive
                                        ? "font-semibold"
                                        : "font-medium",
                                    ].join(" ")}
                                  >
                                    {day}
                                  </span>
                                  <span className="tabular-nums text-micro text-[#0a0d12]/35">
                                    v{release.version}
                                  </span>
                                </a>
                              </li>
                            );
                          })}
                        </ol>
                      </li>
                    ))}
                  </ol>
                </div>
              </nav>
            </aside>

            <div className="mx-auto min-w-0 max-w-[720px] lg:mx-0">
              <h1 className="landing-serif text-[2.6rem] leading-[1.05] tracking-[-0.03em] sm:text-[3.4rem]">
                {t.changelog.title}
              </h1>
              <p className="mt-4 text-body-lg leading-7 text-[#0a0d12]/60 sm:text-title-sm">
                {t.changelog.subtitle}
              </p>

              <div className="mt-16 space-y-16">
                {entries.map((release) => {
                  const hasCategorized =
                    release.features || release.improvements || release.fixes;
                  return (
                    <section
                      key={release.version}
                      id={anchorId(release.version)}
                      data-version={release.version}
                      className="relative scroll-mt-28"
                    >
                      <div className="flex items-baseline gap-3">
                        <span className="text-label font-semibold tabular-nums">
                          v{release.version}
                        </span>
                        <span className="text-label text-[#0a0d12]/40">
                          {fullDateLabel(release.date, locale)}
                        </span>
                      </div>
                      <h2 className="mt-2 text-title-lg font-semibold leading-snug sm:text-display-sm">
                        {release.title}
                      </h2>

                      {hasCategorized ? (
                        <div className="mt-4 space-y-5">
                          {release.features && release.features.length > 0 && (
                            <div>
                              <h3 className="text-label font-semibold uppercase tracking-wide text-[#0a0d12]/50">
                                {categoryLabels.features}
                              </h3>
                              <ChangeList items={release.features} />
                            </div>
                          )}
                          {release.improvements &&
                            release.improvements.length > 0 && (
                              <div>
                                <h3 className="text-label font-semibold uppercase tracking-wide text-[#0a0d12]/50">
                                  {categoryLabels.improvements}
                                </h3>
                                <ChangeList items={release.improvements} />
                              </div>
=======
    <SitePage>
      <PageHero title={t.changelog.title} lead={t.changelog.subtitle} />
      <Container className="pb-site-section lg:grid lg:grid-cols-[10rem_minmax(0,1fr)] lg:gap-16 xl:gap-24">
        <aside className="hidden lg:block">
          <nav aria-label={t.changelog.toc} className="sticky top-28 max-h-[calc(100vh-8rem)] overflow-y-auto pb-8 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
            <Eyebrow>{t.changelog.toc}</Eyebrow>
            <ol className="mt-5 flex flex-col gap-5 border-l border-site-line">
              {groups.map((group) => (
                <li key={group.key}>
                  <p className="pl-5 text-site-small text-site-muted">{monthYearLabel(group.year, group.month, locale)}</p>
                  <ol className="mt-1.5">
                    {group.entries.map((release) => {
                      const isActive = release.version === activeVersion;
                      return (
                        <li key={release.version}>
                          <a
                            href={`#${anchorId(release.version)}`}
                            onClick={jumpTo(release.version)}
                            aria-current={isActive ? "true" : undefined}
                            className={cn(
                              "relative -ml-px flex items-baseline gap-3 border-l py-1 pl-5 text-site-small tabular-nums transition-colors",
                              isActive
                                ? "border-site-ink text-site-ink"
                                : "border-transparent text-site-muted hover:text-site-ink",
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
                            )}
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
                          {release.fixes && release.fixes.length > 0 && (
                            <div>
                              <h3 className="text-label font-semibold uppercase tracking-wide text-[#0a0d12]/50">
                                {categoryLabels.fixes}
                              </h3>
                              <ChangeList items={release.fixes} itemKeys={release.changeKeys?.fixes} />
                            </div>
                          )}
                        </div>
                      ) : (
                        <ChangeList items={release.changes} />
                      )}
                    </section>
                  );
                })}
              </div>
            </div>
          </div>
||||||| 2ae2f08005
                          {release.fixes && release.fixes.length > 0 && (
                            <div>
                              <h3 className="text-label font-semibold uppercase tracking-wide text-[#0a0d12]/50">
                                {categoryLabels.fixes}
                              </h3>
                              <ChangeList items={release.fixes} />
                            </div>
                          )}
                        </div>
                      ) : (
                        <ChangeList items={release.changes} />
                      )}
                    </section>
                  );
                })}
              </div>
            </div>
          </div>
=======
                          >
                            <span className="w-5 text-right">{parseDate(release.date).day}</span>
                            <span>v{release.version}</span>
                          </a>
                        </li>
                      );
                    })}
                  </ol>
                </li>
              ))}
            </ol>
          </nav>
        </aside>

        <div className="min-w-0 border-t border-site-line">
          {entries.map((release) => {
            const hasCategorized = release.features || release.improvements || release.fixes;
            return (
              <section
                key={release.version}
                id={anchorId(release.version)}
                data-version={release.version}
                className="scroll-mt-28 border-b border-site-line py-site-block"
              >
                <p className="flex items-baseline gap-3 text-site-small tabular-nums">
                  <span className="text-site-ink">v{release.version}</span>
                  <span className="text-site-muted">{fullDateLabel(release.date, locale)}</span>
                </p>
                <h2 className="mt-3 max-w-[46rem] font-site-display text-site-title text-site-ink text-balance">
                  {release.title}
                </h2>
                {hasCategorized ? (
                  <div className="mt-6 flex max-w-[46rem] flex-col gap-6">
                    <ChangeGroup label={categoryLabels.features} items={release.features} />
                    <ChangeGroup label={categoryLabels.improvements} items={release.improvements} />
                    <ChangeGroup label={categoryLabels.fixes} items={release.fixes} />
                  </div>
                ) : (
                  <div className="mt-6 max-w-[46rem]">
                    <ChangeList items={release.changes} />
                  </div>
                )}
              </section>
            );
          })}
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
        </div>
      </Container>
    </SitePage>
  );
}
