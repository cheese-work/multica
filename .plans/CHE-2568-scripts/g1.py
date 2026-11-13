"""G1 shadow evaluation for the CHE-2568 plan.

`evaluate` takes one run that G1 would have suppressed plus the issue's linked PRs, comments and timeline,
and returns eligibility (would the G1a design suppress it) and a shadow verdict (what the run actually did).
`tally` turns the records into savings. A run counts as saved only when it is eligible AND the verdict is
`saved`. Anything that could not be established stays unknown.

Verdict, one rule for every gate:
- false_positive: the run changed issue state, authored a comment that is not a "nothing changed" note,
  or has a PR url.
- server_duty: its only work was resolving comment threads.
- saved: none of the above. A "nothing changed" comment is the saving, not work.
- unknown: no timeline.
"""
import re
from datetime import datetime, timedelta

STATE_ACTIONS = {'status_changed', 'assignee_changed', 'wakeup_created', 'wakeup_paused', 'description_updated',
                 'title_changed', 'priority_changed', 'due_date_changed', 'duplicate_added', 'duplicate_marked'}
NOOP = re.compile(r"(no action|no follow-?up|nothing (has )?(changed|to do|needed|new)|took no action|made no (change|changes)|changed nothing|i did nothing|did nothing|no change|posted no comment|posted nothing|still (open|waiting|pending|unanswered|draft|blocked)|is stale|false alarm|already (done|answered|landed)|no new (answer|comment|reply)|not ready yet|stays? (`|in )|left (it|everything) )", re.I)
DID = re.compile(r"(i (re-?ran|pushed|merged|fixed|opened|sent|created|promoted|closed|cancelled|moved|set a|renewed|resolved|started|edited|applied|posted a)|has been (merged|moved))", re.I)
RESOLVE_GRACE = timedelta(minutes=10)


def _ts(s):
    return datetime.fromisoformat(s.replace('Z', '+00:00')) if s else None


def _noop_text(text):
    head = (text or '')[:400]
    return bool(NOOP.search(head)) and not DID.search(head)


def _open_prs(run, prs, start):
    """Other linked PRs open at run start; None when a PR lacks a creation time."""
    others = [p for p in prs if p['html_url'] != run['merged_pr_url']]
    if any(not p.get('pr_created_at') for p in others):
        return None
    return [p for p in others if _ts(p['pr_created_at']) <= start
            and not (p.get('merged_at') and _ts(p['merged_at']) <= start)
            and not (p.get('closed_at') and _ts(p['closed_at']) <= start)]


def _threads(comments, start):
    """(agent_only, human) counts of root threads unresolved at run start, judged on comments up to start."""
    agent_only = human = 0
    for root in comments:
        if root.get('parent_id') or _ts(root['created_at']) > start:
            continue
        if root.get('resolved_at') and _ts(root['resolved_at']) <= start:
            continue
        members = [root] + [c for c in comments if c.get('parent_id') == root['id'] and _ts(c['created_at']) <= start]
        if all(c['author_type'] == 'agent' for c in members):
            agent_only += 1
        else:
            human += 1
    return agent_only, human


def _verdict(run, comments, timeline):
    if timeline is None:
        return 'unknown'
    lo, hi = run['started_at'], run['completed_at']
    if any(e.get('type') == 'activity' and e.get('action') in STATE_ACTIONS and e.get('actor_id') == run['agent_id']
           and lo <= e['created_at'] <= hi for e in timeline):
        return 'false_positive'
    if any(e.get('type') == 'comment' and e.get('source_task_id') == run['id'] and not _noop_text(e.get('content'))
           for e in timeline):
        return 'false_positive'
    if (run.get('result') or {}).get('pr_url'):
        return 'false_positive'
    start, end = _ts(lo), _ts(hi) + RESOLVE_GRACE
    if any(c.get('resolved_by_id') == run['agent_id'] and c.get('resolved_at') and start <= _ts(c['resolved_at']) <= end
           for c in comments):
        return 'server_duty'
    return 'saved'


def evaluate(run, prs, comments, timeline, complete):
    """`complete` is False when the comment list was folded: resolved threads hide replies, so agent-only is unknown."""
    start = _ts(run['started_at'])
    other = _open_prs(run, prs, start)
    agent_only, human = _threads(comments, start)
    if other:
        eligible = False
    elif other is None or not complete:
        eligible = None
    else:
        eligible = agent_only == 0
    return {'eligible': eligible, 'other_open': None if other is None else len(other),
            'agent_only_unresolved': agent_only, 'human_unresolved': human,
            'verdict': _verdict(run, comments, timeline)}


def _bucket(r):
    if r['eligible'] is False:
        return 'not_eligible'
    if r['eligible'] is None or r['verdict'] == 'unknown':
        return 'unknown'
    return r['verdict']


def tally(records):
    out = {'matched_runs': len(records)}
    for name in ('saved', 'false_positive', 'server_duty', 'unknown', 'not_eligible'):
        rs = [r for r in records if _bucket(r) == name]
        out[name + '_runs'] = len(rs)
        out[name + '_usd'] = round(sum(r['usd'] for r in rs), 2)
    return out
