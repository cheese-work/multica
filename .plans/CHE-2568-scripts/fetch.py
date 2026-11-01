import json,subprocess,sys
agents={'sonnet':'75eed3bd-33c6-42f5-b612-cf0f03b8879a','tofu':'4e6b035e-435e-46df-98d1-235cd2854fda'}
CUT='2026-10-25T00:00:00Z'
out={}
for n,a in agents.items():
    rows=[];before=None
    while True:
        cmd=['multica','agent','tasks',a,'--limit','200','--output','json']
        if before: cmd+=['--before',before]
        p=subprocess.run(cmd,capture_output=True,text=True)
        page=json.loads(p.stdout)
        rows+=page
        if not page or (page[-1].get('created_at') or '')<CUT: break
        import re
        m=re.search(r'--before "([^"]+)"',p.stderr+p.stdout)
        if not m: break
        before=m.group(1)
    out[n]=[r for r in rows if (r.get('created_at') or '')>=CUT]
    print(n,len(out[n]),file=sys.stderr)
json.dump(out,open('tasks.json','w'))
