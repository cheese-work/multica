"""Shared run-classification helpers for the CHE-2568 measurement."""
import re

# Activity that changes issue state. wakeup_checkin is deliberately absent:
# it is the "nothing to report" signal.
ACTIONS = {'status_changed', 'assignee_changed', 'wakeup_created', 'wakeup_paused', 'description_updated',
           'title_changed', 'priority_changed', 'due_date_changed', 'duplicate_added', 'duplicate_marked'}
# First 400 chars of a comment: says nothing changed, and claims no action verb.
NOOP = re.compile(r"(no action|no follow-?up|nothing (has )?(changed|to do|needed|new)|took no action|made no (change|changes)|changed nothing|i did nothing|did nothing|no change|posted no comment|posted nothing|still (open|waiting|pending|unanswered|draft|blocked)|is stale|false alarm|already (done|answered|landed)|no new (answer|comment|reply)|not ready yet|stays? (`|in )|left (it|everything) )", re.I)
DID = re.compile(r"(i (re-?ran|pushed|merged|fixed|opened|sent|created|promoted|closed|cancelled|moved|set a|renewed|resolved|started|edited|applied|posted a)|has been (merged|moved))", re.I)


def is_output(run, timeline):
    """True if the run authored a comment or has a PR url; None if unknown.

    `delivered_comment_ids` lists comments delivered TO the agent at claim
    time, so it is never output. An authored comment carries
    `source_task_id == run id` in the issue timeline.
    PR evidence is `result.pr_url` only: a run that pushed or opened a PR
    without recording `pr_url` is counted as no-output (lower bound on output).
    """
    if (run.get('result') or {}).get('pr_url'):
        return True
    if timeline is None:
        return None
    return any(e.get('type') == 'comment' and e.get('source_task_id') == run['id'] for e in timeline)


def noop_text(text):
    head = (text or '')[:400]
    return bool(NOOP.search(head)) and not DID.search(head)


def run_class(run, timeline):
    """substantive | ceremonial | silent | unknown.

    ceremonial = the run changed nothing and its only output is comments that
    say nothing changed. silent = changed nothing, no comment. Both are
    candidates for a server-side decision; substantive is not.
    """
    if (run.get('result') or {}).get('pr_url'):
        return 'substantive'
    if timeline is None:
        return 'unknown'
    lo, hi = run['started_at'], run['completed_at']
    if any(e.get('type') == 'activity' and e.get('action') in ACTIONS and e.get('actor_id') == run['agent_id']
           and lo <= e['created_at'] <= hi for e in timeline):
        return 'substantive'
    texts = [e.get('content') for e in timeline if e.get('type') == 'comment' and e.get('source_task_id') == run['id']]
    if not texts:
        return 'silent'
    return 'ceremonial' if all(noop_text(t) for t in texts) else 'substantive'
