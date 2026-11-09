#!/usr/bin/env bash
# CHE-4873: a workflow step that pipes a command into `tee` runs under the
# default `bash -e`, which has no pipefail: the pipe takes tee's exit status and
# a failing command leaves the step green. Every such step must set
# `shell: bash` (bash -eo pipefail) or `set -o pipefail` itself.
set -eu
cd "$(dirname "$0")/.."

bad=$(awk '
  function flush() { if (tee && !ok) print FILENAME ":" tee; tee = 0; ok = 0 }
  FNR == 1 { flush() }
  /^ *- (name|id|uses|if|run):/ { flush() }
  /\| *tee( |$)/ && !/^ *#/ { tee = FNR }
  /^ *shell: bash/ || /pipefail/ { ok = 1 }
  END { flush() }
' .github/workflows/*.yml)

if [ -n "$bad" ]; then
  echo "steps that pipe into tee without pipefail (add 'shell: bash'):" >&2
  echo "$bad" >&2
  exit 1
fi
echo "ok: every tee step runs with pipefail"
