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
    def test_ssd2_cache_storage_is_rejected_before_any_cache_export(self):
        filename = 'release.yml' if ROOT.name == 'multica' else 'ci.yml' if ROOT.name == 'artemis' else 'backend-ci.yml' if ROOT.name == 'sub2api' else 'run-tests.yml'
        workflow = yaml.safe_load((ROOT / '.github/workflows' / filename).read_text())
        steps = [step for job in workflow['jobs'].values() for step in job.get('steps', [])]
        step = next(step for step in steps if step.get('name') == 'Use disposable build caches')
        with tempfile.TemporaryDirectory() as directory:
            fake_df = Path(directory, 'df')
            fake_df.write_text('#!/bin/sh\nprintf "Filesystem 1024-blocks Used Available Capacity Mounted on\\n/dev/nvme1n1 100 1 99 1%% /_work\\n"\n')
            fake_df.chmod(0o755)
            output = Path(directory, 'environment')
            result = subprocess.run(['bash', '-euc', step['run']], capture_output=True, text=True,
                                    env={**os.environ, 'PATH': directory + os.pathsep + os.environ['PATH'],
                                         'RUNNER_TEMP': directory, 'GITHUB_ENV': str(output)})
            self.assertNotEqual(result.returncode, 0, 'ssd2-backed runner temp must not receive build caches')
            self.assertIn('ssd2', result.stderr)
            self.assertFalse(output.exists(), 'no cache path is exported on the incompatible lane')

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

    def test_builder_is_ephemeral_and_uses_github_cache(self):
        steps = self.workflow['jobs']['build-images']['steps']
        setup = next(step for step in steps if step.get('id') == 'buildx')['with']
        self.assertEqual(setup['driver'], 'docker-container')
        self.assertTrue(setup.get('cleanup', True))
        config = setup['buildkitd-config-inline']
        self.assertIn('max-parallelism = 2', config)
        self.assertIn('maxUsedSpace = "4GB"', config)
        self.assertFalse(any('docker container stop' in step.get('run', '') for step in steps))
        commands = [step['run'] for step in steps if 'docker buildx build' in step.get('run', '')]
        for command in commands:
            self.assertIn('--cache-from type=gha,scope=cd-${{ matrix.image }}', command)
            self.assertIn('--cache-to type=gha,mode=max,scope=cd-${{ matrix.image }}', command)
        runtime = next(step for step in steps if 'ACTIONS_RUNTIME_TOKEN' in str(step.get('with', {})))
        self.assertIn('core.setSecret', runtime['with']['script'])

    def test_self_hosted_tooling_uses_disposable_cache_paths(self):
        for filename, job_name in [('release.yml', 'verify'), ('cd-deploy.yml', 'candidate-preflight')]:
            workflow = yaml.safe_load((ROOT / '.github/workflows' / filename).read_text())
            step = next(step for step in workflow['jobs'][job_name]['steps']
                        if step.get('name') == 'Use disposable build caches')
            with tempfile.TemporaryDirectory() as directory:
                output = Path(directory, 'environment')
                subprocess.run(['bash', '-euc', step['run']], check=True,
                               env={**os.environ, 'RUNNER_TEMP': directory, 'GITHUB_ENV': str(output)})
                environment = dict(line.split('=', 1) for line in output.read_text().splitlines())
            for variable in ['GOCACHE', 'GOMODCACHE', 'XDG_CACHE_HOME', 'PIP_CACHE_DIR']:
                with self.subTest(workflow=filename, variable=variable):
                    self.assertTrue(environment.get(variable, '').startswith(directory + '/'))
            self.assertIn('-modcacherw', environment.get('GOFLAGS', ''))

    def test_turbo_caches_restore_branch_and_lockfile_generations(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/ci.yml').read_text())
        caches = [step['with'] for job in workflow['jobs'].values() for step in job.get('steps', [])
                  if str(step.get('with', {}).get('key', '')).startswith('turbo-')]
        self.assertGreater(len(caches), 0)
        for cache in caches:
            with self.subTest(key=cache['key']):
                self.assertIn('github.ref', cache['key'])
                self.assertIn("hashFiles('pnpm-lock.yaml')", cache['key'])
                self.assertIn('github.run_id', cache['key'])
                self.assertNotIn('github.sha', cache['key'])
                self.assertIn("hashFiles('pnpm-lock.yaml')", cache['restore-keys'])

    def test_compiler_caches_precede_version_invalidation(self):
        backend = (ROOT / 'Dockerfile').read_text()
        self.assertIn('--mount=type=cache,target=/root/.cache/go-build', backend)
        for command in ['migrate', 'maintenance', 'backfill_task_usage_hourly', 'backfill_codex_usage_cache']:
            self.assertLess(backend.index(f'./cmd/{command}'), backend.index('ARG VERSION'))
        build_steps = [line for line in backend.splitlines() if line.startswith('RUN ') and ('go build' in line or 'sh scripts/build-daemon-binaries.sh' in line)]
        self.assertTrue(all('--mount=type=cache,target=/root/.cache/go-build' in line for line in build_steps))
        web = (ROOT / 'Dockerfile.web').read_text()
        self.assertIn('RUN --mount=type=cache,target=/app/apps/web/.next/cache', web)
        self.assertLess(web.index('pnpm install --frozen-lockfile --offline'), web.index('ARG NEXT_PUBLIC_APP_VERSION'))

    def test_go_module_cache_is_available_to_every_consumer(self):
        backend = (ROOT / 'Dockerfile').read_text()
        consumers = [line for line in backend.splitlines() if line.startswith('RUN ') and
                     any(command in line for command in ['go mod download', 'go build', 'sh scripts/build-daemon-binaries.sh'])]
        self.assertEqual(len(consumers), 7)
        for command in consumers:
            with self.subTest(command=command):
                self.assertIn('--mount=type=cache,target=/go/pkg/mod', command)

    def test_pnpm_installs_share_the_explicit_cache_store(self):
        web = (ROOT / 'Dockerfile.web').read_text()
        installs = [line for line in web.splitlines() if line.startswith('RUN ') and 'pnpm install' in line]
        self.assertEqual(len(installs), 2)
        for command in installs:
            with self.subTest(command=command):
                self.assertIn('--mount=type=cache,target=/pnpm/store,sharing=locked', command)
                self.assertIn('--store-dir=/pnpm/store', command)

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
