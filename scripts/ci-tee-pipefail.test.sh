#!/usr/bin/env bash
# CHE-4873: a workflow step that pipes a command into `tee` runs under the
# default `bash -e`, which has no pipefail: the pipe takes tee's exit status and
# a failing command leaves the step green. Every such step must set
# `shell: bash` (bash -eo pipefail) or run `set -o pipefail` itself. A comment
# that merely mentions pipefail does not count.
set -eu
cd "$(dirname "$0")/.."

scan() {
  awk '
    function flush() { if (tee && !ok) print FILENAME ":" tee; tee = 0; ok = 0 }
    FNR == 1 { flush() }
    /^ *- (name|id|uses|if|run):/ { flush() }
    /\| *tee( |$)/ && !/^ *#/ { tee = FNR }
    /^ *shell: bash/ || (!/^ *#/ && /^ *set -[a-zA-Z]*o pipefail/) { ok = 1 }
    END { flush() }
  ' "$@"
}

# Self-test: the guard must flag a tee step whose only "pipefail" is a comment,
# and must pass a step with `shell: bash` or an uncommented `set -o pipefail`.
fx=$(mktemp -d)
trap 'rm -rf "$fx"' EXIT
printf '%s\n' '- name: a' '  run: |' '    # pipefail would help here' '    go test ./... | tee out.log' > "$fx/comment-only.yml"
printf '%s\n' '- name: b' '  shell: bash' '  run: go test ./... | tee out.log' > "$fx/shell.yml"
printf '%s\n' '- name: c' '  run: |' '    set -euo pipefail' '    go test ./... | tee out.log' > "$fx/set.yml"
[ -n "$(scan "$fx/comment-only.yml")" ] || { echo "self-test: comment-only pipefail was not flagged" >&2; exit 1; }
[ -z "$(scan "$fx/shell.yml" "$fx/set.yml")" ] || { echo "self-test: protected step was flagged" >&2; exit 1; }

bad=$(scan .github/workflows/*.yml)
if [ -n "$bad" ]; then
  echo "steps that pipe into tee without pipefail (add 'shell: bash'):" >&2
  echo "$bad" >&2
  exit 1
fi
echo "ok: every tee step runs with pipefail"
