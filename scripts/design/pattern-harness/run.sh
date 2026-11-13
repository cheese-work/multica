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

rm -rf "$build"
