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

cap buttons \
  --component packages/ui/components/ui/button.tsx \
  --token button-height-default --token button-padding-default --token text-body --token bg-primary
cap dialogs \
  --component packages/ui/components/ui/dialog.tsx \
  --token dialog-enter-duration --token dialog-enter-easing --token bg-popover
cap dialogs-alert \
  --component packages/ui/components/ui/alert-dialog.tsx \
  --token dialog-enter-duration --token bg-popover
cap tabs \
  --component packages/ui/components/ui/tabs.tsx \
  --token bg-muted --token text-muted-foreground --token ring-ring
cap ui-copy \
  --token text-caption --token text-muted-foreground --token text-label
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

cap integration-card-grid \
  --component packages/ui/components/ui/card.tsx \
  --component packages/ui/components/ui/badge.tsx \
  --token text-body --token text-caption --token text-muted-foreground

cap settings-table-section \
  --component packages/ui/components/ui/table.tsx \
  --token text-muted-foreground --token tabular-nums

cap settings-action-row \
  --component packages/views/settings/components/settings-layout.tsx \
  --component packages/views/settings/components/workspace-scheduling-row.tsx \
  --token text-body --token text-caption --token text-muted-foreground \
  --copy "label=settings:workspace.scheduling_label" \
  --copy "hint=settings:workspace.scheduling_hint" \
  --copy "pause=settings:workspace.scheduling_pause_action"

cap create-shell \
  --component packages/views/agents/create/create-shell.tsx \
  --token text-body --token text-caption --token text-micro \
  --copy "title=agents:creation_studio.title" \
  --copy "stepper aria-label=agents:creation_studio.steps.aria"

cap confirm-dialog \
  --component packages/ui/components/ui/alert-dialog.tsx \
  --copy "title=settings:workspace.scheduling_pause_title" \
  --copy "description=settings:workspace.scheduling_pause_description" \
  --copy "cancel=settings:workspace.scheduling_keep" \
  --copy "confirm=settings:workspace.scheduling_confirm"

rm -rf "$build"
