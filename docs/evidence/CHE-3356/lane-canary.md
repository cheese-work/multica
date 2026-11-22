# CHE-3356 Tier 0/1 merge-lane canary

This file exists so one docs-only pull request can go through the agent merge lane (CHE-3244, CHE-4370, CHE-3123) end to end. Cheese chose to author the canary on CHE-3356 (19 Nov 2026).

## What the canary proves

1. The `mechanical-tier` job writes a receipt for the exact head, and `mechanical_tier.py verify` accepts it as Tier 0 or Tier 1.
2. Every required check is green on that head.
3. A signed cross-lab PASS exists for that head.
4. A merger who is not the author merges through the merge queue, and the CHE-3266 digest lists the merge.

## What it changes

Nothing at run time. The file is a record only. No code, config or workflow path changes.
