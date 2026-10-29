#!/usr/bin/env bash
set -euo pipefail

: "${RUNNER_TEMP:?}" "${GITHUB_PATH:?}"
package_manager=$(node -e 'const packageManager = require("./package.json").packageManager; if (!/^pnpm@\d+\.\d+\.\d+$/.test(packageManager)) throw new Error("Expected an exact pnpm packageManager version"); console.log(packageManager)')
npm install --prefix "${RUNNER_TEMP:?}/ci-pnpm" "$package_manager"
printf '%s\n' "$RUNNER_TEMP/ci-pnpm/node_modules/.bin" >> "${GITHUB_PATH:?}"
