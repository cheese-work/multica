import json,collections,re,os,statistics as st
from datetime import datetime,timedelta
d=json.load(open('tasks.json'))
T='2026-10-30T07:54:27Z';B0='2026-10-28T07:54:27Z';A1='2026-11-01T07:54:27Z'
P={'opus':(4,8,0.2,20),'sonnet':(2,4,0.2,10)}
def ts(s): return datetime.fromisoformat(s.replace('Z','+00:00'))
def price(u):
    k='opus' if 'opus' in u['model'] else 'sonnet'
    i,w,r,o=P[k]
    return (u['input_tokens']*i+u['cache_write_tokens']*w+u['cache_read_tokens']*r+u['output_tokens']*o)/1e6
def enc(p): return re.sub(r'[^A-Za-z0-9]','-',p)
H=os.path.expanduser('~/.claude/projects')
_cache={}
def first_call(r):
    res=r.get('result') or {}
    sid=res.get('session_id'); wd=res.get('work_dir')
    if not sid or not wd: return None
    f=os.path.join(H,enc(wd),sid+'.jsonl')
    if not os.path.exists(f): return 'nofile'
    st0=ts(r['started_at'])-timedelta(seconds=5)
    en=ts(r['completed_at'])+timedelta(seconds=5) if r.get('completed_at') else None
    seen=set();calls=[]
    for line in open(f):
        try: e=json.loads(line)
        except: continue
        if e.get('type')!='assistant': continue
        m=e.get('message') or {}; u=m.get('usage'); mid=m.get('id')
        if not u or not mid or mid in seen: continue
        t=ts(e['timestamp'])
        if t<st0 or (en and t>en): continue
        seen.add(mid); calls.append((t,u))
    if not calls: return 'nocalls'
    calls.sort(key=lambda x:x[0])
    u=calls[0][1]
    tot={k:sum(c[1].get(k,0) or 0 for c in calls) for k in ('input_tokens','cache_creation_input_tokens','cache_read_input_tokens','output_tokens')}
    return {'fw':u.get('cache_creation_input_tokens',0),'fr':u.get('cache_read_input_tokens',0),'ncalls':len(calls),'tot':tot}
rows=[]
for n,rs in d.items():
    done=[r for r in rs if r.get('completed_at') and r.get('started_at')]
    byissue=collections.defaultdict(list)
    for r in done:
        if r.get('issue_id'): byissue[r['issue_id']].append(r)
    for r in rs:
        s=r.get('started_at') or ''
        w='before' if B0<=s<T else 'after' if T<=s<A1 else None
        if not w or not r.get('usage'): continue
        u=r['usage']
        rec={'agent':n,'w':w,'id':r['id'],'kind':r.get('kind'),'issue':r.get('issue_id'),
             'in':sum(x['input_tokens'] for x in u),'cw':sum(x['cache_write_tokens'] for x in u),
             'cr':sum(x['cache_read_tokens'] for x in u),'out':sum(x['output_tokens'] for x in u),
             'usd':sum(price(x) for x in u),'pr':(r.get('result') or {}).get('pr_url') or '',
             'comments':len(r.get('delivered_comment_ids') or []),'sid':(r.get('result') or {}).get('session_id'),
             'trigger':r.get('trigger_summary') or ''}
        prior=None
        if r.get('issue_id'):
            c=[p for p in byissue[r['issue_id']] if p['id']!=r['id'] and p['completed_at']<=r['started_at']]
            if c: prior=max(c,key=lambda p:p['completed_at'])
        rec['has_prior']=bool(prior)
        if prior:
            rec['idle_min']=(ts(r['started_at'])-ts(prior['completed_at'])).total_seconds()/60
            rec['resumed']=rec['sid'] is not None and rec['sid']==(prior.get('result') or {}).get('session_id')
        rec['fc']=first_call(r)
        rows.append(rec)
json.dump(rows,open('rows.json','w'))
print(len(rows))
