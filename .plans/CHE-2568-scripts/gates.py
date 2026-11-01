import json,re,collections,os
from datetime import datetime
rows=json.load(open('rows2.json')); S=set(json.load(open('strict_ids.json')))
d=json.load(open('tasks.json')); created={r['id']:r['created_at'] for k in d for r in d[k]}
def ts(s): return datetime.fromisoformat(s.replace('Z','+00:00'))
def tl(i):
    f=f"tl/{i}.json"; return json.load(open(f)) if os.path.exists(f) else None
def status_at(r,t=None):
    x=tl(r['issue']) if r['issue'] else None
    if x is None: return None
    t=t or r['started']
    ev=[e for e in x if e['action']=='status_changed' and ts(e['created_at'])<=ts(t)]
    return max(ev,key=lambda e:e['created_at'])['details'].get('to') if ev else None
def kind(r): return (re.search(r'"kind":"(\w+)"',r['note']) or [0,'?'])[1]
tokf=lambda r:r['cw']+r['cr']+r['i']+r['o']
def summ(name,rs):
    print(f"{name:58} runs={len(rs):4} Mtok={sum(map(tokf,rs))/1e6:6.1f} usd={sum(r['usd'] for r in rs):6.0f}")
inert=lambda r: not r['out'] and r['state'] is False
G={}
pm=[r for r in rows if r['cls'].startswith('wakeup:pr.merged') and not r['out']]
G['G1 pr_merged on issue already done']=[r for r in pm if status_at(r)=='done']
G['G1b pr_merged on issue in merged (server sets done)']=[r for r in pm if status_at(r)=='merged']
pe=[r for r in rows if r['cls'].startswith('wakeup:pr.event')]
G['G2 pr.event informational (merged/closed/ready/draft)']=[r for r in pe if kind(r) in('merged','closed','ready','draft') and not r['out']]
G['G3 pr.event head_changed']=[r for r in pe if kind(r)=='head_changed' and not r['out']]
cf=[r for r in rows if r['cls'].startswith('wakeup:pr.checks_failed')]
G['G4 checks_failed (all)']=[r for r in cf if not r['out']]
G['G4s checks_failed stale/old head per agent text']=[r for r in cf if re.search(r'(old head|stale|superseded|no longer the (current )?head|newer head|already (fixed|passing|green)|passes now|now passes|false alarm)',r['text'],re.I)]
poll=[r for r in rows if r['cls'] in('wakeup:time.due','wakeup:condition.met','wakeup:wakeup.timeout') and not r['out']]
G['G5 poll wakeups (time.due/condition.met/timeout), inert']=[r for r in poll if r['state'] is False]
nd=[]
for r in rows:
    if r['cls']=='assignment/status-handoff' and not r['out']:
        x=tl(r['issue'])
        if x is None: continue
        ev=[e for e in x if e['action']=='status_changed' and ts(e['created_at'])<=ts(created[r['id']])]
        if ev and max(ev,key=lambda e:e['created_at'])['details'].get('to')=='need_decision': nd.append(r)
G['G6 handler run on entry to need_decision']=nd
G['G6i  ... of which inert']=[r for r in nd if r['state'] is False]
for k,v in G.items(): summ(k,v); summ('    strict-noop subset',[r for r in v if r['id'] in S])
# overlap: pr.event merged vs pr_merged same issue within 10 min
pmk=collections.defaultdict(list)
for r in rows:
    if r['cls'].startswith('wakeup:pr.merged'): pmk[r['issue']].append(ts(r['started']))
ov=[r for r in G['G2 pr.event informational (merged/closed/ready/draft)'] if kind(r)=='merged' and any(abs((ts(r['started'])-t).total_seconds())<1800 for t in pmk.get(r['issue'],[]))]
summ('pr.event merged duplicating a pr_merged run (<30min)',ov)
summ('ALL completed runs',rows)
