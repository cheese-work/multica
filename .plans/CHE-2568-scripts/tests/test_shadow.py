import sys, unittest, pathlib
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
from shadow import shadow_verdict

AGENT = 'agent-a'
RUN = {'id': 'run-1', 'agent_id': AGENT, 'started_at': '2026-10-26T10:00:00Z', 'completed_at': '2026-10-26T10:05:00Z', 'result': {}}


def activity(action, at, actor=AGENT):
    return {'type': 'activity', 'action': action, 'actor_id': actor, 'created_at': at}


def comment(text, source='run-1'):
    return {'type': 'comment', 'content': text, 'source_task_id': source, 'created_at': '2026-10-26T10:04:00Z'}


def resolved(at, by=AGENT):
    return {'id': 'c1', 'resolved_at': at, 'resolved_by_id': by}


class ShadowVerdict(unittest.TestCase):
    def v(self, timeline=(), comments=(), run=RUN):
        return shadow_verdict(run, list(timeline), list(comments))

    def test_nothing_done_is_saved(self):
        self.assertEqual(self.v()['verdict'], 'saved')

    def test_noop_comment_alone_is_saved(self):
        self.assertEqual(self.v([comment('No follow-up needed. I changed nothing.')])['verdict'], 'saved')

    def test_state_change_in_window_is_false_positive(self):
        r = self.v([activity('status_changed', '2026-10-26T10:03:00Z')])
        self.assertEqual((r['verdict'], r['reasons']), ('false_positive', ['state_change']))

    def test_state_change_by_another_actor_is_ignored(self):
        self.assertEqual(self.v([activity('status_changed', '2026-10-26T10:03:00Z', actor='other')])['verdict'], 'saved')

    def test_state_change_outside_window_is_ignored(self):
        self.assertEqual(self.v([activity('status_changed', '2026-10-26T11:00:00Z')])['verdict'], 'saved')

    def test_real_work_comment_is_false_positive(self):
        r = self.v([comment('I re-ran the failed job and pushed a fix.')])
        self.assertEqual((r['verdict'], r['reasons']), ('false_positive', ['comment']))

    def test_comment_from_another_run_is_ignored(self):
        self.assertEqual(self.v([comment('I pushed a fix.', source='run-2')])['verdict'], 'saved')

    def test_pr_url_is_false_positive(self):
        run = dict(RUN, result={'pr_url': 'https://github.com/o/r/pull/1'})
        self.assertEqual(self.v(run=run)['reasons'], ['pr_output'])

    def test_thread_resolve_only_is_server_duty_not_false_positive(self):
        r = self.v(comments=[resolved('2026-10-26T10:06:00Z')])
        self.assertEqual((r['verdict'], r['reasons']), ('server_duty', ['thread_resolved']))

    def test_thread_resolved_by_someone_else_is_ignored(self):
        self.assertEqual(self.v(comments=[resolved('2026-10-26T10:06:00Z', by='other')])['verdict'], 'saved')

    def test_false_positive_wins_over_server_duty(self):
        r = self.v([activity('status_changed', '2026-10-26T10:03:00Z')], [resolved('2026-10-26T10:06:00Z')])
        self.assertEqual(r['verdict'], 'false_positive')

    def test_missing_timeline_fails_open(self):
        self.assertEqual(shadow_verdict(RUN, None, [])['verdict'], 'unknown')


if __name__ == '__main__':
    unittest.main()
