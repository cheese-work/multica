#!/usr/bin/env python3
"""c00-image-gc.py reclaims old immutable Multica images without trusting ephemeral deploy staging (CHE-7139).

`docker` is a stub driven by a JSON state file; nothing here reaches C00 or a real daemon.
Reproduces the live failure: running and stopped colour containers carry Compose labels that
point into the cd-deploy staging directory cd-deploy.yml deletes after every deploy, so a
guard that resolves those labels skips every image forever.
"""
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
GC = ROOT / "deploy/cd/c00-image-gc.py"
NOW = 1_800_000_000  # fixed clock handed to the script via --now
DAY = 86400
BACKEND, WEB = "ghcr.io/cheese-work/multica-backend", "ghcr.io/cheese-work/multica-web"


def sha(c):
    return c * 40


def iid(c):
    return "sha256:" + c * 64


def created(age_days):
    t = NOW - int(age_days * DAY)
    return __import__("datetime").datetime.fromtimestamp(t, __import__("datetime").timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.123456789Z")


STUB = r'''#!/usr/bin/env python3
import json, os, sys
p = os.environ["STUB_STATE"]
s = json.load(open(p))
args = sys.argv[1:]
s.setdefault("calls", []).append(args)
out = ""
rc = 0
if args[:2] == ["image", "ls"]:
    out = "\n".join(i["Id"] for i in s["images"])
elif args[:2] == ["image", "inspect"]:
    n = s.setdefault("inspects", {})
    res = []
    for ref in args[2:]:
        img = next((i for i in s["images"] if i["Id"] == ref), None)
        if img is None:
            rc = 1
            continue
        n[ref] = n.get(ref, 0) + 1
        img = dict(img)
        if n[ref] > 1 and ref in s.get("late_tags", {}):
            img["RepoTags"] = s["late_tags"][ref]
        res.append(img)
    out = json.dumps(res)
elif args[:2] == ["ps", "-aq"] or args[:1] == ["ps"]:
    flt = [a for a in args if a.startswith("ancestor=")]
    if flt:
        i = flt[0].split("=", 1)[1]
        hit = i in s.get("late_ancestor", []) or any(c["Image"] == i for c in s["containers"])
        out = "cid-late" if hit else ""
    else:
        out = "\n".join(c["Id"] for c in s["containers"])
elif args[:1] == ["inspect"]:
    out = json.dumps([c for c in s["containers"] if c["Id"] in args[1:]])
elif args[:2] == ["image", "rm"]:
    i = args[2]
    if i in s.get("rm_fail", []):
        sys.stderr.write("conflict: image is referenced\n")
        rc = 1
    else:
        s["images"] = [x for x in s["images"] if x["Id"] != i]
else:
    sys.stderr.write("unexpected docker call %r\n" % args)
    rc = 99
json.dump(s, open(p, "w"))
print(out)
sys.exit(rc)
'''


def image(c, age, tags=None, digests=None):
    tags = [f"{BACKEND}:sha-{sha(c)}"] if tags is None else tags
    return {"Id": iid(c), "RepoTags": tags, "RepoDigests": digests or [], "Created": created(age)}


def container(cid, c, ref, running, staging):
    return {
        "Id": cid, "Image": iid(c), "Config": {"Image": ref, "Labels": {
            "com.docker.compose.project.config_files": f"{staging}/docker-compose.selfhost.yml",
            "com.docker.compose.project.working_dir": str(staging)}},
        "State": {"Running": running},
    }


class GcTests(unittest.TestCase):
    def setUp(self):
        t = tempfile.TemporaryDirectory()
        self.addCleanup(t.cleanup)
        self.d = Path(t.name)
        self.staging = self.d / "cd-deploy-1789999999"  # deliberately never created: deploy removed it
        self.state_dir = self.d / "cutover"
        self.state_dir.mkdir()
        self.deploy_root = self.d / "deploy-root"
        self.deploy_root.mkdir()
        self.stub = self.d / "docker"
        self.stub.write_text(STUB)
        self.stub.chmod(0o755)
        self.stub_state = self.d / "stub.json"
        ref = f"{BACKEND}:sha-{sha('c')}"  # one image stands for the active tuple
        self.write_cutover({"backend": ref, "web": ref})

    def write_cutover(self, tuple_):
        (self.state_dir / "cutover-state.json").write_text(json.dumps({"active_colour": "blue", "image_tuple": tuple_}))

    def world(self, images, containers=(), **extra):
        self.stub_state.write_text(json.dumps({"images": images, "containers": list(containers), **extra}))

    def run_gc(self, *extra, apply=True, target=1):
        run_dir = self.d / "run"
        cmd = [sys.executable, str(GC), "--cutover-state-dir", str(self.state_dir), "--deploy-root", str(self.deploy_root),
               "--run-dir", str(run_dir), "--target-bytes", str(target), "--now", str(NOW), "--df-path", str(self.d)]
        if apply:
            cmd.append("--apply")
        p = subprocess.run(cmd + list(extra), capture_output=True, text=True,
                           env={**os.environ, "DOCKER": str(self.stub), "STUB_STATE": str(self.stub_state)})
        receipts = sorted(run_dir.glob("*/receipt.json")) if run_dir.exists() else []
        return p, (json.loads(receipts[-1].read_text()) if receipts else None)

    def ids(self):
        return {i["Id"] for i in json.loads(self.stub_state.read_text())["images"]}

    def calls(self):
        return json.loads(self.stub_state.read_text()).get("calls", [])

    def removed_calls(self):
        return [c for c in self.calls() if c[:2] == ["image", "rm"]]

    def test_ephemeral_staging_labels_do_not_protect_an_unreferenced_old_image(self):
        self.world(
            [image("a", 10), image("c", 1)],
            [container("k1", "c", f"{BACKEND}:sha-{sha('c')}", True, self.staging)],
        )
        self.assertFalse(self.staging.exists())
        p, r = self.run_gc()
        self.assertEqual(self.ids(), {iid("c")}, p.stderr)
        self.assertEqual(r["removed"], [iid("a")])

    def test_keeps_current_running_stopped_predecessor_and_explicit_refs(self):
        self.world(
            [image("a", 10), image("b", 10), image("c", 10), image("d", 10), image("e", 10)],
            [
                container("run", "c", f"{BACKEND}:sha-{sha('c')}", True, self.staging),
                # stopped predecessor created from a ref whose tag now points at another image id
                container("stop", "x", f"{BACKEND}:sha-{sha('b')}", False, self.staging),
                container("id", "d", f"{BACKEND}:sha-{sha('d')}", False, self.staging),
            ],
        )
        p, r = self.run_gc("--keep-ref", f"{BACKEND}:sha-{sha('e')}")
        self.assertEqual(self.ids(), {iid(x) for x in "bcde"}, p.stderr)
        self.assertEqual(r["removed"], [iid("a")])
        self.assertEqual({s["id"] for s in r["skipped"]}, {iid(x) for x in "bcde"})

    def test_age_tags_and_repo_scope(self):
        self.world([
            image("a", 6.9),  # younger than 7 full days
            image("b", 10, [f"{BACKEND}:sha-{sha('b')}", f"{BACKEND}:rollback"]),  # extra tag
            image("d", 10, [f"{BACKEND}:latest"]),  # not immutable sha tag
            image("e", 10, [f"ghcr.io/other/app:sha-{sha('e')}"]),  # other repo
            image("f", 10, []),  # untagged: identity unknown
            image("1", 10, [f"{WEB}:sha-{sha('1')}"]),  # web is in scope
            image("c", 1),
        ])
        p, r = self.run_gc()
        self.assertEqual(self.ids(), {iid(x) for x in "abdefc"}, p.stderr)
        self.assertEqual(r["removed"], [iid("1")])

    def test_registry_digest_current_tuple_resolves_through_repo_digests(self):
        digest = f"{BACKEND}@sha256:{'9' * 64}"
        self.write_cutover({"backend": digest, "web": digest})
        self.world([image("a", 10), image("c", 10, digests=[digest])])
        p, r = self.run_gc()
        self.assertEqual(self.ids(), {iid("c")}, p.stderr)

    def test_unresolvable_current_tuple_blocks_every_removal(self):
        self.write_cutover({"backend": f"{BACKEND}@sha256:{'9' * 64}", "web": f"{BACKEND}:sha-{sha('c')}"})
        self.world([image("a", 10), image("c", 1)])
        p, r = self.run_gc()
        self.assertEqual(p.returncode, 2, p.stderr)
        self.assertEqual(r["status"], "BLOCKED")
        self.assertIn("recovery_owner", r)
        self.assertEqual(self.removed_calls(), [])

    def test_missing_or_malformed_cutover_state_blocks(self):
        self.world([image("a", 10)])
        for body in [None, "{", json.dumps({"active_colour": "blue"})]:
            path = self.state_dir / "cutover-state.json"
            path.unlink(missing_ok=True)
            if body is not None:
                path.write_text(body)
            p, r = self.run_gc()
            self.assertEqual((p.returncode, r["status"]), (2, "BLOCKED"), body)
        self.assertEqual(self.removed_calls(), [])

    def test_active_deploy_staging_blocks(self):
        (self.deploy_root / "cd-deploy-1799999999").mkdir()
        self.world([image("a", 10), image("c", 1)])
        p, r = self.run_gc()
        self.assertEqual((p.returncode, r["status"]), (2, "BLOCKED"))
        self.assertEqual(self.removed_calls(), [])

    def test_container_created_after_freeze_is_caught_by_the_exact_id_recheck(self):
        self.world([image("a", 10), image("c", 1)], late_ancestor=[iid("a")])
        p, r = self.run_gc()
        self.assertEqual(self.ids(), {iid("a"), iid("c")}, p.stderr)
        self.assertEqual(r["removed"], [])
        self.assertTrue(any(s["id"] == iid("a") for s in r["skipped"]))

    def test_tags_changed_after_freeze_skip_the_image(self):
        self.world([image("a", 10), image("c", 1)], late_tags={iid("a"): [f"{BACKEND}:sha-{sha('a')}", f"{BACKEND}:keep"]})
        p, r = self.run_gc()
        self.assertEqual(self.ids(), {iid("a"), iid("c")}, p.stderr)

    def test_never_forces_or_prunes_and_removes_by_full_id(self):
        self.world([image("a", 10), image("c", 1)])
        self.run_gc()
        for c in self.calls():
            self.assertNotIn("prune", c)
            self.assertNotIn("-f", c)
            self.assertNotIn("--force", c)
        self.assertEqual(self.removed_calls(), [["image", "rm", iid("a")]])

    def test_dry_run_freezes_a_manifest_and_removes_nothing(self):
        self.world([image("a", 10), image("c", 1)])
        p, r = self.run_gc(apply=False)
        self.assertEqual(self.removed_calls(), [])
        self.assertEqual(r["mode"], "dry-run")
        self.assertEqual([c["id"] for c in r["candidates"]], [iid("a")])
        run = next((self.d / "run").iterdir())
        self.assertEqual(stat.S_IMODE(run.stat().st_mode), 0o700)
        self.assertTrue((run / "manifest.json").exists())

    def test_rm_failure_is_reported_and_exits_nonzero(self):
        self.world([image("a", 10), image("c", 1)], rm_fail=[iid("a")])
        p, r = self.run_gc()
        self.assertEqual((p.returncode, r["status"]), (1, "FAILED"))
        self.assertEqual([f["id"] for f in r["failed"]], [iid("a")])

    def test_target_unmet_is_never_exit_zero_but_target_met_is(self):
        self.world([image("a", 10), image("c", 1)])
        p, r = self.run_gc(target=1)
        self.assertEqual((p.returncode, r["status"]), (3, "TARGET_UNMET"))
        self.assertIn("remaining_over_target_bytes", r)
        self.world([image("c", 1)])
        p, r = self.run_gc(target=10**18)
        self.assertEqual((p.returncode, r["status"]), (0, "OK"))

    def test_nothing_removed_is_target_unmet_not_success(self):
        self.world([image("c", 1)])
        p, r = self.run_gc(target=1)
        self.assertEqual(p.returncode, 3)
        self.assertEqual(r["removed"], [])


if __name__ == "__main__":
    unittest.main()
