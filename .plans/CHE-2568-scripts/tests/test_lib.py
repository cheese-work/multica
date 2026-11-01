import sys, unittest, pathlib
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
from lib import is_output, run_class

AGENT = 'agent-a'
RUN = {'id': 'run-1', 'agent_id': AGENT, 'delivered_comment_ids': [], 'result': {}}


def comment(cid, author, source_task_id=None):
    return {'type': 'comment', 'id': cid, 'actor_id': author, 'source_task_id': source_task_id}


class IsOutput(unittest.TestCase):
    def test_incoming_comment_receipt_is_not_output(self):
        # delivered_comment_ids lists comments delivered TO the agent at claim time.
        run = dict(RUN, delivered_comment_ids=['c-in'])
        tl = [comment('c-in', 'human-1')]
        self.assertFalse(is_output(run, tl))

    def test_authored_comment_is_output_even_with_empty_receipts(self):
        tl = [comment('c-out', AGENT, source_task_id='run-1')]
        self.assertTrue(is_output(RUN, tl))

    def test_comment_from_another_run_is_not_output(self):
        tl = [comment('c-other', AGENT, source_task_id='run-2')]
        self.assertFalse(is_output(RUN, tl))

    def test_pr_url_is_output(self):
        run = dict(RUN, result={'pr_url': 'https://github.com/o/r/pull/1'})
        self.assertTrue(is_output(run, []))

    def test_missing_timeline_without_pr_is_unknown(self):
        self.assertIsNone(is_output(RUN, None))


RUN2 = dict(RUN, started_at='2026-11-01T08:00:00Z', completed_at='2026-11-01T08:05:00Z')


def activity(action, actor=AGENT, at='2026-11-01T08:02:00Z'):
    return {'type': 'activity', 'action': action, 'actor_id': actor, 'created_at': at}


class RunClass(unittest.TestCase):
    def test_no_change_comment_is_ceremonial(self):
        tl = [comment('c', AGENT, 'run-1') | {'content': 'CHE-1 stays in `todo`. I took no action.'}]
        self.assertEqual(run_class(RUN2, tl), 'ceremonial')

    def test_comment_with_work_is_substantive(self):
        tl = [comment('c', AGENT, 'run-1') | {'content': 'I pushed the fix and opened PR 5.'}]
        self.assertEqual(run_class(RUN2, tl), 'substantive')

    def test_status_change_by_the_agent_is_substantive(self):
        tl = [activity('status_changed')]
        self.assertEqual(run_class(RUN2, tl), 'substantive')

    def test_status_change_by_another_actor_does_not_count(self):
        tl = [activity('status_changed', actor='other')]
        self.assertEqual(run_class(RUN2, tl), 'silent')

    def test_status_change_outside_run_window_does_not_count(self):
        tl = [activity('status_changed', at='2026-11-01T09:00:00Z')]
        self.assertEqual(run_class(RUN2, tl), 'silent')

    def test_wakeup_checkin_alone_is_silent(self):
        self.assertEqual(run_class(RUN2, [activity('wakeup_checkin')]), 'silent')

    def test_mixed_noop_and_real_comment_is_substantive(self):
        tl = [comment('a', AGENT, 'run-1') | {'content': 'No action needed.'},
              comment('b', AGENT, 'run-1') | {'content': 'I merged main into the branch.'}]
        self.assertEqual(run_class(RUN2, tl), 'substantive')

    def test_missing_timeline_is_unknown(self):
        self.assertEqual(run_class(RUN2, None), 'unknown')


if __name__ == '__main__':
    unittest.main()
