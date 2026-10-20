"""Checks generated Mihomo patches that cannot be exercised deterministically by Go tests.

Run: python3 -m unittest scripts/test_prepare_sources.py
"""
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from prepare_sources import prepare_sources  # noqa: E402

REPO = pathlib.Path(__file__).resolve().parents[1]


class PrepareSourcesTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        module = json.loads(subprocess.check_output(
            ['go', 'mod', 'download', '-json', 'github.com/metacubex/mihomo'], cwd=REPO, text=True))
        cls.tmp = tempfile.TemporaryDirectory(prefix='prepare-sources-test-')
        stage = pathlib.Path(cls.tmp.name)
        core = pathlib.Path(module['Dir'])
        _, overlay = prepare_sources(REPO, stage, core)
        replace = json.loads(overlay.read_text())['Replace']
        cls.tailscale = pathlib.Path(replace[str(core / 'adapter/outbound/tailscale.go')]).read_text()

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def close_body(self):
        start = self.tailscale.index('func (t *Tailscale) Close() error {')
        return self.tailscale[start:self.tailscale.index('\n}\n', start)]

    def test_close_cancels_before_inbound_cleanup(self):
        # M3: InboundController re-checks t.ctx after LoadOrStore. Only if Close
        # cancels before deleting the map entry does one of the two always see
        # the other, so a controller can never be left behind for a closed node.
        body = self.close_body()
        self.assertIn('tailscaleInboundClosed(t)', body)
        self.assertLess(body.index('t.cancel()'), body.index('tailscaleInboundClosed(t)'))


if __name__ == '__main__':
    unittest.main()
