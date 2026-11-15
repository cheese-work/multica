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
    def test_ssd2_cache_storage_does_not_fail_jobs(self):
        count = 0
        for path in (ROOT / '.github/workflows').glob('*.yml'):
            workflow = yaml.safe_load(path.read_text())
            for job in workflow.get('jobs', {}).values():
                for step in job.get('steps', []):
                    if step.get('name') != 'Use disposable build caches':
                        continue
                    count += 1
                    with self.subTest(workflow=path.name), tempfile.TemporaryDirectory() as directory:
                        fake_df = Path(directory, 'df')
                        fake_df.write_text('#!/bin/sh\nprintf "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/nvme1n1 100 1 99 1%% /_work\n"\n')
                        fake_df.chmod(0o755)
                        output = Path(directory, 'environment')
                        result = subprocess.run(['bash', '-euc', step['run']], cwd=directory, capture_output=True, text=True,
                                                env={**os.environ, "GITHUB_WORKSPACE": str(ROOT), 'PATH': directory + os.pathsep + os.environ['PATH'],
                                                     'RUNNER_TEMP': directory, 'GITHUB_ENV': str(output), 'GOFLAGS': '-count=1'})
                        self.assertEqual(result.returncode, 0, result.stderr)
                        environment = dict(line.split('=', 1) for line in output.read_text().splitlines())
                        self.assertEqual(environment['GOCACHE'], directory + '/build-cache/go-build')
                        self.assertEqual(environment['GOFLAGS'], '-count=1 -modcacherw')
                        self.assertNotIn('exit 1', step['run'])
        self.assertGreater(count, 0)

    def test_checked_out_jobs_share_cache_initialization(self):
        shared = 0
        for path in (ROOT / '.github/workflows').glob('*.yml'):
            workflow = yaml.safe_load(path.read_text())
            for job in workflow.get('jobs', {}).values():
                checkout = False
                for step in job.get('steps', []):
                    if 'checkout@' in step.get('uses', ''):
                        checkout = True
                    if step.get('name') == 'Use disposable build caches' and checkout and path.name != 'pr-guards.yml':
                        shared += 1
                        self.assertEqual(step['run'].strip(), 'bash "$GITHUB_WORKSPACE/.github/scripts/build-cache-env.sh"')
        self.assertGreater(shared, 0)

    def test_maintenance_has_no_build_cache_setup(self):
        workflow = yaml.safe_load((ROOT / '.github/workflows/cache-maintenance.yml').read_text())
        job = workflow['jobs']['cleanup']
        self.assertFalse(any(step.get('name') == 'Use disposable build caches' for step in job['steps']))
        self.assertEqual(job['runs-on'], 'ubuntu-24.04')

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
                      'test_cache_policy.py', 'test-candidate-preflight.py', 'test-deploy-approval.py', 'test-workflow-path-filters.sh',
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

    def test_every_buildx_call_is_bound_to_the_named_builder(self):
        # Runner ~/.docker persists across jobs. When the named builder already exists the
        # setup action skips `--use`, so an unbound call follows a stale buildx/current
        # pointer to the default docker driver, which rejects --provenance/--sbom.
        steps = self.workflow['jobs']['build-images']['steps']
        calls = [s for s in steps if 'docker buildx build' in s.get('run', '')
                 or 'docker buildx imagetools' in s.get('run', '')]
        self.assertEqual(len(calls), 3)
        for step in calls:
            self.assertEqual(
                step.get('env', {}).get('BUILDX_BUILDER'),
                '${{ steps.buildx.outputs.name }}',
                step['name'],
            )

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
                subprocess.run(['bash', '-euc', step['run']], cwd=directory, check=True,
                               env={**os.environ, "GITHUB_WORKSPACE": str(ROOT), 'RUNNER_TEMP': directory, 'GITHUB_ENV': str(output)})
                environment = dict(line.split('=', 1) for line in output.read_text().splitlines())
            for variable in ['GOCACHE', 'GOMODCACHE', 'XDG_CACHE_HOME', 'PIP_CACHE_DIR']:
                with self.subTest(workflow=filename, variable=variable):
                    self.assertTrue(environment.get(variable, '').startswith(directory + '/'))
            self.assertIn('-modcacherw', environment.get('GOFLAGS', ''))

    def test_turbo_caches_restore_branch_and_lockfile_generations(self):
        workflows = [yaml.safe_load((ROOT / f'.github/workflows/{name}').read_text()) for name in ['ci.yml', 'main-build.yml']]
        caches = [step['with'] for workflow in workflows for job in workflow['jobs'].values() for step in job.get('steps', [])
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
                                        env={**os.environ, "GITHUB_WORKSPACE": str(ROOT), 'GITHUB_SHA': source_sha, 'GITHUB_REPOSITORY': 'cheese-work/multica',
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
