"""Run the development Python unit tests and emit FORGE_RESULTS for Kodama."""
import json
import os
from pathlib import Path
import sys
import unittest

ROOT = Path(__file__).resolve().parents[2]
START_DIR = ROOT / "scripts/dev"


class Recorder(unittest.TextTestResult):
    """Tracks the outcome of every test, keyed by method and by class."""

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.outcomes = {}

    def _record(self, test, outcome):
        self.outcomes[test.id()] = outcome

    def addSuccess(self, test):
        super().addSuccess(test)
        self._record(test, "passed")

    def addFailure(self, test, err):
        super().addFailure(test, err)
        self._record(test, "failed")

    def addError(self, test, err):
        super().addError(test, err)
        self._record(test, "failed")

    def addSkip(self, test, reason):
        super().addSkip(test, reason)
        self._record(test, "skipped")

    def addExpectedFailure(self, test, err):
        super().addExpectedFailure(test, err)
        self._record(test, "skipped")

    def addUnexpectedSuccess(self, test):
        super().addUnexpectedSuccess(test)
        self._record(test, "failed")


def collect(outcomes):
    """Group outcomes by method and class; a class passes only if all its tests do."""
    groups = {}
    for test_id, outcome in outcomes.items():
        parts = test_id.split(".")
        if len(parts) < 2:
            names = [test_id]
        else:
            names = [parts[-1], parts[-2], ".".join(parts[-2:])]
        for name in names:
            groups.setdefault(name, set()).add(outcome)
    passed, failed, skipped = set(), set(), set()
    for name, kinds in groups.items():
        if "failed" in kinds:
            failed.add(name)
        elif "skipped" in kinds:
            skipped.add(name)
        else:
            passed.add(name)
    return sorted(passed - failed - skipped), sorted(failed), sorted(skipped)


def main() -> int:
    sys.path.insert(0, str(START_DIR))
    suite = unittest.defaultTestLoader.discover(str(START_DIR), pattern="test_*.py")
    runner = unittest.TextTestRunner(resultclass=Recorder, verbosity=1)
    result = runner.run(suite)
    results_path = os.environ.get("FORGE_RESULTS")
    if results_path:
        passed, failed, skipped = collect(result.outcomes)
        Path(results_path).parent.mkdir(parents=True, exist_ok=True)
        Path(results_path).write_text(
            json.dumps({"passed": passed, "failed": failed, "skipped": skipped}, indent=2) + "\n"
        )
    return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
    sys.exit(main())
