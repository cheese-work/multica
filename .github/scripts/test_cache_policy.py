import importlib.util
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
