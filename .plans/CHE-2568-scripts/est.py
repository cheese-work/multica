import json,collections,re,os
from datetime import datetime
rows=json.load(open('rows2.json'))
def ts(s): return datetime.fromisoformat(s.replace('Z','+00:00'))
NOOP=re.compile(r"(no action|no follow-?up|nothing (has )?(changed|to do|needed|new)|took no action|made no (change|changes)|changed nothing|i did nothing|did nothing|no change|posted no comment|posted nothing|still (open|waiting|pending|unanswered|draft|blocked)|is stale|false alarm|already (done|answered|landed)|no new (answer|comment|reply)|not ready yet|stays? (`|in )|left (it|everything) )",re.I)
ACT=re.compile(r"(i (re-?ran|pushed|merged|fixed|opened|sent|created|promoted|closed|cancelled|moved|set a|renewed|resolved|started|edited|applied|posted a)|has been (merged|moved))",re.I)
byiss=collections.defaultdict(list)
for r in rows:
    if r['issue']: byiss[r['issue']].append(r)
for v in byiss.values(): v.sort(key=lambda r:r['started'])
def unchanged(r):
    f=f"tl/{r['issue']}.json"
    if not os.path.exists(f): return None
    prev=[p for p in byiss[r['issue']] if p['agent']==r['agent'] and p['done']<=r['started'] and p['id']!=r['id']]
    if not prev: return None
    p=max(prev,key=lambda p:p['done'])
    t=json.load(open(f)); a,b=ts(p['done']),ts(r['started'])
    return not any(e['action'] not in('task_completed','task_failed') and a<ts(e['created_at'])<=b for e in t)
agg=collections.defaultdict(lambda:dict(n=0,out=0,inert=0,noop=0,unch=0,unch_known=0,usd=0,usd_noop=0,usd_unch=0,tok=0,tn=0))
for r in rows:
    k=r['cls']; a=agg[k]; a['n']+=1; a['usd']+=r['usd']
    if r['out']: a['out']+=1; continue
    if r['state'] is False:
        a['inert']+=1
        t=r['text']
        if NOOP.search(t) and not ACT.search(t): a['noop']+=1; a['usd_noop']+=r['usd']; a['tok']+=r['cw']+r['cr']+r['i']+r['o']; a['tn']+=1
        u=unchanged(r)
        if u is not None:
            a['unch_known']+=1
            if u: a['unch']+=1; a['usd_unch']+=r['usd']
print(f"{'class':42}{'n':>5}{'out':>5}{'inert':>6}{'noop':>5}{'$noop':>7}{'unch/known':>11}{'$unch':>7}{'avgTokNoop':>11}")
tot=dict(n=0,inert=0,noop=0,usd=0,usd_noop=0)
for k,a in sorted(agg.items(),key=lambda x:-x[1]['usd_noop']):
    print(f"{k:42}{a['n']:5}{a['out']:5}{a['inert']:6}{a['noop']:5}{a['usd_noop']:7.0f}{str(a['unch'])+'/'+str(a['unch_known']):>11}{a['usd_unch']:7.0f}{(a['tok']//max(a['tn'],1)):11}")
    for x in tot: tot[x]+=a[x]
print(tot)
