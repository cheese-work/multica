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

    def test_builder_is_persistent_and_uses_github_cache(self):
        # CHE-3983 / CHE-6218: a fresh BuildKit container per run starts every Go, pnpm and
        # .next cache mount empty (0 of 195 runs hit the compile layers). Keep the named
        # builder so the cache mounts survive, and keep the remove-if-broken guard.
        steps = self.workflow['jobs']['build-images']['steps']
        setup = next(step for step in steps if step.get('id') == 'buildx')['with']
        self.assertEqual(setup['driver'], 'docker-container')
        self.assertEqual(setup['name'], 'multica-cd-${{ matrix.image }}-v1')
        self.assertIs(setup.get('cleanup'), False)
        guard = next(step for step in steps if step.get('name') == 'Remove a broken persistent Buildx builder')
        self.assertEqual(guard['env']['BUILDX_NAME'], setup['name'])
        config = setup['buildkitd-config-inline']
        self.assertIn('max-parallelism = 2', config)
        self.assertIn('maxUsedSpace = "8GB"', config)
        self.assertIn('minFreeSpace = "10GB"', config)
        self.assertFalse(any('docker container stop' in step.get('run', '') for step in steps))
        # CHE-7209: only the web leg keeps the GHA layer cache; the backend leg builds
        # from prebuilt binaries and has no layer worth caching.
        web = next(step['run'] for step in steps if step.get('name') == 'Build and publish immutable amd64 web')
        self.assertIn('--cache-from type=gha,scope=cd-${{ matrix.image }}', web)
        self.assertIn('--cache-to type=gha,mode=max,scope=cd-${{ matrix.image }}', web)
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
                  if str(step.get('with', {}).get('key', '')).startswith('turbo-')
                  and not step.get('uses', '').startswith('actions/cache/save@')]
        self.assertGreater(len(caches), 0)
        for cache in caches:
            with self.subTest(key=cache['key']):
                self.assertIn('github.ref', cache['key'])
                self.assertIn("hashFiles('pnpm-lock.yaml')", cache['key'])
                self.assertIn('github.run_id', cache['key'])
                self.assertNotIn('github.sha', cache['key'])
                self.assertIn("hashFiles('pnpm-lock.yaml')", cache['restore-keys'])

    def test_ci_turbo_caches_are_saved_only_from_main(self):
        # CHE-7210: PR and merge_group runs only restore. Entries saved on those refs are
        # readable only by that ref and filled the 10 GB repo cache, evicting main's entries.
        jobs = yaml.safe_load((ROOT / '.github/workflows/ci.yml').read_text())['jobs']
        found = 0
        for name, job in jobs.items():
            steps = job.get('steps', [])
            for step in steps:
                key = str(step.get('with', {}).get('key', ''))
                if not key.startswith('turbo-') or step.get('uses') == 'actions/cache/save@v6':
                    continue
                found += 1
                with self.subTest(job=name, key=key):
                    self.assertEqual(step['uses'], 'actions/cache/restore@v6')
                    self.assertIn("hashFiles('pnpm-lock.yaml')", step['with']['restore-keys'])
                    self.assertIn('refs/heads/main-', step['with']['restore-keys'])
                    saves = [s for s in steps if s.get('uses') == 'actions/cache/save@v6'
                             and s['with']['key'] == key]
                    self.assertEqual(len(saves), 1)
                    self.assertEqual(saves[0]['with']['path'], step['with']['path'])
                    self.assertEqual(saves[0]['if'], "${{ success() && github.ref == 'refs/heads/main' }}")
                    self.assertGreater(steps.index(saves[0]), steps.index(step) + 1)
        self.assertEqual(found, 3)

    def test_compiler_caches_precede_version_invalidation(self):
        backend = (ROOT / 'Dockerfile').read_text()
        self.assertIn('--mount=type=cache,target=/root/.cache/go-build', backend)
        for command in ['migrate', 'maintenance', 'backfill_task_usage_hourly', 'backfill_codex_usage_cache',
                   'backfill_issue_created_via']:
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

    def test_daemon_platforms_build_one_at_a_time(self):
        # CHE-3983 canary 1: four platforms at once beside the server stage got
        # `compile: signal: killed` in the 4 GB X99 dind (CHE-731). One go build at a time.
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            go = workspace / 'go'
            go.write_text('''#!/bin/sh
echo start >> "$GO_LOG"
sleep 1
while [ "$1" != -o ]; do shift; done
echo bin > "$2"
echo end >> "$GO_LOG"
''')
            go.chmod(0o755)
            log = workspace / 'go.log'
            result = subprocess.run(['sh', str(ROOT / 'scripts/build-daemon-binaries.sh'), str(workspace / 'out')],
                                    env={**os.environ, 'PATH': f'{workspace}:{os.environ["PATH"]}', 'GO_LOG': str(log)},
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertEqual(log.read_text().split(), ['start', 'end'] * 4, 'platform builds overlapped')
            manifest = json.loads((workspace / 'out/manifest.json').read_text())
            self.assertEqual(sorted(manifest['binaries']), ['darwin-amd64', 'darwin-arm64', 'linux-amd64', 'linux-arm64'])

    def test_backend_go_compiles_fit_the_x99_dind_memory_limit(self):
        # The two parallel stages inherit these from `deps`: at most 2 compiles each, ~1 GiB soft cap per process.
        backend = (ROOT / 'Dockerfile').read_text()
        deps = backend[backend.index('AS deps'):backend.index('AS builder')]
        self.assertRegex(deps, r'(?m)^ENV .*GOFLAGS=-p=2\b')
        self.assertRegex(deps, r'(?m)^ENV .*GOMEMLIMIT=1GiB\b')

    def test_daemon_platform_failure_fails_the_script(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            go = workspace / 'go'
            go.write_text('''#!/bin/sh
case "$GOARCH" in arm64) exit 1 ;; esac
while [ "$1" != -o ]; do shift; done
echo bin > "$2"
''')
            go.chmod(0o755)
            result = subprocess.run(['sh', str(ROOT / 'scripts/build-daemon-binaries.sh'), str(workspace / 'out')],
                                    env={**os.environ, 'PATH': f'{workspace}:{os.environ["PATH"]}'}, capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse((workspace / 'out/manifest.json').exists())

    def test_daemon_binaries_build_in_a_stage_parallel_to_the_server(self):
        backend = (ROOT / 'Dockerfile').read_text()
        stages = {line.split()[-1]: line.split()[1] for line in backend.splitlines() if line.startswith('FROM ') and ' AS ' in line}
        self.assertEqual(stages.get('builder'), 'deps')
        self.assertEqual(stages.get('daemon-builder'), 'deps')
        daemon_stage = backend[backend.index('AS daemon-builder'):backend.index('# --- Runtime stage')]
        server_stage = backend[backend.index('AS builder'):backend.index('AS daemon-builder')]
        self.assertIn('sh scripts/build-daemon-binaries.sh', daemon_stage)
        self.assertNotIn('build-daemon-binaries', server_stage)
        runtime = backend[backend.index('# --- Runtime stage'):]
        self.assertIn('COPY --from=daemon-builder /src/server/bin/daemon-binaries', runtime)
        self.assertIn('COPY --from=daemon-builder /src/server/bin/multica', runtime)

    def test_go_module_cache_is_available_to_every_consumer(self):
        backend = (ROOT / 'Dockerfile').read_text()
        consumers = [line for line in backend.splitlines() if line.startswith('RUN ') and
                     any(command in line for command in ['go mod download', 'go build', 'sh scripts/build-daemon-binaries.sh'])]
        self.assertEqual(len(consumers), 8)
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


# ---------------------------------------------------------------------------
# CHE-7209 (A1): compile the CD backend binaries on the X99 runner.
# ---------------------------------------------------------------------------
import re
import shlex
import shutil
import textwrap

SERVER_BINARIES = ['server', 'multica', 'migrate', 'maintenance',
                   'backfill_task_usage_hourly', 'backfill_codex_usage_cache',
                   'backfill_issue_created_via']
DAEMON_KEYS = ['linux-amd64', 'linux-arm64', 'darwin-arm64', 'darwin-amd64']
SHA = 'a' * 40
VERSION = '1.2.3'
BUILD = 'fork-20261118'
CHECKER = ROOT / 'deploy/cd/check-backend-payloads.py'
COMPILE_SCRIPT = ROOT / 'scripts/build-cd-backend-binaries.sh'
CD_DOCKERFILE = ROOT / 'Dockerfile.cd-backend'


def fake_go_output(path, revision=SHA, modified='false', ldflags='-s -w'):
    lines = [f'{path}: go1.27.3', '\tpath\texample.com/x', '\tbuild\t-buildmode=exe', '\tbuild\t-compiler=gc',
             f'\tbuild\t-ldflags="{ldflags}"', '\tbuild\tCGO_ENABLED=0', '\tbuild\tvcs=git']
    if revision is not None:
        lines.append(f'\tbuild\tvcs.revision={revision}')
    lines += ['\tbuild\tvcs.time=2026-11-18T00:00:00Z', f'\tbuild\tvcs.modified={modified}']
    return '\n'.join(lines) + '\n'


def server_ldflags(version=VERSION, build=BUILD, commit=SHA):
    return f'-s -w -X main.version={version} -X main.build={build} -X main.commit={commit}'


def set_gover(root, path, text):
    Path(root).parent.joinpath('gover', Path(path).name + '.gover').write_text(text)


def make_payloads(root, mutate=None):
    """Write a fake CD backend payload tree; the fake go answers from `<root>/../gover/<basename>.gover`."""
    root = Path(root)
    (root.parent / 'gover').mkdir(exist_ok=True)
    (root / 'daemon-binaries').mkdir(parents=True)
    entries = {}
    for key in DAEMON_KEYS:
        data = f'daemon {key}'.encode()
        (root / 'daemon-binaries' / f'multica-{key}').write_bytes(data)
        entries[key] = {'sha256': hashlib.sha256(data).hexdigest(), 'size': len(data)}
    (root / 'daemon-binaries/manifest.json').write_text(json.dumps({'commit': SHA, 'version': f'{VERSION}-{BUILD}', 'binaries': entries}))
    for name in SERVER_BINARIES:
        (root / name).write_bytes(f'binary {name}'.encode())
    files = [root / name for name in SERVER_BINARIES] + [root / 'daemon-binaries' / f'multica-{key}' for key in DAEMON_KEYS]
    for path in files:
        flags = server_ldflags() if path.name == 'server' else '-s -w'
        set_gover(root, path, fake_go_output(path, ldflags=flags))
    if mutate:
        mutate(root)
    return root


def install_fake_go(directory):
    fake = Path(directory, 'go')
    fake.write_text('#!/bin/sh\n[ "$1 $2" = "version -m" ] || exit 9\ncat "$(dirname "$0")/gover/$(basename "$3").gover"\n')
    fake.chmod(0o755)
    return fake


def run_checker(payloads, go, sha=SHA):
    return subprocess.run(['python3', str(CHECKER), '--dir', str(payloads), '--commit', sha, '--version', VERSION, '--build', BUILD],
                          env={**os.environ, 'GO': str(go)}, capture_output=True, text=True)


def docker_stages(path):
    stages, current = [], None
    for raw in path.read_text().splitlines():
        line = raw.strip()
        if line.startswith('FROM '):
            current = {'from': line, 'lines': []}
            stages.append(current)
        elif current is not None and line and not line.startswith('#'):
            current['lines'].append(line)
    return stages


def app_files(stage):
    files = set()
    for line in stage['lines']:
        if not line.startswith('COPY '):
            continue
        tokens = [t for t in shlex.split(line)[1:] if not t.startswith('--')]
        dest = tokens[-1].removeprefix('./')
        for source in tokens[:-1]:
            files.add(os.path.basename(source) if dest in ('.', '') else dest)
    return files


class CdBackendBinariesTests(unittest.TestCase):
    def setUp(self):
        self.workflow = yaml.safe_load((ROOT / '.github/workflows/cd-qualification.yml').read_text())
        self.steps = self.workflow['jobs']['build-images']['steps']

    # Acceptance 1
    def test_runtime_stage_matches_the_dockerfile_runtime_stage(self):
        original = docker_stages(ROOT / 'Dockerfile')[-1]
        prebuilt = docker_stages(CD_DOCKERFILE)[-1]
        self.assertEqual(original['from'], prebuilt['from'])
        for keyword in ['RUN', 'WORKDIR', 'EXPOSE', 'ENTRYPOINT']:
            self.assertEqual([l for l in original['lines'] if l.startswith(keyword + ' ')],
                             [l for l in prebuilt['lines'] if l.startswith(keyword + ' ')], keyword)
        self.assertEqual(app_files(original), app_files(prebuilt))
        self.assertIn('daemon-binaries', app_files(prebuilt))
        self.assertIn('server', app_files(prebuilt))

    def test_binaries_are_verified_before_the_runtime_stage_copies_them(self):
        stages = docker_stages(CD_DOCKERFILE)
        verify = next(stage for stage in stages if stage['from'].endswith(' AS verify'))
        text = '\n'.join(verify['lines'])
        self.assertIn('CD_BINARIES_SHA256', text)
        self.assertIn('sha256sum -c SHA256SUMS', text)
        self.assertIn('COPY --from=cdbin', text)
        runtime = stages[-1]
        copies = [l for l in runtime['lines'] if l.startswith('COPY --from=')]
        self.assertEqual({shlex.split(l)[1] for l in copies}, {'--from=verify'})
        self.assertGreaterEqual(len(copies), 7)

    def test_verify_stage_normalizes_file_modes_so_the_runner_umask_cannot_change_the_image(self):
        stages = docker_stages(CD_DOCKERFILE)
        verify = next(stage for stage in stages if stage['from'].endswith(' AS verify'))
        text = '\n'.join(verify['lines'])
        self.assertIn('chmod 644', text)
        self.assertIn('chmod 755', text)
        self.assertLess(text.index('sha256sum -c SHA256SUMS'), text.index('chmod 644'))

    # Acceptance 2
    def test_script_builds_the_dockerfile_binaries_with_the_same_ldflags(self):
        def builds(path, pattern):
            return sorted(m.groups() for m in re.finditer(pattern, path.read_text()))
        original = builds(ROOT / 'Dockerfile', r'go build -ldflags "([^"]*)" -o bin/(\w+) \./(cmd/\w+)')
        script = builds(COMPILE_SCRIPT, r'go build -ldflags "([^"]*)" -o "\$out/(\w+)" \./(cmd/\w+)')
        self.assertEqual(len(original), 6)
        self.assertEqual(script, original)
        text = COMPILE_SCRIPT.read_text()
        self.assertIn('scripts/build-daemon-binaries.sh', text)
        self.assertIn('multica-linux-amd64', text)
        self.assertIn('CGO_ENABLED=0 GOOS=linux GOARCH=amd64', text)
        self.assertIn('-p=3', text)
        self.assertIn('-buildvcs=true', text)
        self.assertNotIn('GOMEMLIMIT', text)

    def test_script_uses_the_go_version_of_the_dockerfile_with_a_pinned_checksum(self):
        version = re.search(r'^FROM golang:([0-9.]+)-alpine ', (ROOT / 'Dockerfile').read_text(), re.M).group(1)
        text = COMPILE_SCRIPT.read_text()
        self.assertIn('Dockerfile', text)
        self.assertIn('golang:', text)
        self.assertRegex(text, r'(?m)^\s*%s\) go_sha256=[0-9a-f]{64} ;;' % re.escape(version),
                         'raising the Dockerfile Go version needs a pinned checksum in the script')
        self.assertIn('no pinned checksum', text)
        self.assertIn('sha256sum -c', text)
        self.assertIn('GOTOOLCHAIN=local', text)

    # Acceptance 3
    def run_script(self, runner_temp, **env):
        trap = Path(runner_temp, 'bin')
        trap.mkdir(exist_ok=True)
        for tool in ['go', 'curl']:
            Path(trap, tool).write_text('#!/bin/sh\necho "$0 must not run before the cache guard" >&2\nexit 99\n')
            Path(trap, tool).chmod(0o755)
        return subprocess.run(['bash', str(COMPILE_SCRIPT)], cwd=ROOT, capture_output=True, text=True,
                              env={**os.environ, 'PATH': f'{trap}:{os.environ["PATH"]}', 'RUNNER_TEMP': str(runner_temp),
                                   'GITHUB_SHA': SHA, 'VERSION': VERSION, 'BUILD': BUILD, **env})

    def test_cache_guard_refuses_paths_outside_runner_temp_or_not_empty(self):
        with tempfile.TemporaryDirectory() as directory:
            temp = Path(directory, 'runner-temp')
            temp.mkdir()
            outside = Path(directory, 'outside')
            outside.mkdir()
            good = {'GOCACHE': str(temp / 'cd-go/build'), 'GOMODCACHE': str(temp / 'cd-go/mod')}
            (temp / 'cd-go/build').mkdir(parents=True)
            (temp / 'cd-go/mod').mkdir(parents=True)
            (temp / 'cd-go/mod/old').write_text('stale')
            (outside / 'x').write_text('x')
            cases = {
                'GOCACHE outside': {**good, 'GOCACHE': str(outside)},
                'GOMODCACHE outside': {**good, 'GOMODCACHE': str(outside)},
                'GOCACHE traversal': {**good, 'GOCACHE': str(temp / 'cd-go/build/../../../outside')},
                'GOCACHE is RUNNER_TEMP': {**good, 'GOCACHE': str(temp)},
                'GOMODCACHE not empty': good,
                'GOCACHE unset': {**good, 'GOCACHE': ''},
            }
            for name, env in cases.items():
                with self.subTest(name):
                    result = self.run_script(temp, **env)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotEqual(result.returncode, 99, result.stderr)
                    self.assertNotIn('must not run', result.stderr)
                    self.assertIn('cache', result.stderr.lower())
            (temp / 'cd-go/mod/old').unlink()
            (temp / 'cd-go/build/old').write_text('stale')
            with self.subTest('GOCACHE not empty'):
                result = self.run_script(temp, **good)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn('must not run', result.stderr)

    def test_workflow_removes_the_job_caches_even_after_a_failure(self):
        cleanup = [step for step in self.steps if 'cd-go' in step.get('run', '') and 'rm -rf' in step.get('run', '')]
        self.assertEqual(len(cleanup), 1)
        self.assertIn('always()', cleanup[0]['if'])
        self.assertEqual(self.steps[-1], cleanup[0], 'cleanup must be the last step')
        compile_step = next(step for step in self.steps if 'build-cd-backend-binaries.sh' in step.get('run', ''))
        env = compile_step.get('env', {})
        self.assertEqual(env.get('GOCACHE'), '${{ runner.temp }}/cd-go/build')
        self.assertEqual(env.get('GOMODCACHE'), '${{ runner.temp }}/cd-go/mod')

    # Acceptance 4
    def check(self, mutate=None, sha=SHA):
        with tempfile.TemporaryDirectory() as directory:
            fake = install_fake_go(directory)
            return run_checker(make_payloads(Path(directory, 'payloads'), mutate), fake, sha)

    def test_payload_check_passes_a_correct_set(self):
        result = self.check()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_payload_check_fails_when_the_manifest_commit_differs_from_the_sha(self):
        result = self.check(sha='b' * 40)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('manifest commit', result.stderr)

    def test_payload_check_fails_on_a_wrong_payload_sha256(self):
        def mutate(root):
            (root / 'daemon-binaries/multica-darwin-arm64').write_bytes(b'daemon darwin-arm64'[::-1])
        result = self.check(mutate)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('multica-darwin-arm64', result.stderr)

    def test_payload_check_fails_on_a_wrong_payload_size(self):
        def mutate(root):
            manifest = json.loads((root / 'daemon-binaries/manifest.json').read_text())
            manifest['binaries']['linux-arm64']['size'] += 1
            (root / 'daemon-binaries/manifest.json').write_text(json.dumps(manifest))
        result = self.check(mutate)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('size', result.stderr)

    def test_payload_check_fails_when_a_manifest_payload_is_missing(self):
        def mutate(root):
            manifest = json.loads((root / 'daemon-binaries/manifest.json').read_text())
            del manifest['binaries']['darwin-amd64']
            (root / 'daemon-binaries/manifest.json').write_text(json.dumps(manifest))
        result = self.check(mutate)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('darwin-amd64', result.stderr)

    def test_payload_check_fails_on_a_binary_with_another_vcs_revision(self):
        for victim in ['migrate', 'multica', 'daemon-binaries/multica-linux-amd64']:
            def mutate(root, victim=victim):
                path = root / victim
                set_gover(root, path, fake_go_output(path, revision='c' * 40))
            with self.subTest(victim):
                result = self.check(mutate)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('vcs.revision', result.stderr)

    def test_payload_check_fails_on_a_binary_without_vcs_revision_or_with_modified_tree(self):
        def no_revision(root):
            path = root / 'maintenance'
            set_gover(root, path, fake_go_output(path, revision=None))
        def modified(root):
            path = root / 'server'
            set_gover(root, path, fake_go_output(path, modified='true', ldflags=server_ldflags()))
        for mutate in [no_revision, modified]:
            with self.subTest(mutate.__name__):
                result = self.check(mutate)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('vcs.', result.stderr)

    def test_payload_check_fails_on_a_missing_server_stamp(self):
        for label, flags in [('missing version', '-s -w -X main.build=%s -X main.commit=%s' % (BUILD, SHA)),
                             ('missing build', '-s -w -X main.version=%s -X main.commit=%s' % (VERSION, SHA)),
                             ('missing commit', '-s -w -X main.version=%s -X main.build=%s' % (VERSION, BUILD)),
                             ('wrong commit', server_ldflags(commit='d' * 40)),
                             ('no stamp', '-s -w')]:
            def mutate(root, flags=flags):
                path = root / 'server'
                set_gover(root, path, fake_go_output(path, ldflags=flags))
            with self.subTest(label):
                result = self.check(mutate)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('main.', result.stderr)

    @unittest.skipUnless(shutil.which('go') and shutil.which('git'), 'needs go and git')
    def test_payload_check_reads_real_go_version_output(self):
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory, 'repo')
            repo.mkdir()
            (repo / 'go.mod').write_text('module example.com/x\n\ngo 1.21\n')
            (repo / 'main.go').write_text('package main\n\nvar version, build, commit string\n\nfunc main() {}\n')
            git = ['git', '-C', str(repo), '-c', 'user.name=t', '-c', 'user.email=t@example.com']
            subprocess.run(['git', 'init', '-q', str(repo)], check=True)
            subprocess.run([*git, 'add', '.'], check=True)
            subprocess.run([*git, 'commit', '-qm', 'x'], check=True)
            sha = subprocess.run([*git, 'rev-parse', 'HEAD'], capture_output=True, text=True, check=True).stdout.strip()
            payloads = Path(directory, 'payloads')
            (payloads / 'daemon-binaries').mkdir(parents=True)
            env = {**os.environ, 'GOFLAGS': '-buildvcs=true', 'GOCACHE': directory + '/c', 'GOPATH': directory + '/p', 'GOTOOLCHAIN': 'local'}
            ldflags = f'-s -w -X main.version={VERSION} -X main.build={BUILD} -X main.commit={sha}'
            # Two real builds (stamped server, plain) are copied to every file name: the test
            # reads real `go version -m` output, not Go cross-compilation.
            subprocess.run(['go', 'build', '-ldflags', ldflags, '-o', str(payloads / 'server'), '.'], cwd=repo, env=env, check=True)
            subprocess.run(['go', 'build', '-ldflags', '-s -w', '-o', str(payloads / 'plain'), '.'], cwd=repo, env=env, check=True)
            for name in SERVER_BINARIES:
                if name != 'server':
                    shutil.copy(payloads / 'plain', payloads / name)
            entries = {}
            for key in DAEMON_KEYS:
                target = payloads / 'daemon-binaries' / f'multica-{key}'
                shutil.copy(payloads / 'plain', target)
                entries[key] = {'sha256': hashlib.sha256(target.read_bytes()).hexdigest(), 'size': target.stat().st_size}
            (payloads / 'plain').unlink()
            (payloads / 'daemon-binaries/manifest.json').write_text(json.dumps({'commit': sha, 'version': f'{VERSION}-{BUILD}', 'binaries': entries}))
            result = subprocess.run(['python3', str(CHECKER), '--dir', str(payloads), '--commit', sha, '--version', VERSION, '--build', BUILD],
                                    env={**env, 'GO': 'go'}, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            wrong = subprocess.run(['python3', str(CHECKER), '--dir', str(payloads), '--commit', 'e' * 40, '--version', VERSION, '--build', BUILD],
                                   env={**env, 'GO': 'go'}, capture_output=True, text=True)
            self.assertNotEqual(wrong.returncode, 0)

    # Acceptance 5
    def step_index(self, predicate):
        return next(i for i, step in enumerate(self.steps) if predicate(step))

    def test_backend_leg_compiles_and_checks_payloads_before_the_image_build(self):
        compile_at = self.step_index(lambda s: 'build-cd-backend-binaries.sh' in s.get('run', ''))
        check_at = self.step_index(lambda s: 'check-backend-payloads.py' in s.get('run', ''))
        build_at = self.step_index(lambda s: s.get('name') == 'Build and publish immutable amd64 backend')
        self.assertLess(compile_at, check_at)
        self.assertLess(check_at, build_at)
        for index in (compile_at, check_at):
            self.assertEqual(self.steps[index]['if'], "matrix.image == 'backend'")
        self.assertNotIn('continue-on-error', self.steps[check_at])

    def test_backend_build_uses_prebuilt_binaries_and_no_layer_cache(self):
        step = next(s for s in self.steps if s.get('name') == 'Build and publish immutable amd64 backend')
        run = step['run']
        for needle in ['--file Dockerfile.cd-backend', '--build-context cdbin=', 'CD_BINARIES_SHA256', '--builder "$BUILDX_BUILDER"',
                       'test -n "$BUILDX_BUILDER"']:
            self.assertIn(needle, run)
        self.assertNotIn('--cache-from', run)
        self.assertNotIn('--cache-to', run)
        self.assertNotIn('--file Dockerfile .', run)

    def test_both_legs_keep_attestations_labels_push_and_digest_check(self):
        for name in ['Build and publish immutable amd64 backend', 'Build and publish immutable amd64 web']:
            run = next(s for s in self.steps if s.get('name') == name)['run']
            with self.subTest(name):
                for needle in ['--platform linux/amd64', '--provenance=mode=max', '--sbom=true', '--push', '--metadata-file',
                               'org.opencontainers.image.revision=${GITHUB_SHA}', 'org.opencontainers.image.source=',
                               'containerimage.digest', '--tag']:
                    self.assertIn(needle, run)
        web = next(s for s in self.steps if s.get('name') == 'Build and publish immutable amd64 web')['run']
        self.assertIn('--file Dockerfile.web', web)
        self.assertIn('--cache-from', web)
        self.assertIn('--cache-to', web)

    def test_unrelated_workflow_parts_are_unchanged(self):
        self.assertEqual(self.workflow['concurrency']['group'], 'cd-qualification-build-${{ github.ref }}')
        self.assertIs(self.workflow['concurrency']['cancel-in-progress'], False)
        self.assertEqual(sorted(self.workflow['jobs']), ['build-images', 'cd-qualification', 'd1-fixtures', 'fork-changelog'])


if __name__ == '__main__':
    unittest.main()
