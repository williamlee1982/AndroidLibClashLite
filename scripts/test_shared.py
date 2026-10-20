"""Run host tests with the source overlay and Go tags used by the shared Android core.

Usage: python3 scripts/test_shared.py [-run TestRealConfigTailscaleStatus] [-v]
Extra arguments are passed to go test. All generated files stay outside the repo.
"""
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

from prepare_sources import prepare_sources


def main():
    repo = pathlib.Path(__file__).resolve().parents[1]
    env = os.environ.copy()
    for name in ('GOOS', 'GOARCH', 'GOARM', 'CC', 'CXX', 'CGO_ENABLED'):
        env.pop(name, None)
    module = json.loads(subprocess.check_output(
        ['go', 'mod', 'download', '-json', 'github.com/metacubex/mihomo'],
        cwd=repo, env=env, text=True))
    with tempfile.TemporaryDirectory(prefix='clash-shared-test-') as directory:
        stage = pathlib.Path(directory)
        core = stage / 'mihomo'
        shutil.copytree(module['Dir'], core)
        source, overlay = prepare_sources(repo, stage, core)
        # Android compiles separate native and callback DNS implementations.
        # The host needs Mihomo's original native implementation and methods.
        mapping = json.loads(overlay.read_text())
        del mapping['Replace'][str(core / 'dns/system_common.go')]
        overlay.write_text(json.dumps(mapping))
        subprocess.run(['go', 'mod', 'edit', '-replace=github.com/metacubex/mihomo=../mihomo'],
                       cwd=source, env=env, check=True)
        subprocess.run(['go', 'test', '-overlay', str(overlay), '-tags=foss,with_gvisor,cmfa',
                        *sys.argv[1:], './native/...', './environment/...'],
                       cwd=source, env=env, check=True)


if __name__ == '__main__':
    main()
