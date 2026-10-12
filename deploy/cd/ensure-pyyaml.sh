#!/usr/bin/env bash
# Make PyYAML importable for workflow-path-filters.py.
#
# The path-filter reader parses workflows with a real YAML parser, so PyYAML is
# a hard dependency of the release-candidate evidence set. Runners do not all
# ship it, and a runner that lacks it used to surface as an unexplained
# "required check mobile is not success" at admission rather than as the
# missing dependency it is. Installing it here keeps that failure mode at the
# step that owns the dependency.
set -euo pipefail

if python3 -c 'import yaml' 2>/dev/null; then
  exit 0
fi

# A distro-managed interpreter refuses a plain install (PEP 668); --user keeps
# the install out of the system tree either way.
python3 -m pip install --quiet --user pyyaml \
  || python3 -m pip install --quiet --user --break-system-packages pyyaml

python3 -c 'import yaml'
