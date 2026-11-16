import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location(
    "qualification", Path(__file__).with_name("namespace-qualification.py"))
qualification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(qualification)

LIVE = "6e65e11cfa2c3ee58ecf7ed2e491498d523dfa47"
OLD = "648fd398967d3366e9c52c29f19e1c0abd74836d"


def event(action, package=qualification.PACKAGE):
    return {"Action": action, "Test": qualification.TEST, "Package": package}


class QualificationTest(unittest.TestCase):
    def test_accepts_explicit_pass_for_requested_live_commit(self):
        qualification.verify(LIVE, LIVE, [event("pass")])

    def test_rejects_different_checkout_even_with_pass(self):
        with self.assertRaises(ValueError):
            qualification.verify(LIVE, OLD, [event("pass")])

    def test_rejects_mutable_or_malformed_source(self):
        for source in ("main", "6e65e11c", "", "a" * 39, "a" * 41, "x" * 40,
                       "$(touch /tmp/never)"):
            with self.subTest(source=source), self.assertRaises(ValueError):
                qualification.verify(source, source, [event("pass")])

    def test_rejects_incomplete_or_conflicting_receipts(self):
        for events in ([], [event("skip")], [event("fail")],
                       [event("pass"), event("skip")], [event("pass"), event("pass")],
                       [event("pass", "another/package")]):
            with self.subTest(events=events), self.assertRaises(ValueError):
                qualification.verify(OLD, OLD, events)


if __name__ == "__main__":
    unittest.main()
