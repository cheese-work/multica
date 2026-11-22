#!/usr/bin/env python3
"""Fail unless the CD backend binaries on the runner are the ones for this commit (CHE-7209).

Runs before the image build. The daemon-update rule of LoadDaemonUpdateSource
(server/internal/handler/daemon_update.go) decides what the server serves, so
the same rule is checked here: a bad payload otherwise makes the server log
"serving no daemon binaries" and answer 404 while health stays green.

Usage: check-backend-payloads.py --dir D --commit SHA --version V --build B
Env:   GO   the go command (default "go")
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys

SERVER_BINARIES = ['server', 'multica', 'migrate', 'maintenance',
                   'backfill_task_usage_hourly', 'backfill_codex_usage_cache',
                   'backfill_issue_created_via']
DAEMON_KEYS = ['linux-amd64', 'linux-arm64', 'darwin-arm64', 'darwin-amd64']
MANIFEST = 'daemon-binaries/manifest.json'


def go_build_info(go, path):
    result = subprocess.run([go, 'version', '-m', str(path)], capture_output=True, text=True)
    if result.returncode != 0:
        raise ValueError(f'{path.name}: go version -m failed: {result.stderr.strip()}')
    settings = {}
    for line in result.stdout.splitlines():
        parts = line.strip().split('\t', 1)
        if len(parts) == 2 and parts[0] == 'build' and '=' in parts[1]:
            key, value = parts[1].split('=', 1)
            settings[key] = value
    return settings


def ldflags_of(settings):
    raw = settings.get('-ldflags')
    if raw is None:
        return None
    if raw.startswith('"'):
        try:
            raw = json.loads(raw)
        except ValueError:
            raw = raw.strip('"')
    return raw


def main_stamps(ldflags):
    stamps = {}
    tokens = shlex.split(ldflags)
    for index, token in enumerate(tokens):
        if token == '-X' and index + 1 < len(tokens):
            name, _, value = tokens[index + 1].partition('=')
            stamps[name] = value
    return stamps


def check(directory, commit, version, build, go):
    errors = []
    directory = Path(directory)
    try:
        manifest = json.loads((directory / MANIFEST).read_text())
    except (OSError, ValueError) as error:
        return [f'{MANIFEST}: cannot read: {error}']
    if manifest.get('commit') != commit:
        errors.append(f'manifest commit {manifest.get("commit")!r} is not the build commit {commit!r}')
    if manifest.get('version') != f'{version}-{build}':
        errors.append(f'manifest version {manifest.get("version")!r} is not {version}-{build}')
    listed = manifest.get('binaries') or {}
    for key in DAEMON_KEYS:
        name = f'multica-{key}'
        entry = listed.get(key)
        if entry is None:
            errors.append(f'manifest does not list {key}')
            continue
        path = directory / 'daemon-binaries' / name
        if not path.is_file():
            errors.append(f'{name}: payload file is missing')
            continue
        data = path.read_bytes()
        if len(data) != entry.get('size'):
            errors.append(f'{name}: size {len(data)} does not match the manifest size {entry.get("size")}')
        if hashlib.sha256(data).hexdigest() != entry.get('sha256'):
            errors.append(f'{name}: sha256 does not match the manifest')
    for key in sorted(set(listed) - set(DAEMON_KEYS)):
        errors.append(f'manifest lists unexpected payload {key}')
    expected = {MANIFEST, *SERVER_BINARIES, *(f'daemon-binaries/multica-{key}' for key in DAEMON_KEYS)}
    found = {str(p.relative_to(directory)) for p in directory.rglob('*') if p.is_file() and p.name != 'SHA256SUMS'}
    for extra in sorted(found - expected):
        errors.append(f'unexpected file {extra}')

    files = [*SERVER_BINARIES, *(f'daemon-binaries/multica-{key}' for key in DAEMON_KEYS)]
    for rel in files:
        path = directory / rel
        if not path.is_file():
            errors.append(f'{rel}: binary is missing')
            continue
        try:
            settings = go_build_info(go, path)
        except ValueError as error:
            errors.append(str(error))
            continue
        if settings.get('vcs.revision') != commit:
            errors.append(f'{rel}: vcs.revision {settings.get("vcs.revision")!r} is not {commit!r}')
        if settings.get('vcs.modified') != 'false':
            errors.append(f'{rel}: vcs.modified is {settings.get("vcs.modified")!r}, not false')
        if rel == 'server':
            ldflags = ldflags_of(settings)
            stamps = main_stamps(ldflags) if ldflags is not None else {}
            for name, want in [('main.version', version), ('main.build', build), ('main.commit', commit)]:
                if stamps.get(name) != want:
                    errors.append(f'server: {name} stamp is {stamps.get(name)!r}, expected {want!r}')
    return errors


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--dir', required=True)
    parser.add_argument('--commit', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--build', required=True)
    args = parser.parse_args()
    if not re.fullmatch(r'[0-9a-f]{40}', args.commit):
        print(f'check-backend-payloads: commit {args.commit!r} is not a full SHA', file=sys.stderr)
        return 1
    errors = check(args.dir, args.commit, args.version, args.build, os.environ.get('GO', 'go'))
    for error in errors:
        print(f'check-backend-payloads: {error}', file=sys.stderr)
    if errors:
        return 1
    print(f'check-backend-payloads: all backend payloads match commit {args.commit}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
