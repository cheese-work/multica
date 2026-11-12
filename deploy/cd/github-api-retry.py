#!/usr/bin/env python3
"""Run a read-only gh API request, retrying rate limits within a caller deadline."""
from email.utils import parsedate_to_datetime
import os
import re
import subprocess
import sys
import time


def response_parts(output):
    # --paginate emits a header block per page. Buffer the whole request so
    # a failed later page cannot leak a partial snapshot to the caller.
    body = []
    headers = {}
    status = None
    in_headers = False
    for line in output.splitlines(keepends=True):
        match = re.fullmatch(rb"HTTP/[\d.]+ (\d{3})[^\r\n]*\r?\n?", line)
        if match:
            status = int(match[1])
            headers = {}
            in_headers = True
        elif in_headers:
            if not line.strip():
                in_headers = False
            elif b":" in line:
                key, value = line.decode("utf-8", errors="replace").split(":", 1)
                headers[key.lower()] = value.strip()
        else:
            body.append(line)
    return status, headers, b"".join(body)


def retry_delay(headers, now):
    retry_after = headers.get("retry-after")
    delays = []
    try:
        if retry_after is not None:
            try:
                delays.append(int(retry_after))
            except ValueError:
                delays.append(parsedate_to_datetime(retry_after).timestamp() - now)
        if headers.get("x-ratelimit-remaining") == "0" and "x-ratelimit-reset" in headers:
            delays.append(int(headers["x-ratelimit-reset"]) - now)
    except (ValueError, TypeError, OverflowError):
        pass
    if delays:
        return max(1, *delays)
    # GitHub asks secondary-rate-limit clients without a reset to wait a minute.
    return 60


def token_label(token):
    for name, env in (("app", "APP_TOKEN"), ("fallback", "FALLBACK_TOKEN")):
        if token and token == os.environ.get(env):
            return name
    return "env"


def describe(status, headers, body, stderr):
    # Secret-free: status, bucket and the API message only, so the next log shows which token hit which limit.
    message = (stderr or body).decode("utf-8", errors="replace").strip().splitlines()
    return (f"HTTP {status} resource={headers.get('x-ratelimit-resource', '?')} "
            f"remaining={headers.get('x-ratelimit-remaining', '?')} reset={headers.get('x-ratelimit-reset', '?')} "
            f"retry-after={headers.get('retry-after', '-')} message={(message[0] if message else '')[:160]!r}")


def app_budget_allows(token):
    # Same rule as pick-api-token.sh: the App token is only for use while it keeps the server's reserve.
    # /rate_limit does not count against the limit it reports.
    needed = int(os.environ.get("APP_TOKEN_FLOOR", "1500")) + int(os.environ.get("APP_TOKEN_HEADROOM", "100"))
    env = dict(os.environ, GH_TOKEN=token)
    try:
        out = subprocess.run(["gh", "api", "rate_limit", "--jq", ".resources.core.remaining"], env=env,
                             capture_output=True, text=True, timeout=30).stdout.strip()
        remaining = int(out)
    except (subprocess.TimeoutExpired, ValueError):
        remaining = None
    if remaining is not None and remaining >= needed:
        return True
    print(f"Not failing over to the app token: its budget {remaining if remaining is not None else 'unreadable'} "
          f"is below {needed} (floor plus headroom)", file=sys.stderr)
    return False


def next_token(tokens, untried):
    while untried:
        candidate = min(untried)
        untried.discard(candidate)
        if token_label(tokens[candidate]) != "app" or app_budget_allows(tokens[candidate]):
            return candidate
    return None


def main():
    deadline = int(sys.argv[1])
    max_sleep = int(os.environ.get("GITHUB_API_MAX_SLEEP", "60"))
    # CHE-4875: a rate-limited bucket can stay empty for most of an hour while the other token has budget,
    # so move to the next token before sleeping.
    tokens = list(dict.fromkeys(t for t in (os.environ.get("GH_TOKEN"), os.environ.get("APP_TOKEN"),
                                            os.environ.get("FALLBACK_TOKEN")) if t))
    current = 0
    untried = set(range(1, len(tokens)))
    while True:
        env = dict(os.environ)
        if tokens:
            env["GH_TOKEN"] = tokens[current]
        try:
            result = subprocess.run(["gh", "api", *sys.argv[2:], "--include"], env=env,
                                    capture_output=True, timeout=max(30, deadline - time.time()))
        except subprocess.TimeoutExpired:
            print("GitHub API request exceeded the checks deadline", file=sys.stderr)
            return 1
        status, headers, body = response_parts(result.stdout)
        if result.returncode == 0:
            sys.stdout.buffer.write(body)
            return 0
        rate_limited = status == 429 or (status == 403 and (
            headers.get("x-ratelimit-remaining") == "0" or "retry-after" in headers
            or b"rate limit" in (body + result.stderr).lower()))
        now = time.time()
        delay = retry_delay(headers, now)
        if not rate_limited:
            sys.stderr.buffer.write(result.stderr)
            return result.returncode
        label = token_label(tokens[current]) if tokens else "env"
        print(f"GitHub API rate limited on the {label} token: {describe(status, headers, body, result.stderr)}",
              file=sys.stderr)
        failover = next_token(tokens, untried)
        if failover is not None:
            current = failover
            print(f"Retrying now with the {token_label(tokens[current])} token", file=sys.stderr)
            continue
        if delay > max_sleep or now + delay >= deadline:
            sys.stderr.buffer.write(result.stderr)
            print(f"GitHub rate-limit reset in {delay:.0f}s exceeds the {max_sleep}s sleep cap or the checks deadline",
                  file=sys.stderr)
            return result.returncode
        print(f"GitHub API rate limited (HTTP {status}); retrying in {delay:.0f}s", file=sys.stderr)
        time.sleep(delay)
        untried = set(range(len(tokens))) - {current}


if __name__ == "__main__":
    sys.exit(main())
