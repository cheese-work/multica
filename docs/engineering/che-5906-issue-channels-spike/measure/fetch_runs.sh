#!/usr/bin/env bash
id="$1"; out="data/runs/$id.json"
[ -s "$out" ] && exit 0
"$MULTICA_CLI_PATH" issue runs "$id" --output json 2>/dev/null | sed -n '/^\[/,$p' > "$out.tmp" && mv "$out.tmp" "$out" || echo "FAIL $id" >&2
