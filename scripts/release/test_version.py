import sys
from pathlib import Path
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))

from version import plan


class PlanTests(unittest.TestCase):
    def test_baseline_is_published_when_no_semver_tag_exists(self):
        self.assertEqual(plan("0.0.1", [], []), ("0.0.1", True))

    def test_baseline_ignores_non_semver_tags(self):
        self.assertEqual(plan("0.0.1\n", ["latest", "v0.0.1-rc.1"], []), ("0.0.1", True))

    def test_untagged_commit_increments_patch_only(self):
        self.assertEqual(plan("0.0.1", ["0.0.1", "0.0.2", "v0.1.0"], []), ("0.1.1", True))

    def test_existing_head_tag_is_reused_without_a_new_tag(self):
        self.assertEqual(plan("0.0.1", ["0.0.1", "0.0.2"], ["0.0.2"]), ("0.0.2", False))

    def test_v_prefix_on_head_counts_as_the_same_version(self):
        self.assertEqual(plan("0.0.1", ["v0.0.1"], ["v0.0.1"]), ("0.0.1", False))

    def test_highest_head_tag_wins_when_several_point_here(self):
        self.assertEqual(plan("0.0.1", ["0.0.1", "0.0.3"], ["0.0.1", "0.0.3"]), ("0.0.3", False))

    def test_invalid_baseline_is_rejected(self):
        with self.assertRaises(ValueError):
            plan("0.0.1-rc.1", [], [])


if __name__ == "__main__":
    unittest.main()
