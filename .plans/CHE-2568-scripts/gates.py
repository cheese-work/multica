"""Gate predicates evaluated on the week's runs (run in the fetch directory, after classify.py).

For each gate: matched = runs the predicate would suppress. saved = matched runs that changed
nothing (rc ceremonial/silent) and would be safely dropped. fp = matched runs that did real work
(rc substantive): each is a would-be dropped hand-off and must be read before the gate enforces.
Predicate inputs are limited to facts the server holds at dispatch time.
"""
import json, re, os, collections
from datetime import datetime
rows = json.load(open('rows2.json'))
def ts(s): return datetime.fromisoformat(s.replace('Z', '+00:00'))
def tl(i):
    f = f'tl/{i}.json'
    return json.load(open(f)) if i and os.path.exists(f) else None
def facts(r):
    m = re.search(r'Trigger facts[^\n]*\n(\S+) (\{.*\})', r['note'])
    return json.loads(m.group(2)) if m else {}
tok = lambda r: r['cw'] + r['cr'] + r['i'] + r['o']
nc = lambda r: r['rc'] in ('ceremonial', 'silent')

pm = [r for r in rows if r['cls'].startswith('wakeup:pr.merged')]
pe = [r for r in rows if r['cls'].startswith('wakeup:pr.event')]
cf = [r for r in rows if r['cls'].startswith('wakeup:pr.checks_failed')]
heads = collections.defaultdict(list)  # (pr_url, old_head) -> times a newer head was announced
for r in pe:
    f = facts(r)
    if f.get('kind') == 'head_changed': heads[(f.get('pr_number'), f.get('old_head_sha'))].append(r['started'])
def stale(r):
    f = facts(r)
    return any(t < r['started'] for t in heads.get((f.get('pr_number'), f.get('head_sha')), []))
def mover_is_agent(r):
    t = tl(r['issue']) or []
    ev = [e for e in t if e.get('action') == 'status_changed' and e['created_at'] <= r['created']]
    if not ev: return None
    e = max(ev, key=lambda e: e['created_at'])
    return e['details'].get('to') == 'need_decision' and e.get('actor_id') == r['agent_id']
def unchanged(r):  # nothing on the issue since this agent's previous run ended
    t = tl(r['issue']); prev = [p for p in rows if p['issue'] == r['issue'] and p['agent'] == r['agent'] and p['done'] <= r['started']]
    if t is None or not prev: return None
    a = max(p['done'] for p in prev)
    return not any(e.get('action') not in ('task_completed', 'task_failed', 'wakeup_triggered', 'wakeup_checkin') and a < e['created_at'] <= r['started'] for e in t)

G = {
 'G1 pr_merged, issue Done, not a stack layer': [r for r in pm if facts(r).get('issue_status') == 'done' and facts(r).get('layer_delivered') is False],
 'G4 checks_failed on a head a newer head replaced': [r for r in cf if stale(r)],
 'G6 handler run on entry to need_decision, mover = handler': [r for r in rows if r['cls'] == 'assignment/status-handoff' and mover_is_agent(r)],
 'G5 poll wakeup, issue unchanged since last run': [r for r in rows if r['cls'] in ('wakeup:time.due', 'wakeup:condition.met', 'wakeup:wakeup.timeout') and unchanged(r)],
 'G2 pr.event merged/ready/draft (closed excluded)': [r for r in pe if facts(r).get('kind') in ('merged', 'ready', 'draft')],
 'G3 pr.event head_changed (context only, not a gate)': [r for r in pe if facts(r).get('kind') == 'head_changed'],
}
print(f"{'gate':58}{'match':>6}{'saved':>6}{'fp':>5}{'saved$':>8}{'savedMtok':>10}{'fp$':>7}")
union = set()
for k, v in G.items():
    s = [r for r in v if nc(r)]; fp = [r for r in v if r['rc'] == 'substantive']
    print(f"{k:58}{len(v):6}{len(s):6}{len(fp):5}{sum(r['usd'] for r in s):8.0f}{sum(map(tok, s))/1e6:10.1f}{sum(r['usd'] for r in fp):7.0f}")
    if not k.startswith('G3'): union |= {r['id'] for r in s}
u = [r for r in rows if r['id'] in union]
print('union (G1,G2,G4,G5,G6) saved', len(u), 'runs', round(sum(r['usd'] for r in u)), 'usd', round(sum(map(tok, u)) / 1e6, 1), 'Mtok')
json.dump({k: [r['id'] for r in v if r['rc'] == 'substantive'] for k, v in G.items()}, open('fp_ids.json', 'w'))
