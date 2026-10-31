"""Offline behavior of optional operator-owned vote-gate env file."""
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class VoteGateEnvTest(unittest.TestCase):
    def test_packet_registry_excludes_unattested_hermes_route(self):
        packet = (ROOT / 'deploy/cd/c00-vote-gate-draft.md').read_text()
        registry = json.loads(re.search(r'```json\n(.*?)\n```', packet, re.S).group(1))
        self.assertEqual(registry, {
            'claude/claude-sonnet-5-5[1m]': 'anthropic',
            'codex/gpt-6.1-sol': 'openai',
        })

    def render(self, content=None):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            shutil.copyfile(ROOT / 'docker-compose.selfhost.yml', root / 'docker-compose.selfhost.yml')
            (root / 'deploy/cd').mkdir(parents=True)
            shutil.copyfile(ROOT / 'deploy/cd/docker-compose.ab.yml', root / 'deploy/cd/docker-compose.ab.yml')
            if content is not None:
                (root / '.env.vote-gate').write_text(content)
            env = {'PATH': os.environ['PATH'], 'HOME': tmp, 'JWT_SECRET': 'offline-test-only',
                   'MULTICA_SKIP_MIGRATIONS': '1'}
            result = subprocess.run(['docker', 'compose', '-f', 'docker-compose.selfhost.yml',
                                     '-f', 'deploy/cd/docker-compose.ab.yml', 'config', '--format', 'json'],
                                    cwd=root, env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            return json.loads(result.stdout)

    def test_absent_file_keeps_gate_disabled(self):
        config = self.render()
        for name in ('backend', 'backend-blue', 'backend-green'):
            env = config['services'][name]['environment']
            self.assertNotIn('MULTICA_VOTE_GATE_FIELDS', env)
            self.assertNotIn('MULTICA_VOTE_GATE_LABS', env)

    def test_operator_file_reaches_both_slots_without_other_changes(self):
        # Synthetic fixture only. These are not production registry mappings.
        fields = 'squad.instructions,agent.instructions,workspace.context'
        labs = '{"test/model":"testlab"}'
        content = f'MULTICA_VOTE_GATE_FIELDS={fields}\nMULTICA_VOTE_GATE_LABS={labs}\n'
        baseline, config = self.render(), self.render(content)
        for name in ('backend', 'backend-blue', 'backend-green'):
            env = config['services'][name]['environment']
            self.assertEqual(env.pop('MULTICA_VOTE_GATE_FIELDS', None), fields)
            self.assertEqual(env.pop('MULTICA_VOTE_GATE_LABS', None), labs)
            self.assertEqual(env, baseline['services'][name]['environment'])
        self.assertEqual(config, baseline)

    def test_dollar_values_are_literal(self):
        config = self.render('MULTICA_VOTE_GATE_LABS={"test/$MODEL":"testlab"}\n')
        # Compose's reusable config serialization escapes literal dollars.
        self.assertEqual(config['services']['backend-blue']['environment'].get('MULTICA_VOTE_GATE_LABS'),
                         '{"test/$$MODEL":"testlab"}')


if __name__ == '__main__':
    unittest.main()
