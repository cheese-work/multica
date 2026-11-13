import { type ChangelogRelease, type DeployGroup, forkCategoryLabels } from "./fork-changelog";

export function ChangeList({ items, itemKeys }: { items: string[]; itemKeys?: string[] }) {
  return (
    <ul className="mt-3 flex flex-col gap-2.5">
      {items.map((change, index) => (
        <li
          key={itemKeys?.[index] ?? `${index}:${change}`}
          className="flex items-start gap-3 text-site-body text-site-ink-secondary"
        >
          <span aria-hidden className="mt-[0.6em] size-1 shrink-0 rounded-full bg-site-line-strong" />
          {change}
        </li>
      ))}
    </ul>
  );
}

// Heat by rank: Live and Previous also carry a chip, so colour is never the only signal.
// Chip and divider colours are >= 4.5:1 on their backgrounds (AA); this surface is light-only.
const heat = (rank: number) => (rank === 0 ? "live" : rank === 1 ? "previous" : rank === 2 ? "earlier" : "older");
const heatStyle = {
  live: "border-l-4 border-emerald-700 bg-emerald-50 py-3 pl-4 pr-3",
  previous: "border-l-4 border-sky-700 bg-sky-50 py-3 pl-4 pr-3",
  earlier: "border-l-4 border-slate-300 bg-slate-50 py-3 pl-4 pr-3",
  older: "",
} as const;
const chipStyle = { live: "bg-emerald-700 text-white", previous: "bg-sky-800 text-white" } as const;

const ictTime = (iso: string) => `${new Date(Date.parse(iso) + 7 * 60 * 60 * 1000).toISOString().slice(0, 16).replace("T", " ")} ICT`;

function emptyNote({ hiddenCount }: DeployGroup) {
  return hiddenCount === 0 ? "No user-facing changes" : `${hiddenCount} ${hiddenCount === 1 ? "change" : "changes"} without notes`;
}

export function DeploySections({ release }: { release: ChangelogRelease }) {
  return (
    <div className="mt-4 space-y-6">
      {release.deploys?.map((deploy) => {
        const level = heat(deploy.rank);
        const categories = (["features", "improvements", "fixes"] as const).filter((category) => deploy[category].length > 0);
        return (
          <div key={deploy.sha} data-heat={level} data-deploy={deploy.sha} className={heatStyle[level]}>
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-label font-semibold tabular-nums text-[#0a0d12]/75">
              <span aria-hidden="true">──</span>
              <span>{deploy.live ? `${deploy.version} · Live` : `${deploy.version} · ${ictTime(deploy.deployedAt)}`}</span>
              {(level === "live" || level === "previous") && (
                <span className={`rounded-sm px-1.5 py-0.5 text-xs font-semibold uppercase tracking-wide ${chipStyle[level]}`}>{level === "live" ? "Live" : "Previous"}</span>
              )}
              <span aria-hidden="true">──</span>
            </div>
            {categories.length ? (
              <div className="mt-3 space-y-5">
                {categories.map((category) => (
                  <div key={category}>
                    <h3 className="text-label font-semibold uppercase tracking-wide text-[#0a0d12]/60">{forkCategoryLabels[category]}</h3>
                    <ChangeList items={deploy[category]} itemKeys={deploy.changeKeys[category]} />
                  </div>
                ))}
                {deploy.hiddenCount > 0 && <p className="text-label text-[#0a0d12]/60">{emptyNote(deploy)}</p>}
              </div>
            ) : (
              <p className="mt-3 text-body text-[#0a0d12]/60">{emptyNote(deploy)}</p>
            )}
          </div>
        );
      })}
    </div>
  );
}
