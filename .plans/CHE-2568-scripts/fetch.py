"""Acquire the CHE-2568 inputs into the current directory.

Writes tasks.json, tl/<issue_id>.json (full timeline: activity + comments)
and manifest.json (sha256 of every input, plus timeline truncation warnings).
Run from an empty scratch directory. Window is fixed, so reruns agree.
"""
import hashlib, json, os, re, subprocess, sys
from concurrent.futures import ThreadPoolExecutor

AGENTS = {'sonnet': '75eed3bd-33c6-42f5-b612-cf0f03b8879a', 'tofu': '4e6b035e-435e-46df-98d1-235cd2854fda'}
LO, HI = '2026-10-25T00:00:00Z', '2026-11-01T15:00:00Z'  # created_at, [LO, HI)


def run(cmd):
    p = subprocess.run(cmd, capture_output=True, text=True)
    if p.returncode:
        sys.exit(f'{" ".join(cmd)}: {p.stderr[:300]}')
    return p


def fetch_tasks():
    out = {}
    for name, agent in AGENTS.items():
        rows, before = [], None
        while True:
            cmd = ['multica', 'agent', 'tasks', agent, '--limit', '200', '--output', 'json'] + (['--before', before] if before else [])
            p = run(cmd)
            page = json.loads(p.stdout)
            rows += page
            m = re.search(r'--before "([^"]+)"', p.stderr + p.stdout)
            if not page or page[-1]['created_at'] < LO or not m:
                break
            before = m.group(1)
        out[name] = [r for r in rows if LO <= r['created_at'] < HI]
        print(name, len(out[name]), file=sys.stderr)
    return out


def fetch_timeline(issue_id):
    p = run(['multica', 'issue', 'timeline', issue_id, '--output', 'json'])
    with open(f'tl/{issue_id}.json', 'w') as f:
        f.write(p.stdout)
    return issue_id, ('truncated' if 'truncat' in p.stderr.lower() else None)


def sha(path):
    return hashlib.sha256(open(path, 'rb').read()).hexdigest()


if __name__ == '__main__':
    os.makedirs('tl', exist_ok=True)
    tasks = fetch_tasks()
    json.dump(tasks, open('tasks.json', 'w'))
    issues = sorted({r['issue_id'] for rs in tasks.values() for r in rs if r.get('issue_id')})
    with ThreadPoolExecutor(8) as ex:
        warn = dict(ex.map(fetch_timeline, issues))
    manifest = {'window': [LO, HI], 'tasks_sha256': sha('tasks.json'),
                'timelines': {i: sha(f'tl/{i}.json') for i in issues},
                'truncated': sorted(i for i, w in warn.items() if w)}
    json.dump(manifest, open('manifest.json', 'w'), indent=1)
    print(len(issues), 'timelines;', len(manifest['truncated']), 'truncated', file=sys.stderr)
