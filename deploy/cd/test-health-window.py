#!/usr/bin/env python3
"""health-window.mjs: a native C00 timer samples health into a durable per-deploy journal; verify fails closed (CHE-5175)."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy/cd/health-window.mjs"
SHA = "a" * 40
T0 = 1_700_000_000
WINDOW, INTERVAL = 120, 10  # short window keeps the suite fast; the 900 s window has its own test
LAST = WINDOW // INTERVAL  # final scheduled slot: T0 + WINDOW


class Base(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dir = Path(self.temp.name)
        self.obs = self.dir / "obs"
        for sub in ["bin", "state", "compose", "router"]:
            (self.dir / sub).mkdir()
        (self.dir / "state/cutover-state.json").write_text(json.dumps({"active_colour": "blue"}))
        self.set_containers("c1\nc2\n")
        self.set_health(ok=True)
        stub = self.dir / "bin/docker"
        stub.write_text(f'#!/usr/bin/env bash\ncat "{self.dir}/containers"\n')
        stub.chmod(0o755)

    def set_containers(self, text):
        (self.dir / "containers").write_text(text)

    def set_health(self, ok=True, text=""):
        script = self.dir / "ab-health-check.sh"
        script.write_text("#!/usr/bin/env bash\n" + ("" if ok else f'echo "::error::{text or "route unhealthy"}"\nexit 1\n'))
        script.chmod(0o755)

    def env(self, now=None, **extra):
        env = {**os.environ, "PATH": f"{self.dir}/bin:{os.environ['PATH']}"}
        if now is not None:
            env["HW_NOW"] = str(now)
        env.update(extra)
        return env

    def run_node(self, *args, now=None, **extra):
        return subprocess.run(["node", str(SCRIPT), *map(str, args)], cwd=self.dir, env=self.env(now, **extra), text=True, capture_output=True)

    def arm_args(self, run_id="r1-1", verify=0, window=WINDOW, interval=INTERVAL, sha=SHA):
        return ["arm", "--dir", self.obs, "--id", run_id, "--sha", sha, "--window", window, "--interval", interval,
                "--verify-seconds", verify, "--compose-dir", self.dir / "compose", "--cutover-state-dir", self.dir / "state",
                "--router-state-dir", self.dir / "router", "--health-script", self.dir / "ab-health-check.sh"]

    def seed(self, run_id="r1-1", t0=T0, window=WINDOW, interval=INTERVAL, sha=SHA):
        """Write the observation arm would write, without running the clock."""
        od = self.obs / run_id
        od.mkdir(parents=True)
        shutil.copy(self.dir / "ab-health-check.sh", od / "ab-health-check.sh")
        meta = {"id": run_id, "sha": sha, "t0": t0, "window": window, "interval": interval, "colour": "blue", "containers": ["c1", "c2"],
                "compose_dir": str(self.dir / "compose"), "cutover_state_dir": str(self.dir / "state"), "router_state_dir": str(self.dir / "router")}
        (od / "meta.json").write_text(json.dumps(meta))
        (od / "samples.jsonl").write_text("")
        (self.obs / "active").write_text(run_id)
        return od

    def seed_copy_health(self):
        shutil.copy(self.dir / "ab-health-check.sh", self.obs / "r1-1/ab-health-check.sh")

    def tick(self, now):
        return self.run_node("tick", "--dir", self.obs, "--source", "timer", now=now, INVOCATION_ID="inv-1")

    def run_window(self, until_slot=LAST, skip=(), run_id="r1-1"):
        for slot in range(until_slot + 1):
            if slot not in skip:
                result = self.tick(T0 + slot * INTERVAL + 3)
                self.assertEqual(result.returncode, 0, result.stderr)

    def verify(self, now=T0 + WINDOW + 5, run_id="r1-1", sha=SHA):
        return self.run_node("verify", "--dir", self.obs, "--id", run_id, "--sha", sha, now=now)

    def samples(self, run_id="r1-1"):
        return [json.loads(line) for line in (self.obs / run_id / "samples.jsonl").read_text().splitlines()]


class TickTests(Base):
    def test_a_tick_records_one_sample_per_slot_with_its_schedule_and_identity(self):
        self.seed()
        self.assertEqual(self.tick(T0 + 3).returncode, 0)
        self.assertEqual(self.tick(T0 + 8).returncode, 0)  # same slot: no second sample
        self.assertEqual(self.tick(T0 + 13).returncode, 0)
        rows = self.samples()
        self.assertEqual([r["slot"] for r in rows], [0, 1])
        self.assertEqual(rows[1]["scheduled"], T0 + 10)
        self.assertEqual((rows[1]["start"], rows[1]["ok"], rows[1]["source"]), (T0 + 13, True, "timer"))
        self.assertEqual((rows[1]["colour"], rows[1]["containers"]), ("blue", ["c1", "c2"]))

    def test_a_failed_health_check_is_recorded_with_its_output(self):
        self.seed()
        self.set_health(ok=False, text="/health returned 500")
        self.seed_copy_health()
        self.assertEqual(self.tick(T0 + 3).returncode, 0)
        row = self.samples()[0]
        self.assertFalse(row["ok"])
        self.assertIn("/health returned 500", row["detail"])

    def test_a_tick_with_no_armed_observation_does_nothing(self):
        self.obs.mkdir()
        self.assertEqual(self.tick(T0).returncode, 0)

    def test_no_sample_is_taken_after_the_final_slot_and_the_window_is_marked_complete(self):
        self.seed()
        self.assertEqual(self.tick(T0 + WINDOW + 2).returncode, 0)
        self.assertEqual(self.samples()[0]["slot"], LAST)
        self.assertEqual(self.tick(T0 + WINDOW + 20).returncode, 0)
        self.assertEqual(len(self.samples()), 1)
        self.assertEqual(json.loads((self.obs / "r1-1/state.json").read_text())["state"], "complete")

    def test_a_changed_container_identity_ends_the_journal_as_replaced(self):
        self.seed()
        self.tick(T0 + 3)
        self.set_containers("c3\nc4\n")
        self.tick(T0 + 13)
        self.assertEqual(self.samples()[1]["kind"], "identity")
        self.assertEqual(json.loads((self.obs / "r1-1/state.json").read_text())["state"], "replaced")
        self.tick(T0 + 23)
        self.assertEqual(len(self.samples()), 2)  # a replaced journal is never extended


class VerifyTests(Base):
    def test_a_complete_healthy_window_passes(self):
        self.seed()
        self.run_window()
        result = self.verify()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"{LAST + 1} samples", result.stdout)

    def test_a_slot_sampled_by_hand_instead_of_the_native_timer_fails(self):
        # Only the systemd timer proves the observer ran on C00's own schedule; a sample taken
        # by a person or a CI step (no INVOCATION_ID) must not stand for a slot.
        self.seed()
        for slot in range(LAST + 1):
            if slot == 3:
                self.run_node("tick", "--dir", self.obs, "--source", "timer", now=T0 + slot * INTERVAL + 3, INVOCATION_ID="")
            else:
                self.tick(T0 + slot * INTERVAL + 3)
        result = self.verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("slot 3", result.stderr)
        self.assertIn("timer", result.stderr)

    def test_the_full_900_second_window_needs_all_91_slots_including_the_final_one(self):
        self.seed(window=900)
        for slot in range(91):
            self.assertEqual(self.tick(T0 + slot * INTERVAL + 3).returncode, 0)
        self.assertEqual(self.verify(now=T0 + 905).returncode, 0)
        path = self.obs / "r1-1/samples.jsonl"
        lines = path.read_text().splitlines(keepends=True)
        path.write_text("".join(lines[:-1]))  # the final scheduled sample at T0+900 is missing
        result = self.verify(now=T0 + 905)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("slot 90", result.stderr)

    def test_an_outage_at_minutes_5_to_10_that_recovered_fails_after_recovery(self):
        # Slots 4..7 stand for minutes 5 to 10 of the real window. The router saw no requests, so only active sampling sees this.
        self.seed()
        for slot in range(LAST + 1):
            self.set_health(ok=not 4 <= slot <= 7, text="backend /readyz returned 503")
            self.seed_copy_health()
            self.assertEqual(self.tick(T0 + slot * INTERVAL + 3).returncode, 0)
        result = self.verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("slot 4", result.stderr)

    def test_a_single_failed_sample_fails_the_window_for_each_probe(self):
        for text in ["/health returned 500", "backend-blue /readyz returned 503", "public web returned 502"]:
            with self.subTest(text=text):
                self.obs = self.dir / f"obs-{abs(hash(text))}"
                self.seed()
                for slot in range(LAST + 1):
                    self.set_health(ok=slot != 6, text=text)
                    self.seed_copy_health()
                    self.tick(T0 + slot * INTERVAL + 3)
                result = self.verify()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(text, result.stderr)

    def test_a_missed_scheduled_sample_fails(self):
        self.seed()
        self.run_window(skip={5})
        result = self.verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("slot 5", result.stderr)
        self.assertIn("missing", result.stderr)

    def test_a_missing_final_sample_fails(self):
        self.seed()
        self.run_window(until_slot=LAST - 1)
        self.assertNotEqual(self.verify().returncode, 0)

    def test_an_observer_that_died_before_the_end_fails(self):
        self.seed()
        self.run_window(until_slot=6)
        result = self.verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing", result.stderr)

    def test_a_truncated_journal_fails(self):
        self.seed()
        self.run_window()
        path = self.obs / "r1-1/samples.jsonl"
        path.write_text(path.read_text()[:-40])
        result = self.verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("truncated", result.stderr)

    def test_a_corrupt_line_fails(self):
        self.seed()
        self.run_window()
        path = self.obs / "r1-1/samples.jsonl"
        path.write_text("{not json}\n" + path.read_text())
        self.assertNotEqual(self.verify().returncode, 0)

    def test_a_missing_or_unreadable_journal_fails(self):
        self.seed()
        self.run_window()
        (self.obs / "r1-1/samples.jsonl").unlink()
        self.assertNotEqual(self.verify().returncode, 0)
        self.assertNotEqual(self.verify(run_id="never-armed").returncode, 0)

    def test_a_journal_armed_for_another_commit_fails(self):
        self.seed()
        self.run_window()
        result = self.verify(sha="b" * 40)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("commit", result.stderr)

    def test_a_window_that_has_not_ended_fails(self):
        self.seed()
        self.run_window()
        self.assertNotEqual(self.verify(now=T0 + WINDOW - 5).returncode, 0)

    def test_an_inconsistent_sample_slot_fails(self):
        self.seed()
        self.run_window()
        path = self.obs / "r1-1/samples.jsonl"
        rows = [json.loads(line) for line in path.read_text().splitlines()]
        rows[5]["start"] += 100
        path.write_text("".join(json.dumps(r) + "\n" for r in rows))
        self.assertNotEqual(self.verify().returncode, 0)

    def test_replaced_containers_fail_the_journal(self):
        self.seed()
        self.run_window(until_slot=6)
        self.set_containers("c3\nc4\n")
        self.tick(T0 + 6 * INTERVAL + 13)  # slot 7: identity changed -> replaced
        result = self.verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("replaced", result.stderr)


class ArmTests(Base):
    def test_arm_records_t0_samples_slot_0_and_needs_a_sample_from_the_native_timer(self):
        stop = threading.Event()

        def timer():  # stands in for multica-health-observer.timer
            while not stop.is_set():
                time.sleep(0.4)
                self.run_node("tick", "--dir", self.obs, "--source", "timer", INVOCATION_ID="inv-1")

        thread = threading.Thread(target=timer, daemon=True)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(stop.set)
        self.obs.mkdir()
        output = self.dir / "gh-output"
        result = self.run_node(*self.arm_args(verify=15, window=4, interval=1), GITHUB_OUTPUT=output)
        self.assertEqual(result.returncode, 0, result.stderr)
        text = output.read_text()
        self.assertIn("observation=r1-1", text)
        t0 = int(next(line for line in text.splitlines() if line.startswith("t0=")).split("=")[1])
        meta = json.loads((self.obs / "r1-1/meta.json").read_text())
        self.assertEqual((meta["t0"], meta["sha"], meta["containers"], meta["colour"]), (t0, SHA, ["c1", "c2"], "blue"))
        rows = self.samples()
        self.assertEqual((rows[0]["slot"], rows[0]["source"]), (0, "arm"))
        self.assertIn("timer", {r["source"] for r in rows})
        self.assertEqual((self.obs / "active").read_text().strip(), "r1-1")
        self.assertTrue((self.obs / "r1-1/ab-health-check.sh").exists())
        self.assertTrue((self.obs / "bin/health-window.mjs").exists())

    def test_arm_fails_without_a_sample_from_the_native_timer(self):
        self.obs.mkdir()
        result = self.run_node(*self.arm_args(verify=1, window=4, interval=1))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("timer", result.stderr)

    def test_a_timer_flag_without_a_systemd_invocation_is_not_native_timer_proof(self):
        self.seed()
        self.run_node("tick", "--dir", self.obs, "--source", "timer", now=T0 + 3, INVOCATION_ID="")
        self.assertEqual(self.samples()[0]["source"], "manual")

    def test_arm_fails_when_the_first_sample_is_unhealthy(self):
        self.set_health(ok=False)
        self.obs.mkdir()
        self.assertNotEqual(self.run_node(*self.arm_args(verify=0)).returncode, 0)

    def test_arm_fails_without_two_active_containers(self):
        self.set_containers("c1\n")
        self.obs.mkdir()
        self.assertNotEqual(self.run_node(*self.arm_args(verify=0)).returncode, 0)

    def test_arming_a_newer_deploy_supersedes_the_open_journal_and_never_mixes_samples(self):
        self.seed("old-1")
        self.run_window(until_slot=3, run_id="old-1")
        result = self.run_node(*self.arm_args(run_id="new-1", verify=0))
        self.assertNotEqual(result.returncode, 0)  # no timer in this test: arm itself fails after arming
        self.assertEqual(json.loads((self.obs / "old-1/state.json").read_text())["state"], "superseded")
        self.assertEqual((self.obs / "active").read_text().strip(), "new-1")
        self.assertEqual({r["slot"] for r in self.samples("old-1")}, {0, 1, 2, 3})

    def test_bad_arguments_are_refused(self):
        self.obs.mkdir()
        for args in [self.arm_args(run_id="../x"), self.arm_args(sha="abc"), self.arm_args(window=95, interval=10), ["nope"]]:
            with self.subTest(args=args):
                self.assertEqual(self.run_node(*args).returncode, 2)


if __name__ == "__main__":
    unittest.main(verbosity=2)
