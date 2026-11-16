#!/usr/bin/env bash
id="$1"; out="data/comments/$id.json"
[ -s "$out" ] && exit 0
"$MULTICA_CLI_PATH" issue comment list "$id" --full --compact --output json > "$out.tmp" 2>/dev/null && mv "$out.tmp" "$out" || echo "FAIL $id" >&2
