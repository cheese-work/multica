import argparse
import json
import os
import re
import subprocess
import sys


def plan_deletions(caches, default_ref='refs/heads/main', target=9_000_000_000, limit=10_000_000_000):
    newest = {}
    deleted = []
    for cache in sorted(caches, key=lambda entry: (entry['created_at'], entry['id']), reverse=True):
        family = cache['key']
        managed = family.startswith(('turbo-', 'go-build-race-', 'release-go-', 'golangci-lint.cache-'))
        if managed:
            family = family.split('-refs/', 1)[0]
            family = re.sub(r'-(?:[a-f0-9]{40}|[a-f0-9]{64})$', '', family)
        group = (cache['ref'], family, None if managed else cache.get('version'))
        if group in newest:
            deleted.append(cache['id'])
        else:
            newest[group] = cache
    protected = {cache['id'] for cache in newest.values() if cache['ref'] == default_ref}
    required = sum(cache['size_in_bytes'] for cache in newest.values() if cache['id'] in protected)
    if required > limit:
        raise ValueError(f'Required default-branch caches need {required} bytes; free limit is {limit} bytes. Stop; no paid storage.')
    remaining = list(newest.values())
    size = sum(cache['size_in_bytes'] for cache in remaining)
    for cache in sorted(remaining, key=lambda entry: (entry['last_accessed_at'], entry['id'])):
        if size <= max(target, required):
            break
        if cache['id'] not in protected:
            deleted.append(cache['id'])
            size -= cache['size_in_bytes']
    return sorted(deleted)


def api(endpoint):
    return json.loads(subprocess.check_output(['gh', 'api', endpoint], text=True))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--repo', default=os.environ.get('GITHUB_REPOSITORY'))
    parser.add_argument('--dry-run', action='store_true')
    arguments = parser.parse_args()
    if not arguments.repo or not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', arguments.repo):
        parser.error('--repo must name owner/repository')
    repository = arguments.repo
    default_ref = 'refs/heads/' + api(f'repos/{repository}')['default_branch']
    caches = []
    page = 1
    while True:
        batch = api(f'repos/{repository}/actions/caches?per_page=100&page={page}')['actions_caches']
        caches.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    deletions = plan_deletions(caches, default_ref)
    deleted_bytes = sum(cache['size_in_bytes'] for cache in caches if cache['id'] in deletions)
    print(json.dumps({'repository': repository, 'dry_run': arguments.dry_run,
                      'before_bytes': sum(cache['size_in_bytes'] for cache in caches),
                      'delete_ids': deletions, 'delete_bytes': deleted_bytes,
                      'projected_bytes': sum(cache['size_in_bytes'] for cache in caches) - deleted_bytes}))
    if not arguments.dry_run:
        for identifier in deletions:
            try:
                subprocess.run(['gh', 'cache', 'delete', str(identifier), '--repo', repository],
                               check=True, capture_output=True, text=True)
            except subprocess.CalledProcessError as error:
                if not re.search(r'\bHTTP 404\b', error.stderr or ''):
                    raise
                print(f'Cache {identifier} is already absent (HTTP 404); continuing.', file=sys.stderr)
        print(json.dumps(api(f'repos/{repository}/actions/cache/usage')))


if __name__ == '__main__':
    main()
