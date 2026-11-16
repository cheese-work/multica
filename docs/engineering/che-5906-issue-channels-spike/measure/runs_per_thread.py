import json, os, collections, datetime as dt, statistics
runs = json.load(open('data/comment_runs.json'))
threads = {}  # comment id -> (root id, author_type)
for fn in os.listdir('data/comments'):
    cs = json.load(open('data/comments/' + fn)); by = {c['id']: c for c in cs}
    def root(c):
        n = 0
        while c.get('parent_id') and c['parent_id'] in by and n < 100:
            c = by[c['parent_id']]; n += 1
        return c['id']
    for c in cs: threads[c['id']] = (root(c), c['author_type'])
miss = 0
per = collections.defaultdict(list); trig_type = collections.Counter()
for iid, ref, ts, st, kind in runs:
    t = threads.get(ref)
    if not t: miss += 1; continue
    per[t[0]].append((dt.datetime.fromisoformat(ts.replace('Z', '+00:00')), t[1])); trig_type[t[1]] += 1
def pct(v, p):
    v = sorted(v); k = (len(v) - 1) * p; f = int(k); c = min(f + 1, len(v) - 1)
    return v[f] + (v[c] - v[f]) * (k - f)
def stats(name, v):
    print(f'{name}: n={len(v)} p50={pct(v,.5):.1f} p90={pct(v,.9):.1f} p95={pct(v,.95):.1f} p99={pct(v,.99):.1f} max={max(v)} mean={statistics.mean(v):.2f}')
tot7 = [len(v) for v in per.values()]
def max24(v):
    ts = sorted(x[0] for x in v); best = 0; j = 0
    for i in range(len(ts)):
        while ts[i] - ts[j] >= dt.timedelta(hours=24): j += 1
        best = max(best, i - j + 1)
    return best
m24 = [max24(v) for v in per.values()]
print('runs with comment trigger', len(runs), 'unmapped', miss, 'threads', len(per), 'trigger author type', dict(trig_type))
stats('runs per thread, 7 days', tot7)
stats('max runs per thread in any rolling 24h', m24)
# threads with member-authored triggers only
mem = [len([x for x in v if x[1] == 'member']) for v in per.values()]
mem = [x for x in mem if x]
if mem: stats('member-authored trigger runs per thread (7d, threads with >=1)', mem)
ag = [len([x for x in v if x[1] == 'agent']) for v in per.values()]; ag = [x for x in ag if x]
stats('agent-authored trigger runs per thread (7d, threads with >=1)', ag)
top = sorted(per.items(), key=lambda kv: -len(kv[1]))[:5]
print('top threads', [(k[:8], len(v), max24(v)) for k, v in top])
for M in (3,4,5,6,8,10,15,20,25,30):
    print(f'M={M}: threads over M in 24h = {sum(1 for x in m24 if x>M)} of {len(m24)}')
