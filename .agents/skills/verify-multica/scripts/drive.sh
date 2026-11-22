#!/usr/bin/env bash
# Drive the pending weekly run-guard decision card in a real browser.
# Usage: drive.sh <evidence-dir>   (outside the run dir; survives cleanup)
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
ev="$(mkdir -p "${1:?usage: drive.sh <evidence-dir>}" && cd "$1" && pwd)"
cd "$root"
"$root/.agents/skills/verify-multica/scripts/doctor.sh" | tee "$ev/doctor.log"
# Carry the run id so cleanup can stop any browser process this drive leaves.
. "$root/.agents/skills/verify-multica/scripts/ownership.sh"
RUN_ID="$(vm_value "$(vm_marker)" RUN_ID)"; [ -z "$RUN_ID" ] || export VERIFY_MULTICA_RUN_ID="$RUN_ID"
export EVIDENCE_DIR="$ev"
make env-exec ARGS="-- pnpm exec playwright test -c .agents/skills/verify-multica/scripts/playwright.config.ts" 2>&1 | tee "$ev/drive.log"
