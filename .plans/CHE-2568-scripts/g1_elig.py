"""G1 eligibility per matched run, from per-issue pull-requests list and comment list (current state, read after the window).
other_open: another linked PR was open at run start. unresolved: a root comment thread was unresolved at run start.
resolved_by_run: the run's agent resolved a thread between run start and 10 min after run end."""
import json,sys
from datetime import datetime,timedelta
def ts(s): return datetime.fromisoformat(s.replace('Z','+00:00')) if s else None
def elig(r,prs,cms,agent_id):
    s,e=ts(r['started']),ts(r['done'])
    other=[p for p in prs if p['html_url']!=r['f']['pr_url'] and ts(p['pr_created_at']) and ts(p['pr_created_at'])<=s
           and not ((ts(p['merged_at']) and ts(p['merged_at'])<=s) or (ts(p['closed_at']) and ts(p['closed_at'])<=s))]
    roots=[c for c in cms if not c.get('parent_id')]
    unres=[c for c in roots if ts(c['created_at'])<=s and not (ts(c['resolved_at']) and ts(c['resolved_at'])<=s)]
    byrun=[c for c in roots if ts(c['resolved_at']) and s<=ts(c['resolved_at'])<=e+timedelta(minutes=10) and c.get('resolved_by_id')==agent_id]
    return dict(other_open=len(other),unresolved=len(unres),resolved_by_run=len(byrun))
