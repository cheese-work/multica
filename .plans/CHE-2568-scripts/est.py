"""Per trigger class: how many runs changed nothing (ceremonial or silent), and what they cost.
Run in the fetch directory. Writes nochange_ids.json (consumed by gates.py)."""
import json, collections
rows = json.load(open('rows2.json'))
tok = lambda r: r['cw'] + r['cr'] + r['i'] + r['o']
nc = lambda r: r['rc'] in ('ceremonial', 'silent')
agg = collections.defaultdict(lambda: collections.Counter())
for r in rows:
    a = agg[r['cls']]
    a['n'] += 1; a['usd'] += r['usd']; a[r['rc']] += 1
    if nc(r): a['nc_usd'] += r['usd']; a['nc_tok'] += tok(r)
print(f"{'class':40}{'n':>5}{'subst':>6}{'cerem':>6}{'silent':>7}{'unk':>5}{'nc$':>7}{'ncMtok':>8}")
for k, a in sorted(agg.items(), key=lambda x: -x[1]['nc_usd']):
    print(f"{k:40}{a['n']:5}{a['substantive']:6}{a['ceremonial']:6}{a['silent']:7}{a['unknown']:5}{a['nc_usd']:7.0f}{a['nc_tok']/1e6:8.1f}")
c = collections.Counter(r['rc'] for r in rows)
print(dict(c), 'total usd', round(sum(r['usd'] for r in rows)), 'nochange usd', round(sum(r['usd'] for r in rows if nc(r))),
      'nochange Mtok', round(sum(tok(r) for r in rows if nc(r)) / 1e6, 1), 'total Mtok', round(sum(tok(r) for r in rows) / 1e6, 1))
json.dump([r['id'] for r in rows if nc(r)], open('nochange_ids.json', 'w'))
