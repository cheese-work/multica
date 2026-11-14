#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Build a self-contained sandbox with stub `curl` and a tarball that the
# release-binary fallback path will download. Each test supplies its own
# `brew` stub to model a specific Homebrew failure mode.
_setup_sandbox() {
  local tmp="$1"
  local stub_bin="$tmp/stub-bin"
  local install_bin="$tmp/install-bin"
  local payload_dir="$tmp/payload"
  mkdir -p "$stub_bin" "$install_bin" "$payload_dir"

  cat >"$payload_dir/multica" <<'STUB'
#!/usr/bin/env bash
echo "multica v0.3.2 (commit: test)"
STUB
  chmod +x "$payload_dir/multica"
  tar -czf "$tmp/multica.tar.gz" -C "$payload_dir" multica

  cat >"$stub_bin/curl" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" == *"-sI"* ]]; then
  printf 'HTTP/2 302\r\nlocation: https://github.com/multica-ai/multica/releases/tag/v0.3.2\r\n'
  exit 0
fi

out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o)
      out="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

if [[ -z "$out" ]]; then
  echo "stub curl expected -o" >&2
  exit 2
fi
cp "$MULTICA_TEST_ARCHIVE" "$out"
STUB
  chmod +x "$stub_bin/curl"
}

_run_installer() {
  local tmp="$1"
  local out="$tmp/install.out"
  local err="$tmp/install.err"
  if ! env -u HOMEBREW_NO_AUTO_UPDATE PATH="$tmp/stub-bin:$tmp/install-bin:/usr/bin:/bin" \
    MULTICA_BIN_DIR="$tmp/install-bin" \
    MULTICA_TEST_ARCHIVE="$tmp/multica.tar.gz" \
    bash "$ROOT_DIR/scripts/install.sh" >"$out" 2>"$err"; then
    echo "install.sh exited non-zero" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi

  if [[ ! -x "$tmp/install-bin/multica" ]]; then
    echo "expected fallback binary at $tmp/install-bin/multica" >&2
    cat "$out" >&2 || true
    cat "$err" >&2 || true
    return 1
  fi

  if ! grep -q "Homebrew output (last 80 lines):" "$err"; then
    echo "expected diagnostic tail in stderr" >&2
    cat "$err" >&2 || true
    return 1
  fi
}

test_brew_install_failure_falls_back_to_release_binary() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_sandbox "$tmp"
  cat >"$tmp/stub-bin/brew" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  tap)
    exit 0
    ;;
  install)
    echo "simulated brew install failure" >&2
    exit 42
    ;;
  list)
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
STUB
  chmod +x "$tmp/stub-bin/brew"

  _run_installer "$tmp"
}

test_brew_tap_failure_falls_back_to_release_binary() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_sandbox "$tmp"
  cat >"$tmp/stub-bin/brew" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  tap)
    echo "simulated brew tap failure" >&2
    exit 17
    ;;
  *)
    echo "brew $* should not be reached after tap failure" >&2
    exit 99
    ;;
esac
STUB
  chmod +x "$tmp/stub-bin/brew"

  _run_installer "$tmp"
}

test_remote_ssh_install_prints_token_login_hint() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_sandbox "$tmp"
  cat >"$tmp/stub-bin/brew" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  tap)
    exit 0
    ;;
  install)
    echo "simulated brew install failure" >&2
    exit 42
    ;;
  list)
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
STUB
  chmod +x "$tmp/stub-bin/brew"

  (
    export SSH_CONNECTION="192.0.2.10 54321 198.51.100.20 22"
    _run_installer "$tmp"
  )

  if ! grep -q "Looks like a remote/SSH session" "$tmp/install.out"; then
    echo "expected remote/SSH token-login hint in installer output" >&2
    cat "$tmp/install.out" >&2 || true
    return 1
  fi
  if ! grep -q "https://multica.ai/settings?tab=tokens" "$tmp/install.out"; then
    echo "expected direct API Tokens settings URL in installer output" >&2
    cat "$tmp/install.out" >&2 || true
    return 1
  fi
  if ! grep -q "Settings > API Tokens" "$tmp/install.out"; then
    echo "expected API Tokens tab name in installer output" >&2
    cat "$tmp/install.out" >&2 || true
    return 1
  fi
  if ! grep -q "multica login --token <YOUR_TOKEN>" "$tmp/install.out"; then
    echo "expected token login command in installer output" >&2
    cat "$tmp/install.out" >&2 || true
    return 1
  fi
  if grep -q "multica config set server_url" "$tmp/install.out"; then
    echo "did not expect default cloud server config command in installer output" >&2
    cat "$tmp/install.out" >&2 || true
    return 1
  fi
  if grep -q "multica config set app_url" "$tmp/install.out"; then
    echo "did not expect default cloud app config command in installer output" >&2
    cat "$tmp/install.out" >&2 || true
    return 1
  fi
}

test_local_install_does_not_print_token_login_hint() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_sandbox "$tmp"
  cat >"$tmp/stub-bin/brew" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  tap)
    exit 0
    ;;
  install)
    echo "simulated brew install failure" >&2
    exit 42
    ;;
  list)
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
STUB
  chmod +x "$tmp/stub-bin/brew"

  (
    unset SSH_CONNECTION SSH_CLIENT SSH_TTY
    _run_installer "$tmp"
  )

  if grep -q "Looks like a remote/SSH session" "$tmp/install.out"; then
    echo "did not expect remote/SSH token-login hint in local installer output" >&2
    cat "$tmp/install.out" >&2 || true
    return 1
  fi
  if grep -q "multica login --token <YOUR_TOKEN>" "$tmp/install.out"; then
    echo "did not expect token login command in local installer output" >&2
    cat "$tmp/install.out" >&2 || true
    return 1
  fi
}

# ---------------------------------------------------------------------------
# --with-server: the probed port and the printed port must both be the port
# Docker Compose reported (#6145)
#
# The installer used to derive the port from .env with its own copy of the alias
# chain. Compose gives the *calling environment* precedence over .env, so any
# ambient PORT / BACKEND_PORT / API_PORT / SERVER_PORT / FRONTEND_PORT moved the
# published port while the installer kept probing and printing the file value.
#
# Here the docker stub plays Compose: it answers `port` from the same resolution
# Compose performs, environment first, then .env. The installer must take that
# answer as given for both the health check and the summary — which is exactly
# what a .env-only derivation cannot do, because the two disagree in every case
# below. That real Compose resolves this way is proven separately, against real
# `docker compose config`, in scripts/selfhost-config.test.sh; that test needs a
# Docker CLI, which this job deliberately does not require.
# ---------------------------------------------------------------------------
_setup_server_sandbox() {
  local tmp="$1"
  local stub_bin="$tmp/stub-bin"
  local server_dir="$tmp/server"
  mkdir -p "$stub_bin" "$server_dir/.git"

  # Minimal self-host assets: only the port mapping matters here.
  cat >"$server_dir/.env.example" <<'ENVFILE'
PORT=8080
# BACKEND_PORT=8080
# API_PORT=8080
# SERVER_PORT=8080
FRONTEND_PORT=3000
JWT_SECRET=change-me-in-production
POSTGRES_PASSWORD=multica
DATABASE_URL=postgres://multica:multica@localhost:5432/multica?sslmode=disable
ENVFILE
  touch "$server_dir/docker-compose.selfhost.yml"

  # Compose stand-in. Resolves the published host port the way Compose does:
  # the process environment wins over .env, then the alias chain decides.
  cat >"$stub_bin/docker" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail

_env_file_value() {
  local key="$1" line
  line="$(grep -E "^${key}=" .env 2>/dev/null | tail -n 1 || true)"
  [ -n "$line" ] || return 1
  printf '%s' "${line#*=}"
}

# Environment first (Compose interpolation), then the env file.
_resolve() {
  local key="$1" from_env
  eval "from_env=\${$key-__unset__}"
  if [ "$from_env" != "__unset__" ]; then
    printf '%s' "$from_env"
    return 0
  fi
  _env_file_value "$key"
}

_published_backend_port() {
  local value
  for key in BACKEND_PORT API_PORT SERVER_PORT PORT; do
    if value="$(_resolve "$key")" && [ -n "$value" ]; then
      printf '%s' "$value"
      return
    fi
  done
  printf '8080'
}

_published_frontend_port() {
  local value
  if value="$(_resolve FRONTEND_PORT)" && [ -n "$value" ]; then
    printf '%s' "$value"
    return
  fi
  printf '3000'
}

case "${1:-}" in
  info) exit 0 ;;
  compose)
    shift
    subcommand=""
    for arg in "$@"; do
      case "$arg" in
        pull | up | port | version | ps | logs | down) subcommand="$arg"; break ;;
      esac
    done
    case "$subcommand" in
      port)
        service=""
        for arg in "$@"; do
          case "$arg" in
            backend | frontend) service="$arg"; break ;;
          esac
        done
        case "$service" in
          backend) printf '127.0.0.1:%s\n' "$(_published_backend_port)" ;;
          frontend) printf '127.0.0.1:%s\n' "$(_published_frontend_port)" ;;
          *) exit 1 ;;
        esac
        ;;
      version) echo "2.30.0" ;;
    esac
    exit 0
    ;;
esac
exit 0
STUB
  chmod +x "$stub_bin/docker"

  # git: the installer takes the "existing installation" path, so only the
  # fetch/checkout calls run and they are all tolerant of failure.
  printf '#!/usr/bin/env bash\nexit 0\n' >"$stub_bin/git"
  chmod +x "$stub_bin/git"

  # brew: pretend the CLI installs cleanly so the run reaches the summary.
  printf '#!/usr/bin/env bash\nexit 0\n' >"$stub_bin/brew"
  chmod +x "$stub_bin/brew"

  printf '#!/usr/bin/env bash\necho "multica v0.3.2 (commit: test)"\n' >"$stub_bin/multica"
  chmod +x "$stub_bin/multica"

  # curl records every probed URL so the health-check port can be asserted.
  cat >"$stub_bin/curl" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
for arg in "$@"; do
  case "$arg" in
    http*) printf '%s\n' "$arg" >>"$MULTICA_TEST_CURL_LOG" ;;
  esac
done
exit 0
STUB
  chmod +x "$stub_bin/curl"

  printf '#!/usr/bin/env bash\nhead -c 32 /dev/zero | od -An -tx1 | tr -d " \\n"\n' >"$stub_bin/openssl"
  chmod +x "$stub_bin/openssl"
}

# Runs `install.sh --with-server` with the sandbox stubs. Remaining arguments are
# ambient environment assignments, so each case controls the environment
# explicitly instead of inheriting a CI runner's PORT.
_run_with_server() {
  local tmp="$1"
  shift

  : >"$tmp/curl.log"
  if ! env -i \
    PATH="$tmp/stub-bin:/usr/bin:/bin" \
    HOME="$tmp" \
    MULTICA_INSTALL_DIR="$tmp/server" \
    MULTICA_SELFHOST_REF="main" \
    MULTICA_TEST_CURL_LOG="$tmp/curl.log" \
    "$@" \
    bash "$ROOT_DIR/scripts/install.sh" --with-server \
    >"$tmp/install.out" 2>"$tmp/install.err"; then
    echo "install.sh --with-server exited non-zero" >&2
    cat "$tmp/install.out" >&2 || true
    cat "$tmp/install.err" >&2 || true
    return 1
  fi
}

# Asserts the probed port and the printed ports all match the stub's answer.
_require_server_ports() {
  local tmp="$1" label="$2" expected_backend="$3" expected_frontend="$4"
  local probed printed_backend printed_frontend

  probed="$(sed -n '1s#.*localhost:\([0-9]*\)/health#\1#p' "$tmp/curl.log")"
  printed_backend="$(sed -n 's#.*Backend:[^0-9]*http://localhost:\([0-9]*\).*#\1#p' "$tmp/install.out" | head -n 1)"
  printed_frontend="$(sed -n 's#.*Frontend:[^0-9]*http://localhost:\([0-9]*\).*#\1#p' "$tmp/install.out" | head -n 1)"

  if [ "$probed" != "$expected_backend" ] ||
    [ "$printed_backend" != "$expected_backend" ] ||
    [ "$printed_frontend" != "$expected_frontend" ]; then
    echo "[$label] installer ports disagree with the port Compose published" >&2
    echo "  compose published:  backend=$expected_backend frontend=$expected_frontend" >&2
    echo "  health check probed: ${probed:-<none>}" >&2
    echo "  printed:            backend=${printed_backend:-<none>} frontend=${printed_frontend:-<none>}" >&2
    cat "$tmp/install.out" >&2 || true
    return 1
  fi
}

test_with_server_uses_compose_published_ports() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  # label | .env mutation (sed) | ambient env | expected backend | expected frontend
  local cases='defaults|||8080|3000
env-file PORT|s/^PORT=8080/PORT=9100/||9100|3000
env-file BACKEND_PORT|s/^# BACKEND_PORT=8080/BACKEND_PORT=9200/||9200|3000
env-file API_PORT|s/^# API_PORT=8080/API_PORT=9300/||9300|3000
env-file SERVER_PORT|s/^# SERVER_PORT=8080/SERVER_PORT=9400/||9400|3000
env-file FRONTEND_PORT|s/^FRONTEND_PORT=3000/FRONTEND_PORT=3100/||8080|3100
ambient PORT beats .env|s/^PORT=8080/PORT=9100/|PORT=9500|9500|3000
ambient BACKEND_PORT beats .env|s/^PORT=8080/PORT=9100/|BACKEND_PORT=9600|9600|3000
ambient API_PORT beats .env|s/^PORT=8080/PORT=9100/|API_PORT=9700|9700|3000
ambient SERVER_PORT beats .env|s/^PORT=8080/PORT=9100/|SERVER_PORT=9800|9800|3000
ambient FRONTEND_PORT beats .env|s/^FRONTEND_PORT=3000/FRONTEND_PORT=3100/|FRONTEND_PORT=3200|8080|3200
empty ambient BACKEND_PORT falls through|s/^PORT=8080/PORT=9100/|BACKEND_PORT=|9100|3000
empty env-file BACKEND_PORT falls through|s/^PORT=8080/PORT=9100/;s/^# BACKEND_PORT=8080/BACKEND_PORT=/||9100|3000'

  local label mutation ambient expect_backend expect_frontend
  while IFS='|' read -r label mutation ambient expect_backend expect_frontend; do
    [ -n "$label" ] || continue

    rm -rf "$tmp/server" "$tmp/stub-bin"
    _setup_server_sandbox "$tmp"
    cp "$tmp/server/.env.example" "$tmp/server/.env"
    if [ -n "$mutation" ]; then
      sed "$mutation" "$tmp/server/.env" >"$tmp/server/.env.new"
      mv "$tmp/server/.env.new" "$tmp/server/.env"
    fi

    if [ -n "$ambient" ]; then
      _run_with_server "$tmp" "$ambient" || return 1
    else
      _run_with_server "$tmp" || return 1
    fi
    _require_server_ports "$tmp" "$label" "$expect_backend" "$expect_frontend" || return 1
  done <<<"$cases"
}

test_with_server_fails_when_compose_port_is_unavailable() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  _setup_server_sandbox "$tmp"
  cp "$tmp/server/.env.example" "$tmp/server/.env"

  # Compose cannot report a port, e.g. the container never came up.
  cat >"$tmp/stub-bin/docker" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
case "${1:-}" in
  info) exit 0 ;;
  compose)
    for arg in "$@"; do
      case "$arg" in
        port) exit 1 ;;
        version) echo "2.30.0"; exit 0 ;;
      esac
    done
    exit 0
    ;;
esac
exit 0
STUB
  chmod +x "$tmp/stub-bin/docker"

  if _run_with_server "$tmp" >/dev/null 2>&1; then
    echo "installer must not report success when Compose cannot report the port" >&2
    cat "$tmp/install.out" >&2 || true
    return 1
  fi
  if ! grep -q "could not read the backend host port" "$tmp/install.err"; then
    echo "expected an explicit failure about the backend host port" >&2
    cat "$tmp/install.err" >&2 || true
    return 1
  fi
  if grep -q "server is running and CLI is ready" "$tmp/install.out"; then
    echo "installer claimed success despite an unresolved port" >&2
    cat "$tmp/install.out" >&2 || true
    return 1
  fi
}

# --- download progress (CHE-3541 step 3) ------------------------------------
# Sandbox whose archive is ~2.6 MB and whose `curl` hands it over in 512 KiB
# chunks, 0.4 s apart, so a pty run sees the bar move. FAKE_LENGTH becomes the
# HEAD Content-Length (unset: none) and FAKE_REDIRECT_LENGTH the redirect's;
# FAKE_FAIL=1 makes the download exit 22.
_setup_slow_sandbox() {
  local tmp="$1"
  _setup_sandbox "$tmp"
  head -c 2500000 /dev/urandom >"$tmp/payload/pad.bin"
  tar -czf "$tmp/multica.tar.gz" -C "$tmp/payload" multica pad.bin
  cat >"$tmp/stub-bin/curl" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" == *"-sIL"* ]]; then
  printf 'HTTP/2 302\r\ncontent-length: %s\r\n\r\nHTTP/2 200\r\n' "${FAKE_REDIRECT_LENGTH:-0}"
  if [[ -n "${FAKE_LENGTH:-}" ]]; then printf 'content-length: %s\r\n' "$FAKE_LENGTH"; fi
  printf '\r\n'
  exit 0
fi
if [[ "$*" == *"-sI"* ]]; then
  printf 'HTTP/2 302\r\nlocation: https://github.com/multica-ai/multica/releases/tag/v0.3.2\r\n'
  exit 0
fi
out=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then out="$2"; fi
  shift
done
trap "" HUP # closing the pty must not hide a leaked curl
echo $$ >>"$MULTICA_TEST_CURL_PIDS"
[[ "${FAKE_FAIL:-}" == 1 ]] && { echo "curl: (22) The requested URL returned error: 404" >&2; exit 22; }
n=0
size=$(wc -c <"$MULTICA_TEST_ARCHIVE")
while ((n * 524288 < size)); do
  dd if="$MULTICA_TEST_ARCHIVE" bs=524288 skip=$n count=1 2>/dev/null >>"$out"
  n=$((n + 1))
  sleep 0.4
done
STUB
  chmod +x "$tmp/stub-bin/curl"
}

# Run install.sh with stderr on a pty (script(1)); stdout+stderr land in
# $tmp/tty.out. The shell's pid goes to $tmp/shell.pid. Extra args are env.
_run_installer_tty() {
  local tmp="$1"; shift
  env "$@" PATH="$tmp/stub-bin:$tmp/install-bin:/usr/bin:/bin" \
    MULTICA_BIN_DIR="$tmp/install-bin" \
    MULTICA_TEST_ARCHIVE="$tmp/multica.tar.gz" \
    MULTICA_TEST_CURL_PIDS="$tmp/curl.pids" \
    script -qec "echo \$\$ >'$tmp/shell.pid'; exec bash '$ROOT_DIR/scripts/install.sh'" /dev/null \
    >"$tmp/tty.out" 2>&1
}

test_tty_download_renders_known_size_bar() {
  local tmp size
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_slow_sandbox "$tmp"
  size=$(wc -c <"$tmp/multica.tar.gz")

  _run_installer_tty "$tmp" FAKE_LENGTH="$size" || { cat "$tmp/tty.out" >&2; return 1; }
  tr '\r' '\n' <"$tmp/tty.out" >"$tmp/tty.lines"
  grep -Eq '^\[[#-]{20}\] +[0-9]+%  [0-9.]+ / 2\.[0-9] MB' "$tmp/tty.lines" || {
    echo "expected a known-size bar line" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  grep -Eq '^\[#{20}\] 100%' "$tmp/tty.lines" || {
    echo "expected the bar to reach 100%" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  for phase in Extracting Installing Installed; do
    grep -qx "$phase" "$tmp/tty.lines" || { echo "missing phase line: $phase" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  done
}

test_tty_download_renders_unknown_size_line() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_slow_sandbox "$tmp"

  _run_installer_tty "$tmp" || { cat "$tmp/tty.out" >&2; return 1; }
  tr '\r' '\n' <"$tmp/tty.out" >"$tmp/tty.lines"
  grep -Eq '^[0-9.]+ MB  [0-9.]+ MB/s' "$tmp/tty.lines" || {
    echo "expected an unknown-size line with a rate" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  if grep -q '^\[' "$tmp/tty.lines"; then
    echo "unknown size must not draw a bar" >&2; return 1
  fi
}

test_tty_redirect_length_does_not_leak_into_final_response() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_slow_sandbox "$tmp"

  _run_installer_tty "$tmp" FAKE_REDIRECT_LENGTH=123 || { cat "$tmp/tty.out" >&2; return 1; }
  tr '\r' '\n' <"$tmp/tty.out" >"$tmp/tty.lines"
  if grep -q '^\[' "$tmp/tty.lines"; then
    echo "a redirect's Content-Length must not become the bar total" >&2
    cat "$tmp/tty.lines" >&2; return 1
  fi
  grep -Eq '^[0-9.]+ MB  [0-9.]+ MB/s' "$tmp/tty.lines" || {
    echo "expected unknown-size mode" >&2; cat "$tmp/tty.lines" >&2; return 1; }
}

test_tty_download_failure_keeps_message_and_exit_code() {
  local tmp rc=0
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_slow_sandbox "$tmp"

  _run_installer_tty "$tmp" FAKE_FAIL=1 || rc=$?
  [ "$rc" -eq 1 ] || { echo "expected exit 1, got $rc" >&2; return 1; }
  grep -q "Failed to download CLI binary." "$tmp/tty.out" || {
    echo "expected the download failure message" >&2; cat "$tmp/tty.out" >&2; return 1; }
  grep -q "returned error: 404" "$tmp/tty.out" || {
    echo "expected curl's own error to be replayed" >&2; cat "$tmp/tty.out" >&2; return 1; }
}

test_tty_interrupt_leaves_no_download_process() {
  local tmp pid i rc
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_slow_sandbox "$tmp"

  _run_installer_tty "$tmp" &
  local runner=$!
  for i in $(seq 50); do [ -s "$tmp/curl.pids" ] && break; sleep 0.1; done
  [ -s "$tmp/curl.pids" ] || { echo "download never started" >&2; return 1; }
  sleep 0.5
  kill -TERM "$(cat "$tmp/shell.pid")"
  rc=0
  wait "$runner" || rc=$?
  [ "$rc" -eq 143 ] || { echo "expected exit 143 after TERM, got $rc" >&2; return 1; }
  sleep 0.3
  while read -r pid; do
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      echo "download process $pid survived the interrupt" >&2
      return 1
    fi
  done <"$tmp/curl.pids"
}

test_non_tty_download_stays_quiet() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_slow_sandbox "$tmp"
  local size
  size=$(wc -c <"$tmp/multica.tar.gz")

  FAKE_LENGTH="$size" MULTICA_TEST_CURL_PIDS="$tmp/curl.pids" _run_installer "$tmp" 2>/dev/null || true
  [ -x "$tmp/install-bin/multica" ] || { echo "install failed" >&2; cat "$tmp/install.err" >&2; return 1; }
  if grep -q $'\r\|Extracting\|Installing$\|Installed$' "$tmp/install.err" "$tmp/install.out"; then
    echo "non-TTY output must carry no progress" >&2
    cat "$tmp/install.err" "$tmp/install.out" >&2
    return 1
  fi
}

# --- Homebrew progress (CHE-3541 step 4) -----------------------------------
# Fake `brew` whose `install`/`upgrade` "download" into $FAKE_CACHE the way real
# Homebrew does: 3 x 512 KiB appended to <path>.incomplete, 0.4 s apart, then a
# rename to <path>. Knobs: FAKE_DEPS (names of dependency downloads, 1 MiB
# each), FAKE_PRESEED (owned path already cached), FAKE_UNRELATED (a bigger
# foreign .incomplete grows alongside), FAKE_RETRY (partial file deleted and
# restarted), FAKE_INSTALL_FAIL, FAKE_LIST_RC (`brew list` exit, default 1).
# Every call is appended to $FAKE_BREW_LOG.
_setup_brew_sandbox() {
  local tmp="$1"
  _setup_slow_sandbox "$tmp"
  mkdir -p "$tmp/cache"
  cat >"$tmp/stub-bin/brew" <<'STUB'
#!/usr/bin/env bash
log() { echo "$*" >>"$FAKE_BREW_LOG"; }
grow() { head -c "$2" /dev/zero >>"$1"; }
main="$FAKE_CACHE/multica--0.8.0"
case "${1:-}" in
  tap) exit 0 ;;
  update) log "update"; exit 0 ;;
  list) exit "${FAKE_LIST_RC:-1}" ;;
  trust) log "$*"; touch "$FAKE_CACHE/.trusted"; exit 0 ;;
  --cache|deps)
    # Homebrew 4.6+ refuses to load a formula from a tap nobody trusted yet.
    if [[ ! -e "$FAKE_CACHE/.trusted" ]]; then
      echo "Error: Refusing to load formula $2 from untrusted tap multica-ai/tap." >&2; exit 1
    fi
    [[ "$1" == deps ]] && { log "deps $2"; for d in ${FAKE_DEPS:-}; do echo "$d"; done; exit 0; }
    log "$*"; shift
    for a in "$@"; do echo "$FAKE_CACHE/${a##*/}--0.8.0"; done ;;
  install|upgrade)
    log "$1 $2 auto=${HOMEBREW_NO_AUTO_UPDATE:-}"
    if [[ -n "${FAKE_INSTALL_FAIL:-}" ]]; then echo "simulated brew install failure" >&2; exit 42; fi
    if [[ -z "${FAKE_PRESEED:-}" ]]; then
      if [[ -n "${FAKE_UNRELATED:-}" ]]; then
        ( for i in 1 2 3 4; do grow "$FAKE_CACHE/other--1.0.incomplete" 1048576; sleep 0.4; done ) &
      fi
      if [[ -n "${FAKE_RETRY:-}" ]]; then
        grow "$main.incomplete" 1048576; sleep 0.5; rm -f "$main.incomplete"; sleep 0.5
      fi
      for i in 1 2 3; do grow "$main.incomplete" 524288; sleep 0.4; done
      mv "$main.incomplete" "$main"; sleep 0.5
      for d in ${FAKE_DEPS:-}; do
        grow "$FAKE_CACHE/$d--0.8.0.incomplete" 1048576; sleep 0.4
        mv "$FAKE_CACHE/$d--0.8.0.incomplete" "$FAKE_CACHE/$d--0.8.0"; sleep 0.5
      done
      wait
    fi
    tar -xzf "$MULTICA_TEST_ARCHIVE" -C "$MULTICA_BIN_DIR" multica ;;
  *) exit 0 ;;
esac
STUB
  chmod +x "$tmp/stub-bin/brew"
}

_run_brew_tty() {
  local tmp="$1"; shift
  _run_installer_tty "$tmp" FAKE_CACHE="$tmp/cache" FAKE_BREW_LOG="$tmp/brew.log" "$@"
}

# Fails unless every "N MB" line start in $1 is >= the one before it.
_mb_never_drops() {
  grep -Eo '^[0-9.]+ MB' "$1" | awk '{ if ($1 < p) bad = 1; p = $1 } END { exit bad }'
}

_brew_phases_present() {
  local phase
  for phase in "Updating Homebrew" "Downloading and installing via Homebrew" "Installed"; do
    grep -qx "$phase" "$1" || { echo "missing phase line: $phase" >&2; cat "$1" >&2; return 1; }
  done
}

test_tty_brew_install_renders_measured_progress() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_brew_sandbox "$tmp"

  _run_brew_tty "$tmp" || { cat "$tmp/tty.out" >&2; return 1; }
  tr '\r' '\n' <"$tmp/tty.out" >"$tmp/tty.lines"
  _brew_phases_present "$tmp/tty.lines"
  grep -Eq '^[0-9.]+ MB  [0-9.]+ MB/s' "$tmp/tty.lines" || {
    echo "expected MB and rate before exit" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  grep -Eq '^1\.5 MB' "$tmp/tty.lines" || { echo "expected 1.5 MB" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  _mb_never_drops "$tmp/tty.lines" || { echo "done count dropped across the rename" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  grep -q "auto=1" "$tmp/brew.log" || { echo "install must run with HOMEBREW_NO_AUTO_UPDATE=1" >&2; cat "$tmp/brew.log" >&2; return 1; }
  grep -qx "trust --formula multica-ai/tap/multica" "$tmp/brew.log" || { echo "expected brew trust for the formula" >&2; cat "$tmp/brew.log" >&2; return 1; }
  grep -qx "update" "$tmp/brew.log" || { echo "expected brew update" >&2; return 1; }
}

test_tty_brew_ignores_unrelated_incomplete_file() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_brew_sandbox "$tmp"

  _run_brew_tty "$tmp" FAKE_UNRELATED=1 || { cat "$tmp/tty.out" >&2; return 1; }
  tr '\r' '\n' <"$tmp/tty.out" >"$tmp/tty.lines"
  grep -Eq '^1\.5 MB' "$tmp/tty.lines" || { echo "expected 1.5 MB" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  if grep -Eq '^(1\.[6-9]|[2-9]\.)[0-9]? MB' "$tmp/tty.lines"; then
    echo "an unrelated .incomplete file was counted" >&2; cat "$tmp/tty.lines" >&2; return 1
  fi
}

test_tty_brew_cache_hit_draws_no_line() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_brew_sandbox "$tmp"
  head -c 1048576 /dev/zero >"$tmp/cache/multica--0.8.0"

  _run_brew_tty "$tmp" FAKE_PRESEED=1 || { cat "$tmp/tty.out" >&2; return 1; }
  tr '\r' '\n' <"$tmp/tty.out" >"$tmp/tty.lines"
  if grep -Eq '^[0-9.]+ MB' "$tmp/tty.lines"; then
    echo "all paths cached: no progress line expected" >&2; cat "$tmp/tty.lines" >&2; return 1
  fi
  _brew_phases_present "$tmp/tty.lines"
}

test_tty_brew_retry_never_shows_negative_rate() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_brew_sandbox "$tmp"

  _run_brew_tty "$tmp" FAKE_RETRY=1 || { cat "$tmp/tty.out" >&2; return 1; }
  tr '\r' '\n' <"$tmp/tty.out" >"$tmp/tty.lines"
  grep -Eq '^1\.5 MB' "$tmp/tty.lines" || { echo "expected 1.5 MB" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  if grep -Eq -- '-[0-9.]+ MB/s' "$tmp/tty.lines"; then
    echo "negative rate after the partial file restarted" >&2; cat "$tmp/tty.lines" >&2; return 1
  fi
}

test_tty_brew_counts_dependency_download() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_brew_sandbox "$tmp"

  _run_brew_tty "$tmp" FAKE_DEPS=depdl || { cat "$tmp/tty.out" >&2; return 1; }
  tr '\r' '\n' <"$tmp/tty.out" >"$tmp/tty.lines"
  grep -Eq '^2\.5 MB' "$tmp/tty.lines" || { echo "expected 2.5 MB incl. the dependency" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  grep -q "^--cache multica-ai/tap/multica depdl" "$tmp/brew.log" || {
    echo "expected brew --cache over the formula and its deps" >&2; cat "$tmp/brew.log" >&2; return 1; }
}

test_tty_brew_upgrade_renders_measured_progress() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_brew_sandbox "$tmp"
  printf '#!/usr/bin/env bash\necho "multica v0.3.1 (commit: old)"\n' >"$tmp/install-bin/multica"
  chmod +x "$tmp/install-bin/multica"

  _run_brew_tty "$tmp" FAKE_LIST_RC=0 || { cat "$tmp/tty.out" >&2; return 1; }
  tr '\r' '\n' <"$tmp/tty.out" >"$tmp/tty.lines"
  _brew_phases_present "$tmp/tty.lines"
  grep -Eq '^[0-9.]+ MB  [0-9.]+ MB/s' "$tmp/tty.lines" || {
    echo "expected MB and rate before exit" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  grep -q "^upgrade multica-ai/tap/multica auto=1" "$tmp/brew.log" || { cat "$tmp/brew.log" >&2; return 1; }
  grep -q "upgraded via Homebrew" "$tmp/tty.out" || { echo "expected the success message" >&2; return 1; }
}

test_tty_brew_install_failure_falls_back_with_progress() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_brew_sandbox "$tmp"

  _run_brew_tty "$tmp" FAKE_INSTALL_FAIL=1 || { cat "$tmp/tty.out" >&2; return 1; }
  tr '\r' '\n' <"$tmp/tty.out" >"$tmp/tty.lines"
  grep -q "Homebrew output (last 80 lines):" "$tmp/tty.out" || { echo "expected the failure dump" >&2; cat "$tmp/tty.out" >&2; return 1; }
  grep -q "simulated brew install failure" "$tmp/tty.out" || { echo "expected the captured brew output" >&2; return 1; }
  grep -Eq '^[0-9.]+ MB  [0-9.]+ MB/s' "$tmp/tty.lines" || {
    echo "fallback download must show the T2 line" >&2; cat "$tmp/tty.lines" >&2; return 1; }
  [ -x "$tmp/install-bin/multica" ] || { echo "fallback binary missing" >&2; return 1; }
}

test_non_tty_brew_path_is_unchanged() {
  local tmp
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  _setup_brew_sandbox "$tmp"

  FAKE_CACHE="$tmp/cache" FAKE_BREW_LOG="$tmp/brew.log" FAKE_INSTALL_FAIL=1 _run_installer "$tmp"
  if grep -Eq '^(update|--cache|deps)' "$tmp/brew.log"; then
    echo "non-TTY must not call update, --cache or deps" >&2; cat "$tmp/brew.log" >&2; return 1
  fi
  grep -q "^install multica-ai/tap/multica auto=$" "$tmp/brew.log" || {
    echo "non-TTY install must not set HOMEBREW_NO_AUTO_UPDATE" >&2; cat "$tmp/brew.log" >&2; return 1; }
  if grep -q $'\r\|Updating Homebrew\|Installed$' "$tmp/install.err" "$tmp/install.out"; then
    echo "non-TTY output must carry no progress" >&2; return 1
  fi
}

test_brew_install_failure_falls_back_to_release_binary
test_brew_tap_failure_falls_back_to_release_binary
test_remote_ssh_install_prints_token_login_hint
test_local_install_does_not_print_token_login_hint
test_with_server_uses_compose_published_ports
test_with_server_fails_when_compose_port_is_unavailable
test_tty_download_renders_known_size_bar
test_tty_download_renders_unknown_size_line
test_tty_redirect_length_does_not_leak_into_final_response
test_tty_download_failure_keeps_message_and_exit_code
test_tty_interrupt_leaves_no_download_process
test_non_tty_download_stays_quiet
test_tty_brew_install_renders_measured_progress
test_tty_brew_ignores_unrelated_incomplete_file
test_tty_brew_cache_hit_draws_no_line
test_tty_brew_retry_never_shows_negative_rate
test_tty_brew_counts_dependency_download
test_tty_brew_upgrade_renders_measured_progress
test_tty_brew_install_failure_falls_back_with_progress
test_non_tty_brew_path_is_unchanged
echo "install.sh tests passed"
