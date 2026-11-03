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
        nodes {
          state
          enqueuedAt
          pullRequest { number url }
          headCommit {
            oid
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
    queue = response["data"]["repository"]["mergeQueue"]
    stalls = []
    for entry in (queue["entries"]["nodes"] if queue else []):
        age = now - dt.datetime.fromisoformat(entry["enqueuedAt"].replace("Z", "+00:00"))
        if age < dt.timedelta(minutes=max_age_minutes):
            continue
        where = entry["pullRequest"]["url"]
        if entry["state"] == "AWAITING_CHECKS":
            stalls.append(f"{where} is AWAITING_CHECKS since {entry['enqueuedAt']}")
        head = entry["headCommit"]
        contexts = ((head or {}).get("statusCheckRollup") or {}).get("contexts") or {"totalCount": 0, "nodes": []}
        if contexts["totalCount"] > len(contexts["nodes"]):
            # Without the full list a check cannot be called missing; the age rule above still applies.
            print(f"::warning::{where}: {contexts['totalCount']} checks on {head['oid']}, only {len(contexts['nodes'])} read; skipped missing-check test")
            continue
        reported = {node.get("name") or node.get("context") for node in contexts["nodes"]}
        for name in required:
            if name not in reported:
                stalls.append(f"{where}: required check {name} missing on merge-group head {head['oid'] if head else '(none)'} since {entry['enqueuedAt']}")
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
