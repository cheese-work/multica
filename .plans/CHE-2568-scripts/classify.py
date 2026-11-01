import json,collections,re,os
from lib import run_class
from datetime import datetime
d=json.load(open('tasks.json'))
P={'opus':(4,8,0.2,20),'sonnet':(2,4,0.2,10)}
def ts(s): return datetime.fromisoformat(s.replace('Z','+00:00'))
def usd(r):
    t=0
    for u in r.get('usage') or []:
        k='opus' if 'opus' in u['model'] else 'sonnet'; i,w,rd,o=P[k]
        t+=(u['input_tokens']*i+u['cache_write_tokens']*w+u['cache_read_tokens']*rd+u['output_tokens']*o)/1e6
    return t
def toks(r):
    u=r.get('usage') or []
    return dict(cw=sum(x['cache_write_tokens'] for x in u),cr=sum(x['cache_read_tokens'] for x in u),
                i=sum(x['input_tokens'] for x in u),o=sum(x['output_tokens'] for x in u))
TL={}
def tl(i):
    if i not in TL:
        f=f'tl/{i}.json'
        TL[i]=json.load(open(f)) if os.path.exists(f) else None
    return TL[i]
def cls(r):
    ev=(r.get('attribution') or {}).get('evidence',{}).get('kind')
    h=r.get('handoff_note') or ''
    if ev=='issue_wakeup':
        m=re.search(r'Trigger facts[^\n]*\n(\S+)',h)
        f=m.group(1) if m else 'nofacts'
        return 'wakeup:'+f+('/'+r['wakeup_system_rule'] if r.get('wakeup_system_rule') else '')
    if ev=='issue_assignment': return 'assignment/status-handoff'
    if ev=='comment': return 'comment'
    return ev or 'other'
rows=[]
for n,rs in d.items():
    for r in rs:
        if r['status']!='completed' or not r.get('completed_at'): continue
        t=toks(r)
        rc=run_class(r,tl(r['issue_id']) if r.get('issue_id') else None)
        rows.append(dict(agent=n,agent_id=r['agent_id'],created=r['created_at'],id=r['id'],issue=r.get('issue_id'),cls=cls(r),rc=rc,usd=usd(r),**t,
          kind=r['kind'],started=r['started_at'],done=r['completed_at'],
          text=((r.get('result') or {}).get('output') or '')[:3000], note=(r.get('handoff_note') or '')[:4000],
          wk=r.get('wakeup_id'),rule=r.get('wakeup_system_rule')))
json.dump(rows,open('rows2.json','w'))
print(len(rows))
