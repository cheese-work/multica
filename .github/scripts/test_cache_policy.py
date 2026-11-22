import contextlib
import datetime
import importlib.util
import io
import json
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


BUILD_FAMILY = 'go-cd-build-Linux-X64-1.27.3'
MOD_FAMILY = 'go-cd-mod-Linux-X64-1.27.3'


def build_key(tag):
    return f'{BUILD_FAMILY}-refs/heads/main-{tag * 64}'


def mod_key(tag):
    return f'{MOD_FAMILY}-{tag * 64}'


class GoCdFamilyTests(unittest.TestCase):
    # CHE-7211 acceptance 1
    def test_go_cd_build_entries_keep_only_the_newest_on_main(self):
        caches = [cache(1, build_key('a')), cache(2, build_key('b')), cache(3, build_key('c'))]
        self.assertEqual(policy.plan_deletions(caches), [1, 2])

    def test_go_cd_module_entries_keep_only_the_newest_on_main(self):
        caches = [cache(1, mod_key('a')), cache(2, mod_key('b'))]
        self.assertEqual(policy.plan_deletions(caches), [1])

    def test_go_cd_build_and_module_families_are_independent(self):
        self.assertEqual(policy.plan_deletions([cache(1, build_key('a')), cache(2, mod_key('a'))]), [])


GIB = 1_000_000_000


def usage(total):
    return {'active_caches_size_in_bytes': total, 'active_caches_count': 1}


def listing(entries):
    return {'total_count': len(entries), 'actions_caches': entries}


class FakeApi:
    """Replaces policy.http_api: serves the cache list in pages of 100 and the usage value."""

    def __init__(self, entries, usage_bytes=None, fail=(), drop_page=None):
        self.entries = entries
        self.usage_bytes = sum(e['size_in_bytes'] for e in entries) if usage_bytes is None else usage_bytes
        self.fail = fail
        self.drop_page = drop_page

    def __call__(self, endpoint):
        if 'usage' in endpoint:
            if 'usage' in self.fail:
                raise OSError('usage read failed')
            return usage(self.usage_bytes)
        page = int(endpoint.rsplit('page=', 1)[1])
        if 'list' in self.fail or page == self.drop_page:
            raise OSError('list read failed')
        batch = self.entries[(page - 1) * 100:page * 100]
        return {'total_count': len(self.entries), 'actions_caches': batch}


class CheckSaveTests(unittest.TestCase):
    # CHE-7211 acceptance 2
    def decide(self, entries, candidates, **fake):
        with mock.patch.object(policy, 'http_api', FakeApi(entries, **fake)):
            return policy.check_save('cheese-work/test', candidates)

    def test_empty_family_with_room_saves(self):
        result = self.decide([], [(build_key('a'), 500_000_000)])
        self.assertEqual([(r['save'], r['reason']) for r in result], [(True, 'ok')])
        self.assertEqual(result[0]['family'], BUILD_FAMILY)
        self.assertEqual(result[0]['family_entries'], 0)
        self.assertEqual(result[0]['current_bytes'], 0)
        self.assertEqual(result[0]['projected_bytes'], 500_000_000)

    def test_family_with_one_entry_saves(self):
        result = self.decide([cache(1, build_key('a'), size=GIB)], [(build_key('b'), 500_000_000)])
        self.assertEqual([(r['save'], r['reason'], r['family_entries']) for r in result], [(True, 'ok', 1)])

    def test_family_with_two_entries_skips_cleanup_pending(self):
        entries = [cache(1, build_key('a'), size=GIB), cache(2, build_key('b'), ref='refs/pull/9/merge', size=GIB)]
        result = self.decide(entries, [(build_key('c'), 500_000_000)])
        self.assertEqual([(r['save'], r['reason'], r['family_entries']) for r in result], [(False, 'cleanup-pending', 2)])

    def test_projected_total_above_budget_skips_over_budget(self):
        result = self.decide([cache(1, 'other', size=8_600_000_000)], [(build_key('a'), 500_000_000)])
        self.assertEqual([(r['save'], r['reason']) for r in result], [(False, 'over-budget')])
        self.assertEqual(result[0]['projected_bytes'], 9_100_000_000)

    def test_projected_total_exactly_at_budget_saves(self):
        result = self.decide([cache(1, 'other', size=8_500_000_000)], [(build_key('a'), 500_000_000)])
        self.assertEqual([(r['save'], r['reason']) for r in result], [(True, 'ok')])

    def test_failed_usage_read_skips_unverified(self):
        result = self.decide([], [(build_key('a'), 1)], fail=('usage',))
        self.assertEqual([(r['save'], r['reason']) for r in result], [(False, 'unverified')])

    def test_failed_list_read_skips_unverified(self):
        result = self.decide([], [(build_key('a'), 1)], fail=('list',))
        self.assertEqual([(r['save'], r['reason']) for r in result], [(False, 'unverified')])

    def test_missing_list_page_skips_unverified(self):
        entries = [cache(i % 28 + 1, f'other-{i}', size=1) for i in range(150)]
        result = self.decide(entries, [(build_key('a'), 1)], drop_page=2)
        self.assertEqual([(r['save'], r['reason']) for r in result], [(False, 'unverified')])

    def test_incomplete_list_skips_unverified(self):
        # total_count says 3 entries exist; the pages returned only 1.
        def short(endpoint):
            if 'usage' in endpoint:
                return usage(1)
            return {'total_count': 3, 'actions_caches': [cache(1, 'other')]}
        with mock.patch.object(policy, 'http_api', short):
            result = policy.check_save('cheese-work/test', [(build_key('a'), 1)])
        self.assertEqual([(r['save'], r['reason']) for r in result], [(False, 'unverified')])

    def test_every_page_of_a_long_list_is_read(self):
        entries = [cache(i % 28 + 1, f'other-{i}', size=1) for i in range(250)]
        result = self.decide(entries, [(build_key('a'), 1)])
        self.assertEqual(result[0]['current_bytes'], 250)

    def test_go_sum_change_with_room_for_both_saves_both(self):
        result = self.decide([cache(1, build_key('a'), size=GIB), cache(2, mod_key('a'), size=GIB)],
                             [(build_key('b'), 500_000_000), (mod_key('b'), 300_000_000)])
        self.assertEqual([(r['save'], r['reason']) for r in result], [(True, 'ok'), (True, 'ok')])
        self.assertEqual([r['projected_bytes'] for r in result], [2_500_000_000, 2_800_000_000])

    def test_go_sum_change_with_room_for_the_build_entry_only_saves_the_build_entry(self):
        result = self.decide([cache(1, 'other', size=8_300_000_000)],
                             [(build_key('b'), 500_000_000), (mod_key('b'), 300_000_000)])
        self.assertEqual([(r['save'], r['reason']) for r in result], [(True, 'ok'), (False, 'over-budget')])
        self.assertEqual(result[1]['projected_bytes'], 9_100_000_000)

    def test_other_families_count_toward_bytes_not_toward_the_family_count(self):
        entries = [cache(i, f'other-{i}', size=GIB) for i in range(1, 6)]
        result = self.decide(entries, [(build_key('a'), 500_000_000)])
        self.assertEqual([(r['save'], r['family_entries']) for r in result], [(True, 0)])
        self.assertEqual(result[0]['current_bytes'], 5 * GIB)

    def test_a_usage_value_below_the_list_sum_uses_the_list_sum(self):
        result = self.decide([cache(1, 'other', size=8_700_000_000)], [(build_key('a'), 500_000_000)], usage_bytes=1)
        self.assertEqual([(r['save'], r['reason'], r['current_bytes']) for r in result], [(False, 'over-budget', 8_700_000_000)])

    def test_a_usage_value_above_the_list_sum_uses_the_usage_value(self):
        result = self.decide([], [(build_key('a'), 500_000_000)], usage_bytes=8_700_000_000)
        self.assertEqual([(r['save'], r['reason'], r['current_bytes']) for r in result], [(False, 'over-budget', 8_700_000_000)])

    def test_cli_prints_one_json_line_per_key_and_fails_closed_on_a_crash(self):
        out = io.StringIO()
        with mock.patch.object(policy, 'http_api', FakeApi([])), \
             mock.patch('sys.argv', ['cache-policy', '--repo', 'cheese-work/test', '--check-save',
                                     '--candidate', f'{build_key("a")}=10', '--candidate', f'{mod_key("a")}=20']), \
             contextlib.redirect_stdout(out):
            policy.main()
        lines = [json.loads(line) for line in out.getvalue().splitlines()]
        self.assertEqual([line['key'] for line in lines], [build_key('a'), mod_key('a')])
        self.assertEqual([line['save'] for line in lines], [True, True])
        for line in lines:
            self.assertEqual(sorted(line), ['current_bytes', 'family', 'family_entries', 'key', 'projected_bytes', 'reason', 'save'])
        with mock.patch.object(policy, 'http_api', FakeApi([], fail=('usage',))), \
             mock.patch('sys.argv', ['cache-policy', '--repo', 'cheese-work/test', '--check-save', '--candidate', f'{build_key("a")}=10']), \
             contextlib.redirect_stdout(io.StringIO()) as failed:
            policy.main()
        self.assertEqual(json.loads(failed.getvalue())['reason'], 'unverified')

    def test_check_save_never_deletes(self):
        with mock.patch.object(policy, 'http_api', FakeApi([cache(1, build_key('a')), cache(2, build_key('b'))])), \
             mock.patch.object(policy.subprocess, 'run') as run, \
             mock.patch('sys.argv', ['cache-policy', '--repo', 'cheese-work/test', '--check-save', '--candidate', f'{build_key("c")}=1']), \
             contextlib.redirect_stdout(io.StringIO()):
            policy.main()
        run.assert_not_called()


class MergeQueueRefTests(unittest.TestCase):
    QUEUE = 'refs/heads/gh-readonly-queue/main/pr-1-' + 'a' * 40

    def test_idle_merge_queue_ref_caches_are_deleted(self):
        entry = cache(5, 'turbo-build-Linux-X64-v22-refs/heads/gh-readonly-queue/main/pr-1-x-1', ref=self.QUEUE)
        now = datetime.datetime(2026, 10, 5, 2, tzinfo=datetime.timezone.utc)
        self.assertEqual(policy.plan_deletions([entry], now=now), [5])

    def test_recent_merge_queue_ref_caches_are_kept(self):
        entry = cache(5, 'turbo-build-Linux-X64-v22-refs/heads/gh-readonly-queue/main/pr-1-x-1', ref=self.QUEUE)
        now = datetime.datetime(2026, 10, 5, 0, 30, tzinfo=datetime.timezone.utc)
        self.assertEqual(policy.plan_deletions([entry], now=now), [])


class PullRequestOnlySetupGoTests(unittest.TestCase):
    def test_go_lint_does_not_save_a_setup_go_cache_per_pull_request(self):
        # CHE-5128: go-lint runs on pull requests only, so main never holds the
        # 1.26.x setup-go entry and every PR ref saved its own ~397 MB copy.
        ci = Path(__file__).parents[1].joinpath('workflows', 'ci.yml').read_text()
        job = ci.split('\n  go-lint:\n', 1)[1].split('\n  # ', 1)[0]
        step = job.split('uses: actions/setup-go@v7', 1)[1].split('- name:', 1)[0]
        self.assertRegex(step, r'(?m)^\s+cache: false\s*$')


if __name__ == '__main__':
    unittest.main()
