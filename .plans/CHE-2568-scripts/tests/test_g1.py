import sys, unittest, pathlib
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
from g1 import evaluate, tally

AGENT, HUMAN = 'agent-a', 'human-1'
PR = 'https://github.com/o/r/pull/1'
RUN = {'id': 'run-1', 'agent_id': AGENT, 'started_at': '2026-10-26T10:00:00Z', 'completed_at': '2026-10-26T10:05:00Z',
       'merged_pr_url': PR, 'usd': 1.0, 'run_class': 'silent', 'result': {}}


def pr(url, created='2026-10-20T00:00:00Z', closed=None, merged=None):
    return {'html_url': url, 'pr_created_at': created, 'closed_at': closed, 'merged_at': merged}


def cm(cid, author_type='agent', parent=None, created='2026-10-25T00:00:00Z', resolved=None, by=None):
    return {'id': cid, 'author_type': author_type, 'parent_id': parent, 'created_at': created,
            'resolved_at': resolved, 'resolved_by_id': by}


def ev(prs=(), comments=(), timeline=(), complete=True, run=RUN):
    return evaluate(run, list(prs), list(comments), list(timeline), complete)


class Eligibility(unittest.TestCase):
    def test_no_other_pr_no_thread_is_eligible(self):
        self.assertIs(ev([pr(PR, merged='2026-10-26T09:00:00Z')])['eligible'], True)

    def test_other_open_pr_is_not_eligible(self):
        r = ev([pr(PR), pr('https://github.com/o/r/pull/2')])
        self.assertEqual((r['eligible'], r['other_open']), (False, 1))

    def test_other_pr_merged_before_start_is_not_open(self):
        self.assertTrue(ev([pr('https://github.com/o/r/pull/2', merged='2026-10-26T09:00:00Z')])['eligible'])

    def test_other_pr_created_after_start_is_ignored(self):
        self.assertTrue(ev([pr('https://github.com/o/r/pull/2', created='2026-10-27T00:00:00Z')])['eligible'])

    def test_unresolved_agent_only_thread_is_not_eligible(self):
        r = ev(comments=[cm('t1')])
        self.assertEqual((r['eligible'], r['agent_only_unresolved']), (False, 1))

    def test_thread_resolved_before_start_is_ignored(self):
        self.assertTrue(ev(comments=[cm('t1', resolved='2026-10-25T12:00:00Z')])['eligible'])

    def test_thread_resolved_after_start_still_counts(self):
        self.assertFalse(ev(comments=[cm('t1', resolved='2026-10-26T10:02:00Z', by=AGENT)])['eligible'])

    def test_human_reply_before_start_makes_thread_not_agent_only(self):
        r = ev(comments=[cm('t1'), cm('r1', 'member', parent='t1')])
        self.assertEqual((r['agent_only_unresolved'], r['human_unresolved']), (0, 1))

    def test_human_reply_after_start_does_not_count(self):
        r = ev(comments=[cm('t1'), cm('r1', 'member', parent='t1', created='2026-10-26T11:00:00Z')])
        self.assertEqual(r['agent_only_unresolved'], 1)

    def test_human_root_thread_is_not_agent_only(self):
        self.assertEqual(ev(comments=[cm('t1', 'member')])['agent_only_unresolved'], 0)

    def test_incomplete_comment_list_is_unknown(self):
        # A folded list hides replies in resolved threads, so agent-only cannot be inferred.
        r = ev(comments=[cm('t1', resolved='2026-10-26T10:02:00Z')], complete=False)
        self.assertIsNone(r['eligible'])

    def test_incomplete_list_is_known_when_other_pr_is_open(self):
        self.assertIs(ev([pr(PR), pr('https://github.com/o/r/pull/2')], complete=False)['eligible'], False)

    def test_pr_without_created_at_is_unknown(self):
        self.assertIsNone(ev([pr(PR), dict(pr('https://github.com/o/r/pull/2'), pr_created_at=None)])['eligible'])


class Verdict(unittest.TestCase):
    def test_state_change_is_false_positive(self):
        tl = [{'type': 'activity', 'action': 'status_changed', 'actor_id': AGENT, 'created_at': '2026-10-26T10:03:00Z'}]
        self.assertEqual(ev(timeline=tl)['verdict'], 'false_positive')

    def test_real_work_comment_is_false_positive(self):
        tl = [{'type': 'comment', 'source_task_id': 'run-1', 'content': 'I pushed a fix.'}]
        self.assertEqual(ev(timeline=tl)['verdict'], 'false_positive')

    def test_noop_comment_is_saved(self):
        tl = [{'type': 'comment', 'source_task_id': 'run-1', 'content': 'No follow-up needed. I changed nothing.'}]
        self.assertEqual(ev(timeline=tl)['verdict'], 'saved')

    def test_thread_resolve_only_is_server_duty(self):
        r = ev(comments=[cm('t1', resolved='2026-10-26T10:06:00Z', by=AGENT)])
        self.assertEqual(r['verdict'], 'server_duty')

    def test_run_pr_output_is_false_positive(self):
        # result.pr_url is the PR the run produced, not the merged PR that triggered it.
        run = dict(RUN, result={'pr_url': 'https://github.com/o/r/pull/9'})
        r = ev(run=run)
        self.assertEqual(r['verdict'], 'false_positive')

    def test_run_pr_output_wins_over_thread_resolve(self):
        run = dict(RUN, result={'pr_url': 'https://github.com/o/r/pull/9'})
        r = ev(comments=[cm('t1', resolved='2026-10-26T10:06:00Z', by=AGENT)], run=run)
        self.assertEqual(r['verdict'], 'false_positive')

    def test_run_without_result_is_not_pr_output(self):
        run = {k: v for k, v in RUN.items() if k != 'result'}
        self.assertEqual(ev(run=run)['verdict'], 'saved')

    def test_trigger_pr_url_is_not_run_output(self):
        self.assertEqual(ev()['verdict'], 'saved')

    def test_missing_timeline_is_unknown(self):
        self.assertEqual(evaluate(RUN, [], [], None, True)['verdict'], 'unknown')


class Tally(unittest.TestCase):
    def rec(self, eligible, verdict, usd=1.0):
        return {'eligible': eligible, 'verdict': verdict, 'usd': usd}

    def test_savings_need_eligible_and_saved(self):
        t = tally([self.rec(True, 'saved', 2.0)])
        self.assertEqual((t['saved_runs'], t['saved_usd']), (1, 2.0))

    def test_eligible_false_positive_is_not_savings(self):
        t = tally([self.rec(True, 'false_positive', 0.44)])
        self.assertEqual((t['saved_runs'], t['false_positive_runs']), (0, 1))

    def test_eligible_server_duty_is_not_savings(self):
        t = tally([self.rec(True, 'server_duty')])
        self.assertEqual((t['saved_runs'], t['server_duty_runs']), (0, 1))

    def test_not_eligible_is_not_savings(self):
        self.assertEqual(tally([self.rec(False, 'saved')])['saved_runs'], 0)

    def test_unknown_eligibility_or_verdict_stays_unknown(self):
        t = tally([self.rec(None, 'saved', 3.0), self.rec(True, 'unknown', 4.0)])
        self.assertEqual((t['saved_runs'], t['unknown_runs'], t['unknown_usd']), (0, 2, 7.0))

    def test_every_matched_run_lands_in_exactly_one_bucket(self):
        recs = [self.rec(True, 'saved'), self.rec(True, 'false_positive'), self.rec(True, 'server_duty'),
                self.rec(None, 'saved'), self.rec(False, 'saved'), self.rec(True, 'unknown')]
        t = tally(recs)
        parts = t['saved_runs'] + t['false_positive_runs'] + t['server_duty_runs'] + t['unknown_runs'] + t['not_eligible_runs']
        self.assertEqual((t['matched_runs'], parts), (6, 6))


if __name__ == '__main__':
    unittest.main()
