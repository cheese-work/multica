import argparse
import datetime
import json
import os
import re
import subprocess
import sys
import urllib.request


QUEUE_REF_PREFIX = "refs/heads/gh-readonly-queue/"
QUEUE_REF_IDLE = datetime.timedelta(hours=1)
MANAGED_PREFIXES = ("turbo-", "go-build-race-", "release-go-", "golangci-lint.cache-", "go-job-", "go-cd-")
SAVE_BUDGET = 9_000_000_000


def parse_time(value):
    return datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))


def cache_family(key):
    """Return (family, managed). A managed family keeps one generation: the ref and the
    trailing content hash or run counters of the key are not part of it."""
    if not key.startswith(MANAGED_PREFIXES):
        return key, False
    family = key.split("-refs/", 1)[0]
    return re.sub(r"-(?:[a-f0-9]{40}|[a-f0-9]{64})$", "", family), True


def plan_deletions(
    caches,
    default_ref="refs/heads/main",
    target=9_000_000_000,
    limit=10_000_000_000,
    now=None,
):
    now = now or datetime.datetime.now(datetime.timezone.utc)
    newest = {}
    deleted = []
    for cache in sorted(caches, key=lambda entry: (entry["created_at"], entry["id"]), reverse=True):
        # A merge-queue ref is deleted once its entry merges, so nothing can restore this
        # entry again. Wait an hour so a run still using it is not disturbed.
        if cache["ref"].startswith(QUEUE_REF_PREFIX) and now - parse_time(
            cache["last_accessed_at"]
        ) > QUEUE_REF_IDLE:
            deleted.append(cache["id"])
            continue
        family, managed = cache_family(cache["key"])
        group = (cache["ref"], family, None if managed else cache.get("version"))
        if group in newest:
            deleted.append(cache["id"])
        else:
            newest[group] = cache
    protected = {cache["id"] for cache in newest.values() if cache["ref"] == default_ref}
    required = sum(cache["size_in_bytes"] for cache in newest.values() if cache["id"] in protected)
    if required > limit:
        raise ValueError(
            f"Required default-branch caches need {required} bytes; free limit is {limit} bytes. Stop; no paid storage."
        )
    remaining = list(newest.values())
    size = sum(cache["size_in_bytes"] for cache in remaining)
    for cache in sorted(remaining, key=lambda entry: (entry["last_accessed_at"], entry["id"])):
        if size <= max(target, required):
            break
        if cache["id"] not in protected:
            deleted.append(cache["id"])
            size -= cache["size_in_bytes"]
    return sorted(deleted)


def api(endpoint):
    return json.loads(subprocess.check_output(["gh", "api", endpoint], text=True))


def http_api(endpoint):
    """GET one GitHub API endpoint with GITHUB_TOKEN. No gh CLI, so a self-hosted runner needs none."""
    request = urllib.request.Request(
        os.environ.get("GITHUB_API_URL", "https://api.github.com").rstrip("/") + "/" + endpoint,
        headers={
            "Accept": "application/vnd.github+json",
            "Authorization": "Bearer " + os.environ["GITHUB_TOKEN"],
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def read_all_caches(repository):
    """Every cache entry, or raise: a short or failed read must never look like a complete list."""
    caches = []
    page = 1
    while True:
        body = http_api(f"repos/{repository}/actions/caches?per_page=100&page={page}")
        batch = body["actions_caches"]
        caches.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    if len(caches) != body["total_count"]:
        raise ValueError(f"cache list has {len(caches)} of {body['total_count']} entries")
    return caches


def check_save(repository, candidates, budget=SAVE_BUDGET):
    """Decide per candidate (key, bytes), in order, whether saving it keeps the repo under budget.

    Saves only when the reads are complete (else unverified), the key's family holds at most
    one entry on any ref (else cleanup-pending) and the projected total stays within budget
    (else over-budget). Read-only: never deletes a cache.
    """
    try:
        caches = read_all_caches(repository)
        reported = http_api(f"repos/{repository}/actions/cache/usage")["active_caches_size_in_bytes"]
    except Exception as error:  # fail closed: any read problem skips every save
        print(f"cache budget unverified: {error}", file=sys.stderr)
        caches = None
    decisions = []
    allowed = 0
    for key, size in candidates:
        family, _ = cache_family(key)
        decision = {"key": key, "save": False, "reason": "unverified", "family": family,
                    "family_entries": None, "current_bytes": None, "projected_bytes": None}
        if caches is not None:
            current = max(reported, sum(entry["size_in_bytes"] for entry in caches))
            entries = sum(1 for entry in caches if cache_family(entry["key"]) == (family, True))
            projected = current + allowed + size
            decision.update(family_entries=entries, current_bytes=current, projected_bytes=projected)
            if entries > 1:
                decision["reason"] = "cleanup-pending"
            elif projected > budget:
                decision["reason"] = "over-budget"
            else:
                decision.update(save=True, reason="ok")
                allowed += size
        decisions.append(decision)
    return decisions


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", default=os.environ.get("GITHUB_REPOSITORY"))
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--check-save", action="store_true",
                        help="print one JSON decision per --candidate and exit; deletes nothing")
    parser.add_argument("--candidate", action="append", default=[], metavar="KEY=BYTES",
                        help="with --check-save: a key that missed and its du -sb bytes, in save order")
    arguments = parser.parse_args()
    if not arguments.repo or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", arguments.repo):
        parser.error("--repo must name owner/repository")
    repository = arguments.repo
    if arguments.check_save:
        candidates = []
        for item in arguments.candidate:
            key, _, size = item.rpartition("=")
            if not key or not size.isdigit():
                parser.error("--candidate must be KEY=BYTES")
            candidates.append((key, int(size)))
        for decision in check_save(repository, candidates):
            print(json.dumps(decision))
        return
    default_ref = "refs/heads/" + api(f"repos/{repository}")["default_branch"]
    caches = []
    page = 1
    while True:
        batch = api(f"repos/{repository}/actions/caches?per_page=100&page={page}")["actions_caches"]
        caches.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    deletions = plan_deletions(caches, default_ref)
    deleted_bytes = sum(cache["size_in_bytes"] for cache in caches if cache["id"] in deletions)
    print(
        json.dumps(
            {
                "repository": repository,
                "dry_run": arguments.dry_run,
                "before_bytes": sum(cache["size_in_bytes"] for cache in caches),
                "delete_ids": deletions,
                "delete_bytes": deleted_bytes,
                "projected_bytes": sum(cache["size_in_bytes"] for cache in caches) - deleted_bytes,
            }
        )
    )
    if not arguments.dry_run:
        for identifier in deletions:
            try:
                subprocess.run(
                    ["gh", "cache", "delete", str(identifier), "--repo", repository],
                    check=True,
                    capture_output=True,
                    text=True,
                )
            except subprocess.CalledProcessError as error:
                if not re.search(
                    r"\bHTTP 404\b|Could not find a cache matching\b", error.stderr or ""
                ):
                    raise
                print(f"Cache {identifier} is already absent; continuing.", file=sys.stderr)
        print(json.dumps(api(f"repos/{repository}/actions/cache/usage")))


if __name__ == "__main__":
    main()
