#!/usr/bin/env bash
set -euo pipefail

output=${1:?baseline output path is required}
mkdir -p ~/.ssh
printf '%s\n' "$C00_SSH_KEY" > ~/.ssh/c00_deploy_key
chmod 600 ~/.ssh/c00_deploy_key
ssh-keyscan -H "$C00_SSH_HOST" >> ~/.ssh/known_hosts 2>/dev/null

ssh_cmd=(ssh -i ~/.ssh/c00_deploy_key -o BatchMode=yes "${C00_SSH_USER}@${C00_SSH_HOST}")
remote_dir="${C00_STATE_DIR%/}/cd-baseline-$(date +%s)-$$"
trap '"${ssh_cmd[@]}" "rm -rf '\''$remote_dir'\''" || true' EXIT
"${ssh_cmd[@]}" "mkdir -p '$remote_dir'"
scp -i ~/.ssh/c00_deploy_key -o BatchMode=yes \
  deploy/cd/capture-tuple.sh deploy/cd/tuple-snapshot.mjs \
  "${C00_SSH_USER}@${C00_SSH_HOST}:${remote_dir}/"

active_colour="$("${ssh_cmd[@]}" "node -e 'const fs=require(\"node:fs\"); const p=\"${C00_CUTOVER_STATE_DIR%/}/cutover-state.json\"; const c=JSON.parse(fs.readFileSync(p,\"utf8\")).active_colour; if(!/^(blue|green)\$/.test(c)) process.exit(1); process.stdout.write(c)'" | tr -d '[:space:]')"
[[ "$active_colour" =~ ^(blue|green)$ ]]

baseline_sha=""
deployed_label="$("${ssh_cmd[@]}" "docker inspect --format '{{index .Config.Labels \"org.opencontainers.image.revision\"}}' \"\$(cd '${C00_COMPOSE_DIR}' && docker compose -f docker-compose.selfhost.yml -f deploy/cd/docker-compose.ab.yml ps -q backend-${active_colour})\"" 2>/dev/null | tr -d '[:space:]' || true)"
if ! [[ "$deployed_label" =~ ^[a-f0-9]{40}$ ]]; then
  deployed_tag="$("${ssh_cmd[@]}" "cd '${C00_COMPOSE_DIR}' && docker compose -f docker-compose.selfhost.yml -f deploy/cd/docker-compose.ab.yml images backend-${active_colour} --format json" 2>/dev/null \
    | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>{try{const r=JSON.parse(d||"[]");const x=Array.isArray(r)?r[0]:r;process.stdout.write(x&&x.Tag?String(x.Tag):"")}catch{process.stdout.write("")}})' || true)"
  candidate="${deployed_tag#sha-}"
  if [[ "$candidate" =~ ^[a-f0-9]{7,40}$ ]]; then
    baseline_sha="$(git rev-parse --verify --quiet "${candidate}^{commit}" 2>/dev/null || true)"
  fi
fi

capture_args=(--compose-dir "'${C00_COMPOSE_DIR}'" --output "'${remote_dir}/baseline-tuple.json'" --compose-service-suffix "'${active_colour}'")
if [[ "$baseline_sha" =~ ^[a-f0-9]{40}$ ]]; then
  capture_args+=(--application-sha "'${baseline_sha}'")
fi
"${ssh_cmd[@]}" "bash '${remote_dir}/capture-tuple.sh' ${capture_args[*]}"
scp -i ~/.ssh/c00_deploy_key -o BatchMode=yes \
  "${C00_SSH_USER}@${C00_SSH_HOST}:${remote_dir}/baseline-tuple.json" "$output"
