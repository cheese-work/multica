#!/usr/bin/env python3
"""Reclaim old immutable Multica images on C00 without a model (CHE-7139).

Scope is frozen: an image is a candidate only when every RepoTag is
ghcr.io/cheese-work/multica-{backend,web}:sha-<40 hex>, it is older than 7 full
days, and no container (running or stopped) and no keep reference names it.
Never force, never prune, never touch containers, volumes or config.

Keep references come from live state only: the active tuple in
cutover-state.json (the contract of cutover.sh write_cutover_state), every
container's image ID and configured ref (the retained predecessor colour is a
stopped container, see rollback-failed-readback.sh; both its services must be
present or the run is BLOCKED), and --keep-ref. Compose
labels are NOT a reference source: cd-deploy.yml deletes its staging directory
after every deploy, so they point at paths that are gone by design. The
manifest.json / release-record.json beside cutover-state.json are not part of the
controller contract, but any image ref in them must resolve to a local image or be
accepted by exact value with --accept-unresolved-ref (recorded in the receipt);
otherwise BLOCKED. Every keep ref must resolve to a local image
(by config ID, tag, or RepoDigest, which is how a registry manifest digest maps
to a local config ID) or the run is BLOCKED. The deploy lock in
--cutover-state-dir is held across discovery and removal; if it is busy, BLOCKED.

Exit: 0 target met, 1 a removal failed or an unexpected error, 2 BLOCKED (no
mutation), 3 TARGET_UNMET. A timer that runs this and exits 0 has met the target.
usage: c00-image-gc.py --cutover-state-dir DIR --deploy-root DIR --run-dir DIR
         [--target-bytes N] [--apply] [--keep-ref REF]...
"""
import argparse
import datetime
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import sys

TAG_RE = re.compile(r"^ghcr\.io/cheese-work/multica-(backend|web):sha-[0-9a-f]{40}$")
IMAGE_REF_RE = re.compile(r"ghcr\.io/cheese-work/multica-(?:backend|web)(?::[A-Za-z0-9._-]+)?(?:@sha256:[0-9a-f]{64})?")
MIN_AGE = 7 * 86400
OWNER = "deploy-controller owner (cutover.sh state) or the C00 operator for a stuck cd-deploy staging directory"


class Blocked(Exception):
    pass


def docker(*args, check=True):
    p = subprocess.run([os.environ.get("DOCKER", "docker"), *args], capture_output=True, text=True)
    if check and p.returncode != 0:
        raise RuntimeError(f"docker {' '.join(args[:2])} failed: {p.stderr.strip()}")
    return p.stdout


def inspect_images(ids):
    return json.loads(docker("image", "inspect", *ids) or "[]") if ids else []


def used_bytes(path):
    s = os.statvfs(path)
    return (s.f_blocks - s.f_bfree) * s.f_frsize


def age_seconds(created, now):
    # docker reports nanoseconds; fromisoformat takes at most microseconds
    t = re.sub(r"(\.\d{6})\d*", r"\1", created).replace("Z", "+00:00")
    return now - datetime.datetime.fromisoformat(t).timestamp()


def matches(ref, img):
    return ref == img["Id"] or ref in (img.get("RepoTags") or []) or ref in (img.get("RepoDigests") or [])


def read_cutover_state(state_dir):
    path = Path(state_dir) / "cutover-state.json"
    try:
        st = json.loads(path.read_text())
        refs = [st["image_tuple"]["backend"], st["image_tuple"]["web"]]
        colour = st["active_colour"]
    except (OSError, ValueError, KeyError, TypeError) as e:
        raise Blocked(f"cannot read the active image_tuple and colour from {path}: {e}")
    if not all(isinstance(r, str) and r for r in refs) or colour not in ("blue", "green"):
        raise Blocked(f"{path} has no two non-empty image refs and a blue/green active_colour")
    return refs, colour


def record_refs(state_dir):
    """{ref: file} from retained release records beside cutover-state.json.

    cutover.sh and rollback-failed-readback.sh never read these files (the retained
    predecessor is whatever Docker runs), so they are not authoritative. They are still
    unexamined evidence of a deployment reference: each ref must resolve locally or be
    accepted by exact value, else BLOCKED.
    """
    found = {}
    for name in ("manifest.json", "release-record.json"):
        path = Path(state_dir) / name
        if not path.exists():
            continue
        try:
            doc = json.loads(path.read_text())
        except (OSError, ValueError) as e:
            raise Blocked(f"retained record {path} is unreadable ({e}); its deployment references are unknown")
        stack = [doc]
        while stack:
            v = stack.pop()
            if isinstance(v, dict):
                stack.extend(v.values())
            elif isinstance(v, list):
                stack.extend(v)
            elif isinstance(v, str) and IMAGE_REF_RE.fullmatch(v):
                found.setdefault(v, name)
    return found


def freeze(args, now):
    """Return (candidates, skipped, accepted_unresolved_refs); raise Blocked when identity or deploy state is unknown."""
    root = Path(args.deploy_root)
    if not root.is_dir():
        raise Blocked(f"deploy root {root} is not readable; cannot prove no deploy is active")
    active = sorted(p.name for p in root.glob("cd-deploy-*"))
    if active:
        raise Blocked(f"deploy staging present ({', '.join(active)}); a deploy or rollback may be active, or cleanup after one failed")

    images = inspect_images(docker("image", "ls", "-aq", "--no-trunc").split())
    by_id = {i["Id"]: i for i in images}

    refs, colour = read_cutover_state(args.cutover_state_dir)
    keep, accepted = set(), []
    for ref in refs:
        hit = [i["Id"] for i in images if matches(ref, i)]
        if not hit:
            raise Blocked(f"active tuple ref {ref} resolves to no local image; the current deployment identity is unknown")
        keep.update(hit)
    for ref, name in record_refs(args.cutover_state_dir).items():
        hit = [i["Id"] for i in images if matches(ref, i)]
        if hit:
            keep.update(hit)
        elif ref in args.accept_unresolved_ref:
            accepted.append(ref)
        else:
            raise Blocked(f"{name} references {ref}, which resolves to no local image; pass --accept-unresolved-ref {ref} once its owner confirms it is not a local image")
    for ref in args.keep_ref:
        hit = [i["Id"] for i in images if matches(ref, i)]
        if not hit:
            raise Blocked(f"keep ref {ref} resolves to no local image; pass a ref that exists locally or drop it")
        keep.update(hit)

    cids = docker("ps", "-aq", "--no-trunc").split()
    containers = json.loads(docker("inspect", *cids)) if cids else []
    predecessor = "blue" if colour == "green" else "green"
    services = {(c["Config"].get("Labels") or {}).get("com.docker.compose.service") for c in containers}
    missing = [f"{s}-{predecessor}" for s in ("backend", "frontend") if f"{s}-{predecessor}" not in services]
    if missing:  # same contract as rollback-failed-readback.sh: no identifiable predecessor, no rollback target
        raise Blocked(f"retained predecessor colour {predecessor} is not identifiable (no container for {', '.join(missing)}); rollback images cannot be proven")
    for c in containers:
        cref = c["Config"]["Image"]
        keep.add(c["Image"])
        keep.update(i["Id"] for i in images if matches(cref, i))

    candidates, skipped = [], []
    for i in sorted(images, key=lambda i: i["Id"]):
        tags = i.get("RepoTags") or []
        if not tags or not all(TAG_RE.match(t) for t in tags):
            continue
        why = None
        if i["Id"] in keep:
            why = "referenced by the active tuple, a container or a keep ref"
        elif age_seconds(i["Created"], now) <= MIN_AGE:
            why = "younger than 7 full days"
        (skipped.append({"id": i["Id"], "tags": tags, "reason": why}) if why
         else candidates.append({"id": i["Id"], "tags": tags, "size": i.get("Size"), "created": i["Created"]}))
    return candidates, skipped, accepted


def remove_one(c):
    """Fresh exact-ID checks, then a plain `docker image rm <id>`. Returns an error string, or None."""
    fresh = inspect_images([c["id"]])
    if len(fresh) != 1 or sorted(fresh[0].get("RepoTags") or []) != sorted(c["tags"]):
        return "tags changed or image gone since freeze"
    if docker("ps", "-aq", "--no-trunc", "--filter", f"ancestor={c['id']}").split():
        return "a container references the image"
    p = subprocess.run([os.environ.get("DOCKER", "docker"), "image", "rm", c["id"]], capture_output=True, text=True)
    if p.returncode != 0:
        return f"docker image rm failed: {p.stderr.strip()}"
    if inspect_images_safe(c["id"]):
        return "image still present after rm"
    return None


def inspect_images_safe(image_id):
    p = subprocess.run([os.environ.get("DOCKER", "docker"), "image", "inspect", image_id], capture_output=True, text=True)
    return p.returncode == 0


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--cutover-state-dir", required=True)
    ap.add_argument("--deploy-root", required=True, help="directory holding cd-deploy-* staging (C00_STATE_DIR)")
    ap.add_argument("--run-dir", required=True, help="private parent for per-run manifest and receipt")
    ap.add_argument("--target-bytes", type=int, default=65_000_000_000)
    ap.add_argument("--df-path", default="/")
    ap.add_argument("--keep-ref", action="append", default=[])
    ap.add_argument("--accept-unresolved-ref", action="append", default=[], help="exact retained-record ref the owner confirmed is not a local image")
    ap.add_argument("--apply", action="store_true", help="remove; default is a dry run")
    ap.add_argument("--now", type=float, default=None)
    args = ap.parse_args(argv)

    now = args.now if args.now is not None else datetime.datetime.now(datetime.timezone.utc).timestamp()
    started = datetime.datetime.fromtimestamp(now, datetime.timezone(datetime.timedelta(hours=7)))
    run = Path(args.run_dir) / started.strftime("%Y%m%dT%H%M%S%z")
    run.mkdir(parents=True, mode=0o700, exist_ok=True)
    os.chmod(run.parent, 0o700)
    os.chmod(run, 0o700)

    receipt = {"started_at_ict": started.strftime("%d %b %Y %H:%M ICT"), "mode": "apply" if args.apply else "dry-run",
               "target_bytes": args.target_bytes, "used_before": used_bytes(args.df_path),
               "candidates": [], "skipped": [], "removed": [], "failed": []}
    code = 0
    try:
        # Same lock deploy.sh and cutover.sh (deploy and rollback) take; staging alone cannot
        # show a rollback. Held until exit, so it covers discovery and every removal.
        lock = open(Path(args.cutover_state_dir) / "deploy.lock", "a")
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError as e:
            raise Blocked(f"deploy lock {lock.name} is held or unavailable ({e}); a deploy or rollback may be running")
        receipt["candidates"], receipt["skipped"], receipt["accepted_unresolved_refs"] = freeze(args, now)
        (run / "manifest.json").write_text(json.dumps(receipt["candidates"], indent=1))
        if args.apply:
            for c in receipt["candidates"]:
                err = remove_one(c)
                if err is None:
                    receipt["removed"].append(c["id"])
                elif err.startswith(("docker image rm", "image still")):
                    receipt["failed"].append({"id": c["id"], "error": err})
                else:
                    receipt["skipped"].append({"id": c["id"], "tags": c["tags"], "reason": err})
        receipt["used_after"] = used_bytes(args.df_path)
        receipt["net_reclaimed_bytes"] = receipt["used_before"] - receipt["used_after"]
        over = receipt["used_after"] - args.target_bytes
        receipt["remaining_over_target_bytes"] = max(over, 0)
        if receipt["failed"]:
            receipt["status"], code = "FAILED", 1
        elif over > 0:
            receipt["status"], code = "TARGET_UNMET", 3
        else:
            receipt["status"] = "OK"
    except Blocked as e:
        receipt.update(status="BLOCKED", reason=str(e), recovery_owner=OWNER)
        code = 2
    except Exception as e:  # unknown state is never success
        receipt.update(status="FAILED", reason=f"{type(e).__name__}: {e}")
        code = 1
    (run / "receipt.json").write_text(json.dumps(receipt, indent=1))
    print(json.dumps({k: receipt.get(k) for k in ("status", "reason", "mode", "net_reclaimed_bytes", "remaining_over_target_bytes", "removed", "recovery_owner")}))
    return code


if __name__ == "__main__":
    sys.exit(main())
