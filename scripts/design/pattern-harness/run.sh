#!/usr/bin/env bash
# Render the real pattern components and capture each at 1440 and 390 (X99 only).
#   scripts/design/pattern-harness/run.sh <out-dir>
# Foreground only. Builds one inline HTML file, then screenshots it over file://.
set -euo pipefail
cd "$(dirname "$0")/../../.."
out="$(mkdir -p "${1:?usage: run.sh <out-dir>}" && cd "$1" && pwd)"
build="$out/.harness-build"
HARNESS_OUT="$build" node_modules/.bin/vite build --config scripts/design/pattern-harness/vite.config.mjs >"$out/harness-build.log" 2>&1
node -e '
const fs = require("fs"), d = process.argv[1];
const js = fs.readdirSync(d + "/assets").filter((f) => f.endsWith(".js")).map((f) => fs.readFileSync(d + "/assets/" + f, "utf8")).join("\n");
const css = fs.readdirSync(d + "/assets").filter((f) => f.endsWith(".css")).map((f) => fs.readFileSync(d + "/assets/" + f, "utf8")).join("\n");
const safe = (s, tag) => s.replaceAll("</" + tag, "<\\/" + tag);
fs.writeFileSync(d + "/page.html", `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><style>${safe(css, "style")}</style></head><body><div id="root"></div><script>${safe(js, "script")}</script></body></html>`);
' "$build"
base="file://$build/page.html?pattern"
cap() { local id=$1; shift; node scripts/design/capture-frame.mjs --url "$base=$id" --id "$id" --out "$out" "$@"; }

cap icon-button-action \
  --component packages/ui/components/common/icon-button.tsx \
  --component packages/ui/components/ui/button.tsx \
  --token text-caption --token text-muted-foreground --token size-icon-sm

cap property-picker \
  --component packages/views/issues/components/pickers/property-picker.tsx \
  --component packages/views/common/pill-button.tsx \
  --token PICKER_TRIGGER_CLASS --token text-body --token bg-accent --token w-56 \
  --copy "search placeholder=issues:filters.placeholder" \
  --copy "search aria-label=issues:pickers.filter_options_aria"

cap issue-mention-chip \
  --component packages/views/issues/components/issue-chip.tsx \
  --token ISSUE_CHIP_CLASS --token text-caption --token text-muted-foreground --token text-foreground

cap pause-banner \
  --component packages/views/layout/machine-pause-banner.tsx \
  --component packages/views/runtimes/components/machine-pause-state.ts \
  --token text-caption --token border-warning/60 --token bg-warning/15 --token bg-success/10 \
  --copy "banner title=runtimes:machine_pause.banner.paused" \
  --copy "resume=runtimes:machine_pause.resume"

cap run-budget \
  --component packages/views/issues/components/run-budget-meter.tsx \
  --token text-caption --token text-warning --token text-destructive --token text-muted-foreground \
  --copy "runs=issues:run_budget.runs" \
  --copy "near=issues:run_budget.near" \
  --copy "exhausted=issues:run_budget.exhausted"

cap attachment-card-list \
  --component packages/views/issues/components/attachment-list.tsx \
  --component packages/views/issues/components/deliverables/version-badge.tsx \
  --token text-micro --token bg-muted --token text-muted-foreground \
  --copy "revision badge=issues:attachment_revision.badge"

cap wakeup-rule-form \
  --component packages/views/issues/components/wakeup-instruction-editor.tsx \
  --token text-caption --token text-destructive \
  --copy "invalid=issues:wakeups.instruction_invalid" \
  --copy "conflict=issues:wakeups.conflict_error"

cap settings-number-row \
  --component packages/views/settings/components/auto-compact-tokens-row.tsx \
  --component packages/views/settings/components/settings-layout.tsx \
  --token text-caption --token text-destructive

rm -rf "$build"
