"""Shadow-mode verdict for one run that a gate would have suppressed.

One observable rule for every gate. The run still ran, so compare its window with what it did:
- false_positive: it changed issue state, authored a comment that is not a "nothing changed" note,
  or has a PR url. Suppressing it would have dropped a hand-off.
- server_duty: its only work was resolving comment threads. Safe only if the server resolves them.
- saved: it did none of the above (a "nothing changed" comment is the saving, not work).
- unknown: no timeline. Fails open: never counted as saved.
"""
from datetime import datetime, timedelta
from lib import ACTIONS, noop_text

RESOLVE_GRACE = timedelta(minutes=10)


def _ts(s):
    return datetime.fromisoformat(s.replace('Z', '+00:00')) if s else None


def shadow_verdict(run, timeline, comments):
    if timeline is None:
        return {'verdict': 'unknown', 'reasons': []}
    lo, hi = run['started_at'], run['completed_at']
    reasons = []
    if any(e.get('type') == 'activity' and e.get('action') in ACTIONS and e.get('actor_id') == run['agent_id']
           and lo <= e['created_at'] <= hi for e in timeline):
        reasons.append('state_change')
    if any(e.get('type') == 'comment' and e.get('source_task_id') == run['id'] and not noop_text(e.get('content'))
           for e in timeline):
        reasons.append('comment')
    if (run.get('result') or {}).get('pr_url'):
        reasons.append('pr_output')
    if reasons:
        return {'verdict': 'false_positive', 'reasons': reasons}
    start, end = _ts(lo), _ts(hi) + RESOLVE_GRACE
    if any(c.get('resolved_by_id') == run['agent_id'] and c.get('resolved_at') and start <= _ts(c['resolved_at']) <= end
           for c in comments):
        return {'verdict': 'server_duty', 'reasons': ['thread_resolved']}
    return {'verdict': 'saved', 'reasons': []}
