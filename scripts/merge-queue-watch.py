#!/usr/bin/env python3
"""Read-only merge-queue stall alarm, independent of any runner or queue state."""
import datetime as dt
import json
import os
import subprocess
import sys

QUERY = """
query($owner: String!, $name: String!, $branch: String!) {
  repository(owner: $owner, name: $name) {
    mergeQueue(branch: $branch) {
      entries(first: 100) {
        totalCount
        nodes {
          state
          enqueuedAt
          pullRequest { number url }
          headCommit {
            oid
            committedDate
            statusCheckRollup {
              contexts(first: 100) {
                totalCount
                nodes {
                  __typename
                  ... on CheckRun { name }
                  ... on StatusContext { context }
                }
              }
            }
          }
        }
      }
    }
  }
}
"""


def now():
    return dt.datetime.now(dt.timezone.utc)


def find_stalls(response, required, now, max_age_minutes=30):
    """Clock = the merge-group head commit date: enqueuedAt also counts time spent
    queued behind other entries, before this entry had a head or started checks."""
    queue = response["data"]["repository"]["mergeQueue"]
    if not queue:
        return []
    entries = queue["entries"]
    stalls = []
    if entries["totalCount"] > len(entries["nodes"]):
        stalls.append(f"inconclusive: {entries['totalCount']} queue entries, only {len(entries['nodes'])} read")
    for entry in entries["nodes"]:
        head = entry["headCommit"]
        if not head:
            # No head, so no state clock. AWAITING_CHECKS cannot have lasted longer than the entry
            # has been queued: younger than the limit is healthy, older cannot be judged.
            enqueued = dt.datetime.fromisoformat(entry["enqueuedAt"].replace("Z", "+00:00"))
            if entry["state"] == "AWAITING_CHECKS" and now - enqueued >= dt.timedelta(minutes=max_age_minutes):
                stalls.append(f"inconclusive: {entry['pullRequest']['url']} is AWAITING_CHECKS without a merge-group head, queued since {entry['enqueuedAt']}")
            continue
        since = head["committedDate"]
        if now - dt.datetime.fromisoformat(since.replace("Z", "+00:00")) < dt.timedelta(minutes=max_age_minutes):
            continue
        where = entry["pullRequest"]["url"]
        if entry["state"] == "AWAITING_CHECKS":
            stalls.append(f"{where} is AWAITING_CHECKS on merge-group head {head['oid']} since {since}")
        contexts = (head.get("statusCheckRollup") or {}).get("contexts") or {"totalCount": 0, "nodes": []}
        if contexts["totalCount"] > len(contexts["nodes"]):
            stalls.append(f"inconclusive: {where} has {contexts['totalCount']} checks on {head['oid']}, only {len(contexts['nodes'])} read")
            continue
        reported = {node.get("name") or node.get("context") for node in contexts["nodes"]}
        for name in required:
            if name not in reported:
                stalls.append(f"{where}: required check {name} missing on merge-group head {head['oid']} since {since}")
    return stalls


def required_checks(repo, branch):
    rules = json.loads(subprocess.check_output(["gh", "api", f"repos/{repo}/rules/branches/{branch}"], text=True))
    return sorted({check["context"] for rule in rules if rule["type"] == "required_status_checks"
                   for check in rule["parameters"]["required_status_checks"]})


def main():
    repo = os.environ["GITHUB_REPOSITORY"]
    branch = os.environ.get("MERGE_QUEUE_BRANCH", "main")
    owner, name = repo.split("/")
    response = json.loads(subprocess.check_output([
        "gh", "api", "graphql", "-f", f"query={QUERY}",
        "-f", f"owner={owner}", "-f", f"name={name}", "-f", f"branch={branch}",
    ], text=True))
    stalls = find_stalls(response, required_checks(repo, branch), now())
    for stall in stalls:
        print(f"::error::Merge queue stalled: {stall}")
    if stalls:
        print("Inspect the queue entry and the check that never reported on the merge_group head; "
              "the multica security-gate stall (CHE-3240) is the precedent.")
        return 1
    print("No merge-queue entry waiting or missing a required check for 30 minutes.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
