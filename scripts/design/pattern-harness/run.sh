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

cap settings-page \
  --component packages/views/settings/components/settings-layout.tsx \
  --token text-title-lg --token text-body --token text-caption --token divide-surface-border --token min-h-16 \
  --copy "scope badge=settings:layout.scope.device" \
  --copy "scope badge, workspace=settings:layout.scope.workspace"

cap settings-navigation \
  --component packages/views/settings/components/settings-page.tsx \
  --token h-10 --token text-body --token bg-surface-selected --token border-input \
  --copy "select label=settings:page.navigate" \
  --copy "search placeholder=settings:page.search" \
  --copy "page title=settings:page.title"

cap read-only-member-view \
  --component packages/views/settings/components/settings-layout.tsx \
  --token bg-surface --token border-surface-border --token text-caption --token text-muted-foreground \
  --copy "title=settings:layout.read_only.title" \
  --copy "with names=settings:layout.read_only.contact" \
  --copy "no names=settings:layout.read_only.description"

cap save-state \
  --component packages/views/settings/components/settings-layout.tsx \
  --token text-caption --token text-muted-foreground --token text-success --token text-destructive

cap lazy-detail \
  --token text-body --token text-caption --token text-muted-foreground --token border-surface-border --token bg-surface

cap decision-receipt-card \
  --token text-caption --token text-label --token text-micro --token bg-muted --token border-surface-border --token bg-surface

cap collapsible-section \
  --component packages/ui/components/ui/collapsible.tsx \
  --token text-caption --token text-muted-foreground --token text-body \
  --copy "trigger aria-label=chat:message_list.show_earlier_context" \
  --copy "line=chat:message_list.continued_from"

cap settings-select-row \
  --component packages/views/settings/components/settings-layout.tsx \
  --component packages/ui/components/ui/select.tsx \
  --token SETTINGS_CONTROL_WIDTHS --token text-body --token text-caption --token divide-surface-border \
  --copy "label=settings:workspace.idle_guard_label" \
  --copy "description=settings:workspace.idle_guard_hint"

cap pill-menu \
  --component packages/views/issues/components/wakeup-pickers.tsx \
  --component packages/ui/components/ui/dropdown-menu.tsx \
  --token pillClass --token text-label --token border-surface-border --token bg-accent

rm -rf "$build"
