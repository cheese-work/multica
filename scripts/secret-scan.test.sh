#!/usr/bin/env bash
# CHE-2696: secret-scan.sh fails on a planted key in the scanned range and passes on a clean one.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
t="$(mktemp -d)"; trap 'rm -rf "$t"' EXIT
cd "$t"; git init -q -b main; git config user.email t@t; git config user.name t
echo ok > a; git add a; git commit -qm clean; base="$(git rev-parse HEAD)"
echo "aws_key = \"AKIA$(printf 'QYLPMN5HHQLC4B6X')\"" > b.env; git add b.env; git commit -qm leak; leak="$(git rev-parse HEAD)"
echo more >> a; git commit -qam clean2; head="$(git rev-parse HEAD)"

fail() { echo "FAIL: $*" >&2; exit 1; }
"$here/secret-scan.sh" "$base..$leak" >/dev/null 2>&1 && fail "planted key in range was not caught"
"$here/secret-scan.sh" "$base..$head" >/dev/null 2>&1 && fail "leak commit inside the range was not caught"
"$here/secret-scan.sh" "$leak..$head" >/dev/null 2>&1 || fail "clean range was flagged"
echo PASS
