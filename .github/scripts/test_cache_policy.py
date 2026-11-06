import contextlib
import importlib.util
import io
import subprocess
from unittest import mock
from pathlib import Path
import unittest


spec = importlib.util.spec_from_file_location('cache_policy', Path(__file__).with_name('cache-policy.py'))
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


def cache(identifier, key, ref='refs/heads/main', size=100):
    return {'id': identifier, 'key': key, 'ref': ref, 'size_in_bytes': size,
            'created_at': f'2026-10-{identifier:02d}T00:00:00Z',
            'last_accessed_at': f'2026-10-{identifier:02d}T00:00:00Z'}


class CachePolicyTests(unittest.TestCase):
    def test_shared_managed_families_keep_only_latest_generation(self):
        for family in ['turbo-', 'go-build-race-', 'release-go-', 'golangci-lint.cache-', 'go-job-']:
            with self.subTest(family=family):
                prefix = family + 'Linux-X64-refs/heads/main-'
                self.assertEqual(
                    policy.plan_deletions([
                        cache(1, prefix + 'a' * 64 + '-1'),
                        cache(2, prefix + 'b' * 64 + '-2'),
                    ]),
                    [1],
                )

    def test_missing_cache_during_delete_does_not_stop_cleanup(self):
        entries = [cache(1, 'first'), cache(2, 'second')]
        missing = subprocess.CalledProcessError(1, ['gh', 'cache', 'delete'], stderr='X Could not find a cache matching 1 in cheese-work/test')
        with mock.patch('sys.argv', ['cache-policy', '--repo', 'cheese-work/test']), \
             mock.patch.object(policy, 'api', side_effect=[{'default_branch': 'main'}, {'actions_caches': entries}, {'active_caches_size_in_bytes': 0}]), \
             mock.patch.object(policy, 'plan_deletions', return_value=[1, 2]), \
             mock.patch.object(policy.subprocess, 'run', side_effect=[missing, subprocess.CompletedProcess([], 0)]) as delete, \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            policy.main()
        self.assertEqual(delete.call_count, 2)
        self.assertEqual(delete.call_args_list[1].args[0][3], '2')

    def test_permission_denied_during_delete_still_fails(self):
        denied = subprocess.CalledProcessError(1, ['gh', 'cache', 'delete'], stderr='HTTP 403: Forbidden')
        with mock.patch('sys.argv', ['cache-policy', '--repo', 'cheese-work/test']), \
             mock.patch.object(policy, 'api', side_effect=[{'default_branch': 'main'}, {'actions_caches': [cache(1, 'first')]}]), \
             mock.patch.object(policy, 'plan_deletions', return_value=[1]), \
             mock.patch.object(policy.subprocess, 'run', side_effect=denied), \
             contextlib.redirect_stdout(io.StringIO()), self.assertRaises(subprocess.CalledProcessError):
            policy.main()

    def test_superseded_linter_generations_are_compacted(self):
        prefix = 'golangci-lint.cache-Linux-X64-refs/heads/main-'
        self.assertEqual(policy.plan_deletions([cache(1, prefix + 'a' * 64 + '-1'), cache(2, prefix + 'b' * 64 + '-2')]), [1])

    def test_superseded_generation_is_deleted_not_latest(self):
        prefix = 'turbo-build-Linux-X64-v22-refs/heads/main-'
        caches = [cache(1, prefix + 'a' * 64 + '-101-1'), cache(2, prefix + 'a' * 64 + '-102-1')]
        self.assertEqual(policy.plan_deletions(caches), [1])

    def test_legacy_commit_keys_are_compacted(self):
        prefix = 'turbo-build-Linux-X64-v22-'
        self.assertEqual(policy.plan_deletions([cache(1, prefix + 'a' * 40), cache(2, prefix + 'b' * 40)]), [1])

    def test_branches_and_architectures_are_independent(self):
        caches = [cache(1, 'turbo-build-Linux-X64-v22-' + 'a' * 40),
                  cache(2, 'turbo-build-Linux-ARM64-v22-' + 'b' * 40),
                  cache(3, 'turbo-build-Linux-X64-v22-' + 'c' * 40, 'refs/pull/1/merge')]
        self.assertEqual(policy.plan_deletions(caches), [])

    def test_budget_evicts_oldest_non_default_branch(self):
        caches = [cache(1, 'module-cache', 'refs/pull/1/merge', 200), cache(2, 'module-cache', size=200)]
        self.assertEqual(policy.plan_deletions(caches, target=300, limit=400), [1])

    def test_required_default_branch_caches_over_free_limit_stop(self):
        with self.assertRaisesRegex(ValueError, 'free limit'):
            policy.plan_deletions([cache(1, 'modules', size=401)], target=300, limit=400)

    def test_required_default_branch_cache_may_use_headroom(self):
        self.assertEqual(policy.plan_deletions([cache(1, 'modules', size=350)], target=300, limit=400), [])

    def test_no_caches_is_a_noop(self):
        self.assertEqual(policy.plan_deletions([]), [])

    def test_buildkit_blobs_are_not_superseded_generations(self):
        caches = [cache(1, 'buildkit-blob-1-' + 'a' * 64), cache(2, 'buildkit-blob-1-' + 'b' * 64)]
        self.assertEqual(policy.plan_deletions(caches), [])

    def test_unmanaged_archive_versions_remain_independent(self):
        caches = [dict(cache(1, 'modules'), version='one'), dict(cache(2, 'modules'), version='two')]
        self.assertEqual(policy.plan_deletions(caches), [])


if __name__ == '__main__':
    unittest.main()
