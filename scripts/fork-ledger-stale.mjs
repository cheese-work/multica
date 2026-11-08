// CHE-3960: weekly stale check for docs/fork/. The ledger pins upstream main (UPSTREAM-FIXES.md Method); it is stale when upstream
// moved, when the pin is older than 7 days, or when there is no pin. Security-class rows are listed by id only, so the weekly
// reader re-checks whether upstream fixed them. Usage: node scripts/fork-ledger-stale.mjs   (exit 1 = stale, needs network)
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const PIN = /checked against upstream `main` `([0-9a-f]{40})` \((\d{4}-\d\d-\d\d \d\d:\d\d) ICT\)/;
const MAX_DAYS = 7;

export function check({ ledger, upstreamHead, now }) {
  const reasons = [];
  const m = PIN.exec(ledger);
  if (!m) reasons.push("no upstream pin found in the ledger");
  else {
    if (m[1] !== upstreamHead) reasons.push(`upstream main moved: pin ${m[1].slice(0, 10)}, now ${upstreamHead.slice(0, 10)}`);
    const days = (now - new Date(`${m[2].replace(" ", "T")}:00+07:00`)) / 864e5;
    if (days > MAX_DAYS) reasons.push(`pin is older than ${MAX_DAYS} days (${days.toFixed(1)} days)`);
  }
  const withheld = [...ledger.matchAll(/^\|[^|]*\|\s*(FX\d+)\s*\|.*details withheld/gm)].map((r) => r[1]);
  return { stale: reasons.length > 0, reasons, withheld };
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  const ledger = readFileSync(new URL("../docs/fork/UPSTREAM-FIXES.md", import.meta.url), "utf8");
  const upstreamHead = execFileSync("git", ["ls-remote", "https://github.com/multica-ai/multica.git", "refs/heads/main"], { encoding: "utf8" }).split(/\s/)[0];
  const r = check({ ledger, upstreamHead, now: new Date() });
  console.log(r.stale ? `STALE: ${r.reasons.join("; ")}` : "fresh");
  if (r.withheld.length) console.log(`withheld security-class rows to re-check: ${r.withheld.join(", ")}`);
  process.exit(r.stale ? 1 : 0);
}
