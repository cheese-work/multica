# Upstream merge review scope

Rule for a PR whose change is `git merge` of `multica-ai/multica` (tag or `main`) into `cheese-work/multica` (CHE-5577, plan CHE-5574 step 2). It replaces `git diff <auto-merge-result> HEAD`, which has no reproducible baseline. It does not change the standing test-first waiver (Cheese, 14 Nov 2026 02:45 ICT, D2 = A); it defines what the reviewer looks at.

## Run

On X99, in a clone that holds `P`, `U` and `M`:

```bash
scripts/merge-review-pack --fork-parent <P> --upstream-parent <U> --merge <M> \
  --upstream-ref <tag-or-main> --github-repo cheese-work/multica
```

Paste the output, or a link to it, in the review. The script is read-only apart from a scratch bare clone for provenance and the tree objects `git merge-tree` writes.

| Exit | Meaning | Reviewer action |
| --- | --- | --- |
| 0 | Pack printed | Review it |
| 2 | Pin rejected: not a 40-hex SHA, unknown commit, or `M` has parents other than exactly `P` and `U` | Reject the head |
| 3 | No trustworthy baseline (`git merge-tree --write-tree` failed, was unstable across two runs, or `P` and `U` share no merge base) | Stop. No waiver review without `B` |
| 4 | Provenance failed: `U` is not the fetched tag commit, or not an ancestor of the fetched branch | Reject the head |
| 5 | Check runs on `M` missing, not green, or unreadable | Wait or fix; the pack is still printed |
| 64 | Usage error | Fix the call |

## What the rule fixes

1. **Pin.** `P` (fork parent), `U` (upstream parent) and `M` (merge result) are full SHAs in the PR body. In a stack, `P` is the previous layer head. Any head whose `M` has other parents is rejected.
2. **Provenance.** With `--upstream-ref`, the script fetches the ref into a scratch bare repo. A tag must equal `U`. A branch must contain `U` as an ancestor. `ls-remote` is not proof: it lists tips only.
3. **Baseline `B`** = `git merge-tree --write-tree --name-only P U`. Deterministic; the script runs it twice and stops if the exit status or output differs. A conflict gives a tree with markers. A non-text conflict (modify/delete, rename, file/directory, mode, submodule, binary) has no markers: `B` holds one side of that path, and the pack flags it `NON-TEXT CONFLICT`. Review such a path whole, not as a diff against `B`.
4. **Resolution edits** = `git diff B M`, read line by line. This is the only part the waiver covers. Touching only conflicted files is a necessary check, not proof (fork code can sit inside a conflicted file). The pack tags every edited path `in conflicted file` or `OUTSIDE CONFLICT`. The reviewer affirms in the verdict that each edit resolves the merge. An edit that does not, or that the reviewer cannot classify, fails the waiver closed, and the PR needs observed red→green.
5. **Incoming delta** = `git diff $(git merge-base P U) U`, reviewed for behavioural and security impact even when the merge is clean. The pack lists every changed file, plus new endpoints (added route registrations), new config or permission, and new dependencies. The list is a lead, not a proof: the patterns are line greps, so read the file list, not only the classes.
6. **Minimum class checks**, never the limit. An unclassified change that affects the fork is still a finding.
   - Migrations (`server/migrations`): number collision between `U` and `P` is reported as `COLLISION (U vs P)`; the same number still used by two stems in `M` is `COLLISION IN M (unresolved)`. A collision needs the CHE-3455 renumber and the migration tests.
   - Workflow and CI files (`.github`).
   - Trust-boundary paths: auth, tokens, secrets, permissions, sessions, middleware, daemon exec and sandbox.
   - Fork overlap: files both sides changed. The reviewer runs the fork's existing tests for each listed directory. A clean merge can still break fork behaviour.
7. **Checks on the exact head `M`.** All required check runs green (the script reads the check runs of `M` with `gh`; compare with the required list), CI job `migration-prefix-check` green, fork tests for every overlap directory. Cross-lab PASS as usual.

## Path ledger coverage check

CHE-6210 approach A writes one ledger row per path before the merge. `scripts/merge-ledger-check` proves the ledger covers the merge. It fails closed. Run it on X99 in a clone that holds `P` and `U`; A4, A5e and A6 run it on the exact head:

```bash
scripts/merge-ledger-check --ledger <ledger.tsv> --fork-parent <P> --upstream-parent <U>
```

Ledger: tab-separated, a header row, then 8 columns: `path group change conflict fork_touched fork_features disposition evidence`. `disposition` is one of `resolved`, `clean-both`, `upstream-only`, `fork-kept`, `regenerated`, `deleted`.

Computed set: every path in `git diff --no-renames --name-status <merge-base> U` (a rename is its old and its new path) plus every conflicted path of `git merge-tree --write-tree P U`. Fork-touched: the path is in `git diff --name-only <merge-base> P`. FORK-FEATURES names a path when `docs/fork/FORK-FEATURES.md` at `P` contains it as text (a substring match, so it can over-require evidence, never under-require). Git and that file decide; the ledger's own `fork_touched` and `fork_features` columns are not trusted.

| Exit | Meaning |
| --- | --- |
| 0 | Ledger path set equals the computed set, every row has an allowed disposition, every fork-touched or FORK-FEATURES path has evidence |
| 2 | Pin rejected (not a 40-hex SHA, or unknown commit) |
| 3 | No trustworthy computed set (no merge base, `git merge-tree` failed, a path needs quoting, FORK-FEATURES missing at `P`) |
| 6 | Path set differs: a missing, extra or duplicate row |
| 7 | Bad header or column count, or a row with no or an unknown disposition |
| 8 | A fork-touched or FORK-FEATURES path has empty or placeholder evidence (`-`, `n/a`, `none`, `todo`, `tbd`) |
| 64 | Usage error or unreadable ledger |

Every fault is printed. The exit code is the lowest of 6, 7 and 8 that applies. The check proves coverage, not truth: a reviewer still reads the evidence. It does not use `M`; a merge-up head with new pins needs a new ledger run.

## Limits

- The script reports; it does not decide. The waiver verdict is the reviewer's.
- Class patterns are heuristics (path names and added-line greps). A miss is a script gap, not a pass: fix the pattern in `scripts/merge-review-pack` and its test.
- Needs git 2.38 or newer (`git merge-tree --write-tree`). Older git exits 3.
