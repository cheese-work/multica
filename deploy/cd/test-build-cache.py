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
        self.assertEqual(sorted(self.workflow['jobs']['cd-qualification']['needs']), ['build-images', 'd1-fixtures'])

    def test_off_x99_jobs_keep_x99_runner_slots_for_image_builds(self):
        # CHE-3983: queue wait on the shared X99 pool dominated slow runs. Fixtures and
        # manifest assembly need neither Docker nor X99, so they run on hosted runners.
        jobs = self.workflow['jobs']
        for name in ['d1-fixtures', 'cd-qualification']:
            with self.subTest(job=name):
                self.assertEqual(jobs[name]['runs-on'], 'ubuntu-24.04')
        fixtures = '\n'.join(step.get('run', '') for step in jobs['d1-fixtures']['steps'])
        for suite in ['test-release-manifest.sh', 'test-admission.sh', 'test-record-checks.sh', 'test-build-cache.py',
                      'test-candidate-preflight.py', 'test-deploy-approval.py', 'test-workflow-path-filters.sh',
                      'test-isolated-qualification.sh', 'test-tuple-snapshot.sh']:
            self.assertIn(suite, fixtures)
        self.assertIn('ensure-pyyaml.sh', fixtures)
        build = '\n'.join(step.get('run', '') for step in jobs['build-images']['steps'])
        self.assertNotIn('deploy/cd/test-', build)
        self.assertTrue(any(step.get('uses', '').startswith('actions/setup-node@') for step in jobs['cd-qualification']['steps']))

    def test_web_builder_extends_deps_instead_of_copying_node_modules(self):
        web = (ROOT / 'Dockerfile.web').read_text()
        self.assertIn('FROM deps AS builder', web)
        self.assertNotIn('COPY --from=deps', web)

    def test_web_image_build_skips_next_typecheck_that_ci_already_runs(self):
        self.assertIn('ENV NEXT_SKIP_TYPECHECK=true', (ROOT / 'Dockerfile.web').read_text())
        self.assertIn('NEXT_SKIP_TYPECHECK', (ROOT / 'apps/web/next.config.ts').read_text())

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
        self.assertLess(web.index('pnpm install --frozen-lockfile --offline'), web.index('ARG NEXT_PUBLIC_APP_VERSION'))

    def test_broken_named_builder_is_removed_before_setup(self):
        steps = self.workflow['jobs']['build-images']['steps']
        before_setup = steps[:next(i for i, step in enumerate(steps) if step.get('id') == 'buildx')]
        recovery = '\n'.join(step.get('run', '') for step in before_setup
                             if 'docker buildx' in step.get('run', ''))
        for image in ['backend', 'web']:
            for state in ['healthy', 'bootstrap-failed', 'node-error', 'wrong-driver', 'absent']:
                with self.subTest(image=image, state=state), tempfile.TemporaryDirectory() as directory:
                    workspace = Path(directory)
                    docker = workspace / 'docker'
                    docker.write_text('''#!/usr/bin/env bash
set -eu
case "$1 $2" in
  'buildx ls')
    # Real buildx: ls takes --format only; json prints one object per builder.
    [ "$3" = --format ] || exit 125
    case "$4" in
      '{{.Name}}')
        echo unrelated-builder
        [ "$STUB_STATE" = absent ] || echo "$BUILDX_NAME"
        ;;
      json)
        echo '{"Name":"unrelated-builder","Driver":"docker","Nodes":[{}]}'
        if [ "$STUB_STATE" = node-error ]; then
          echo '{"Name":"'"$BUILDX_NAME"'","Driver":"docker-container","Nodes":[{"Err":"dead context"}]}'
        elif [ "$STUB_STATE" = wrong-driver ]; then
          echo '{"Name":"'"$BUILDX_NAME"'","Driver":"docker","Nodes":[{}]}'
        elif [ "$STUB_STATE" != absent ]; then
          echo '{"Name":"'"$BUILDX_NAME"'","Driver":"docker-container","Nodes":[{}]}'
        fi
        ;;
      *) exit 125 ;;
    esac
    ;;
  'buildx inspect')
    # Real buildx: inspect has no --format flag.
    [ "$3" = "$BUILDX_NAME" ] && [ "${4:-}" = --bootstrap ] && [ "$#" -eq 4 ] || exit 125
    [ "$STUB_STATE" != bootstrap-failed ] || exit 1
    ;;
  'buildx rm') printf '%s\\n' "$*" >> "$REMOVALS" ;;
  *) exit 2 ;;
esac
''')
                    docker.chmod(0o755)
                    removals = workspace / 'removals'
                    name = f'multica-cd-{image}-v1'
                    result = subprocess.run(['bash', '-euo', 'pipefail', '-c', recovery], cwd=ROOT,
                                            env={**os.environ, 'PATH': f'{workspace}:{os.environ["PATH"]}',
                                                 'BUILDX_NAME': name, 'STUB_STATE': state,
                                                 'REMOVALS': str(removals)}, capture_output=True, text=True)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    removed = removals.read_text() if removals.exists() else ''
                    if state in ['bootstrap-failed', 'node-error', 'wrong-driver']:
                        self.assertIn(name, removed, 'broken named builder was retained')
                        self.assertEqual(len(removed.splitlines()), 1)
                    else:
                        self.assertEqual(removed, '')
                    self.assertNotIn('unrelated-builder', removed)

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
        self.assertIn('build-images', job['needs'])
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
