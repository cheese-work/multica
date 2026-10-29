#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]


class BuildCacheTests(unittest.TestCase):
    def setUp(self):
        self.workflow = yaml.safe_load((ROOT / '.github/workflows/cd-qualification.yml').read_text())

    def test_images_use_separate_x99_memory_budgets(self):
        build = self.workflow['jobs']['build-images']
        self.assertEqual(build['runs-on'], ['self-hosted', 'linux', 'x64', 'cheese-x99'])
        self.assertEqual(build['strategy']['matrix']['image'], ['backend', 'web'])
        self.assertEqual(build['strategy']['max-parallel'], 2)
        self.assertFalse(build['strategy']['fail-fast'])
        commands = [step for step in build['steps'] if 'docker buildx build' in step.get('run', '')]
        self.assertEqual(len(commands), 2)
        for image, step in zip(['backend', 'web'], commands):
            self.assertEqual(step['if'], f"matrix.image == '{image}'")
            for flag in ['--platform linux/amd64', '--provenance=mode=max', '--sbom=true', '--push']:
                self.assertIn(flag, step['run'])
        self.assertEqual(self.workflow['jobs']['cd-qualification']['needs'], 'build-images')

    def test_builder_survives_without_retaining_running_memory(self):
        steps = self.workflow['jobs']['build-images']['steps']
        setup = next(step for step in steps if step.get('id') == 'buildx')['with']
        self.assertEqual(setup['name'], 'multica-cd-${{ matrix.image }}-v1')
        self.assertEqual(setup['driver'], 'docker-container')
        self.assertFalse(setup['cleanup'])
        config = setup['buildkitd-config-inline']
        self.assertIn('max-parallelism = 2', config)
        self.assertIn('maxUsedSpace = "4GB"', config)
        stop = next(step for step in steps if 'docker container stop' in step.get('run', ''))
        self.assertIn('always()', stop['if'])
        self.assertNotIn('docker buildx rm', stop['run'])

    def test_compiler_caches_precede_version_invalidation(self):
        backend = (ROOT / 'Dockerfile').read_text()
        self.assertIn('--mount=type=cache,target=/root/.cache/go-build', backend)
        for command in ['migrate', 'maintenance', 'backfill_task_usage_hourly', 'backfill_codex_usage_cache']:
            self.assertLess(backend.index(f'./cmd/{command}'), backend.index('ARG VERSION'))
        build_steps = [line for line in backend.splitlines() if line.startswith('RUN ') and ('go build' in line or 'sh scripts/build-daemon-binaries.sh' in line)]
        self.assertTrue(all('--mount=type=cache,target=/root/.cache/go-build' in line for line in build_steps))
        web = (ROOT / 'Dockerfile.web').read_text()
        self.assertIn('RUN --mount=type=cache,target=/app/apps/web/.next/cache', web)
        self.assertLess(web.index('RUN pnpm install --frozen-lockfile --offline'), web.index('ARG NEXT_PUBLIC_APP_VERSION'))

    def test_manifest_uses_both_image_digests_and_actual_web_entries(self):
        job = self.workflow['jobs']['cd-qualification']
        self.assertEqual(job['needs'], 'build-images')
        step = next(step for step in job['steps'] if step['name'] == 'Write immutable build-evidence manifest')
        for entries in [None, b'{"entries":[{"title":"Built web entries"}]}\n']:
            with self.subTest(entries=entries), tempfile.TemporaryDirectory() as directory:
                workspace = Path(directory)
                (workspace / 'deploy').symlink_to(ROOT / 'deploy', target_is_directory=True)
                (workspace / 'server').symlink_to(ROOT / 'server', target_is_directory=True)
                (workspace / 'backend.digest').write_text('sha256:' + 'a' * 64)
                (workspace / 'web.digest').write_text('sha256:' + 'b' * 64)
                if entries is not None:
                    (workspace / 'web-fork-changelog.json').write_bytes(entries)
                source_sha = '1' * 40
                result = subprocess.run(['bash', '-euo', 'pipefail', '-c', step['run']], cwd=workspace,
                                        env={**os.environ, 'GITHUB_SHA': source_sha, 'GITHUB_REPOSITORY': 'cheese-work/multica',
                                             'BACKEND_IMAGE': 'ghcr.io/cheese-work/multica-backend',
                                             'WEB_IMAGE': 'ghcr.io/cheese-work/multica-web'},
                                        capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                manifest = json.loads((workspace / 'cd-build-evidence.json').read_text())
                provenance = json.loads((workspace / 'cd-image-provenance.json').read_text())
                expected = 'absent' if entries is None else 'sha256:' + hashlib.sha256(entries).hexdigest()
                self.assertEqual(manifest['fork_changelog_sha256'], expected)
                self.assertEqual(provenance['fork_changelog_sha256'], expected)
                for image, digest in [('backend', 'a'), ('web', 'b')]:
                    self.assertEqual(manifest['image_digests'][image], 'sha256:' + digest * 64)
                    self.assertEqual(provenance['images'][image]['digest'], manifest['image_digests'][image])
                    self.assertEqual(provenance['images'][image]['source_sha'], source_sha)


if __name__ == '__main__':
    unittest.main()
