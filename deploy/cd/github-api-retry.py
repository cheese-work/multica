#!/usr/bin/env python3
"""Run a read-only gh API request, retrying rate limits within a caller deadline."""
from email.utils import parsedate_to_datetime
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


def main():
    deadline = int(sys.argv[1])
    while True:
        try:
            result = subprocess.run(["gh", "api", *sys.argv[2:], "--include"],
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
        if not rate_limited or now + delay >= deadline:
            sys.stderr.buffer.write(result.stderr)
            if rate_limited:
                print("GitHub rate-limit reset exceeds the checks deadline", file=sys.stderr)
            return result.returncode
        print(f"GitHub API rate limited (HTTP {status}); retrying in {delay:.0f}s", file=sys.stderr)
        time.sleep(delay)


if __name__ == "__main__":
    sys.exit(main())
