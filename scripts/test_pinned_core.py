"""Guards the Mihomo pin: it must be a published stable tag, never an Alpha pseudo-version.

Run: python3 -m unittest scripts/test_pinned_core.py
"""
from pathlib import Path
import re
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import check_upstream  # noqa: E402

REPO = Path(__file__).resolve().parents[1]


class PinnedCoreTest(unittest.TestCase):
    def pinned(self):
        match = re.search(r'(?m)^\s*' + re.escape(check_upstream.MODULE) + r'\s+(\S+)', (REPO / 'go.mod').read_text())
        self.assertIsNotNone(match, 'go.mod does not require Mihomo')
        return match.group(1)

    def test_mihomo_is_pinned_to_a_stable_tag(self):
        # check_upstream.version() raises for pseudo-versions and Alpha/RC tags, which would also
        # break the tidy workflow's stable tracking.
        check_upstream.version(self.pinned())

    def test_mihomo_is_not_replaced(self):
        replaces = re.findall(r'(?m)^replace\s+(\S+)', (REPO / 'go.mod').read_text())
        self.assertNotIn(check_upstream.MODULE, replaces)


if __name__ == '__main__':
    unittest.main()
