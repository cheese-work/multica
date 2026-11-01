"""G1 evidence for the CHE-2568 plan. Run in the fetch directory after classify.py.

  python3 g1_report.py fetch    # per matched issue: pull-requests list, timeline, comment list into g1/
  python3 g1_report.py report   # tally; writes g1_evidence.json

G1 matches: pr.merged wakeup, issue_status done, layer_delivered false.
Eligibility (the G1 design): no other linked PR open at run start, no unresolved agent-only thread.
Verdict per matched run: shadow.shadow_verdict (one rule for all gates).
"""
import json, re, subprocess, sys, os, collections
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime
from g1_elig import elig
from shadow import shadow_verdict

rows = json.load(open('rows2.json'))
def ts(s): return datetime.fromisoformat(s.replace('Z', '+00:00')) if s else None
def facts(r):
    m = re.search(r'Trigger facts[^\n]*\n(\S+) (\{.*\})', r['note']); return json.loads(m.group(2)) if m else {}
matched = [dict(r, f=facts(r)) for r in rows
           if r['cls'].startswith('wakeup:pr.merged') and facts(r).get('issue_status') == 'done' and facts(r).get('layer_delivered') is False]

def mc(args):
    p = subprocess.run(['multica', 'issue'] + args + ['--output', 'json'], capture_output=True, text=True)
    return json.loads(p.stdout)

def fetch_one(i):
    cm = mc(['comment', 'list', i]); cm = cm if isinstance(cm, list) else cm.get('comments', cm)
    json.dump(dict(prs=mc(['pull-requests', 'list', i]), tl=mc(['timeline', i]), cm=cm), open(f'g1/{i}.json', 'w'))

def agent_only_unresolved(r, cms):
    s = ts(r['started'])
    roots = [c for c in cms if not c.get('parent_id') and ts(c['created_at']) <= s and not (ts(c['resolved_at']) and ts(c['resolved_at']) <= s)]
    return sum(1 for c in roots if c['author_type'] == 'agent' and all(
        x['author_type'] == 'agent' for x in cms if x.get('parent_id') == c['id'] and ts(x['created_at']) <= s))

if sys.argv[1] == 'fetch':
    os.makedirs('g1', exist_ok=True)
    with ThreadPoolExecutor(8) as ex: list(ex.map(fetch_one, sorted({r['issue'] for r in matched})))
else:
    out = []
    for r in matched:
        d = json.load(open(f"g1/{r['issue']}.json"))
        run = dict(id=r['id'], agent_id=r['agent_id'], started_at=r['started'], completed_at=r['done'], result={})
        e = elig(r, d['prs']['pull_requests'], d['cm'], r['agent_id'])
        v = shadow_verdict(run, d['tl'], d['cm'])
        out.append(dict(id=r['id'], issue=r['issue'], rc=r['rc'], usd=round(r['usd'], 2), other_open=e['other_open'],
                        unresolved_agent_only=agent_only_unresolved(r, d['cm']), resolved_by_run=e['resolved_by_run'],
                        verdict=v['verdict'], reasons=v['reasons']))
    json.dump(out, open('g1_evidence.json', 'w'), indent=0)
    usd = lambda x: round(sum(r['usd'] for r in x), 1)
    print('matched', len(out), usd(out))
    for k, v in sorted(collections.Counter(r['verdict'] for r in out).items()):
        print(' verdict', k, v, usd([r for r in out if r['verdict'] == k]))
    ok = [r for r in out if r['other_open'] == 0 and r['unresolved_agent_only'] == 0]
    print('eligible under the G1 design (no other open PR, no agent-only unresolved thread):', len(ok), usd(ok))
    print('silent only (certain no-comment bound):', len([r for r in out if r['rc'] == 'silent']), usd([r for r in out if r['rc'] == 'silent']))
