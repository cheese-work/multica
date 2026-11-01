import json,collections,statistics as st
rows=json.load(open('rows.json'))
def agg(rs):
    n=len(rs)
    if not n: return {}
    s=lambda k:sum(r[k] for r in rs)
    return dict(n=n,inp=s('in'),cw=s('cw'),cr=s('cr'),out=s('out'),usd=round(s('usd'),2),cw_run=round(s('cw')/n),usd_run=round(s('usd')/n,4),cr_run=round(s('cr')/n))
for w in ('before','after'):
    rs=[r for r in rows if r['w']==w]
    print(w,'ALL',agg(rs))
    for a in sorted({r['agent'] for r in rows}):
        print('   ',a,agg([r for r in rs if r['agent']==a]))
# file coverage
c=collections.Counter((r['w'], r['fc'] if isinstance(r['fc'],str) or r['fc'] is None else 'ok') for r in rows); print(c)
# resume classes
def cls(r):
    if not r['has_prior']: return 'first-on-issue'
    st_='stale' if r['idle_min']>=55 else 'warm'
    return st_+('-resumed' if r['resumed'] else '-fresh')
for w in ('before','after'):
    rs=[r for r in rows if r['w']==w]
    cc=collections.defaultdict(list)
    for r in rs: cc[cls(r)].append(r)
    print('==',w)
    for k,v in sorted(cc.items()):
        fw=[r['fc']['fw'] for r in v if isinstance(r['fc'],dict)]
        print(f"  {k:22} runs={len(v):5} cw/run={sum(r['cw'] for r in v)/len(v):9.0f} usd/run={sum(r['usd'] for r in v)/len(v):.3f} firstcall_w mean={st.mean(fw) if fw else 0:8.0f} median={st.median(fw) if fw else 0:8.0f} n={len(fw)} cwshare={sum(r['cw'] for r in v)/sum(r['cw'] for r in rs):.2%}")
