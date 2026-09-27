# CHE-705 C02 verification evidence

Observed 2026-09-27 on X99 against the merged C01 base `43c938b46360f65eb214bce34f3af5af1e66d8f8`. E01 remains `REJECT`; decision `01a0de3c-6338-7306-9241-7a15e55f056b` waives its economic PASS, measurements, and N>=5 only for default-off development admission.

## Sol review corrections

Issue deletion now clears linked case `reason`, `trigger_aliases`, and `evidence_digest`. Source deletion scrubs attempt `usage` alongside result/confidence. The 30-day sweep clears attempt `usage`, case `frozen_strategy`, and transition `sanitized_reason`, while retaining obligation IDs and qualification revisions.

`InsertGovernanceEvaluationSource` now locks a live issue first, then a live non-tombstoned comment when applicable, before inserting its index row. This matches the issue-then-comment deletion lock order; a deleted or tombstoned source returns no row. The regression holds the writer transaction open, observes comment deletion blocked by that writer through `pg_blocking_pids`, commits it, and verifies the resulting evidence is redacted and unavailable to replay.

Observed passing on the isolated `_test` database (migrated through 536):

```sh
cd server
DATABASE_URL="$CHE705_TEST_DATABASE_URL" go test -p 1 ./internal/handler ./internal/scheduler ./internal/featureflags \
  -run 'Governance(CaseAudit|Evidence)|GovernanceCaseEvidenceRetention|GovernanceEvaluationSourceInsertSerializes' -count=1
DATABASE_URL="$CHE705_TEST_DATABASE_URL" go test -race -p 1 ./internal/handler ./internal/scheduler ./internal/featureflags \
  -run 'Governance(CaseAudit|Evidence)|GovernanceCaseEvidenceRetention|GovernanceEvaluationSourceInsertSerializes' -count=1
go vet ./cmd/server ./internal/featureflags ./internal/handler ./internal/scheduler
go test ./internal/governance/receipt -run '^TestObserve_BudgetExceededSheds$' -count=1
cd ..
make sqlc
git diff --check
```

`CHE705_TEST_DATABASE_URL` must point to the isolated local `_test` database; the recorded run used the test database migrated through 536. Both focused runs passed across 13 tests in three packages, including the deletion/writer interleaving under `-race`. SQLC regeneration, targeted vet, the 50ms observation-budget test, and whitespace validation passed. The 90-day retention and audit-access/ENG-07 evidence below remain unchanged.

## Source redaction and audit access

The focused handler tests cover default-off behavior, owner/admin enforcement, cross-workspace isolation, list/detail/export secret-sentinel omission, keyset continuation, transactional comment and issue deletion redaction, 30-day content scrubbing, 90-day terminal-case pruning, and preservation of active case/attempt and obligation references.

Observed passing commands:

```sh
ENV_FILE=.env.worktree make migrate-up
set -a; . ./.env.worktree; set +a
cd server && go test ./internal/handler ./internal/scheduler ./internal/featureflags \
  -run 'Governance(CaseAudit|Evidence)|GovernanceCaseEvidenceRetention' -count=1
```

The isolated `_test` database ran through migration 536. All three packages passed. A compile-only check also passed for `internal/handler`, `internal/scheduler`, `internal/featureflags`, and `cmd/server`.

## ENG-07

`go test ./internal/governance/receipt -run 'TestObserve_BudgetExceededSheds' -count=1` passed. It exercises the existing 50ms observation budget with a delayed fake provider; CHE-705 does not modify the observation path.

Representative `EXPLAIN (ANALYZE, BUFFERS)` probes ran inside a transaction against synthetic rows (1,000 cases; 20,080 attempts, evaluations, and transitions; 5,000 receipts). The transaction rolled back. Warm local-cache observations:

| Query | Selected index | Page | Execution |
| --- | --- | ---: | ---: |
| Workspace case listing | `idx_governance_case_workspace_created` | 25 | 0.056 ms, 3 shared hits |
| Deadline reconciliation | `idx_governance_attempt_workspace_deadline` | 100 | 0.090 ms, 26 shared hits |
| Attempt audit keyset | `idx_governance_attempt_workspace_case_created` | 51 | 0.055 ms, 4 shared hits |
| Evaluation audit keyset | `idx_governance_evaluation_workspace_case_captured` | 51 | 0.057 ms, 5 shared hits |
| Transition audit keyset | `idx_governance_case_transition_workspace_case_created` | 51 | 0.056 ms, 5 shared hits |
| Observation audit page | `idx_governance_receipt_workspace_comment_created` | 50 | 0.052 ms, 3 shared hits |

The audit list uses one bounded query. Case detail/export use a fixed four reads (case plus one page each for transitions, attempts, and evaluations); row mapping issues no additional queries. Each detail list fetches at most `limit + 1` rows and returns a continuation cursor when truncated. The pagination integration test verifies distinct first/second pages at `limit=1`.

The current C01 schema has no governance action table or budget-ledger relation, so action-claim and ledger index plans are not applicable to this slice. Existing workspace-leading C01 indexes cover case listing, deadline reconciliation, and evaluation/source lookup; C02 adds case-detail, retention, and observation-audit indexes.

## Overhead and limits

Audit read routes fail closed behind the default-off `governance_case_audit_enabled` flag. C02 adds no work to comment create/edit observation. Source redaction adds a set-based query only to comment/issue deletion transactions so deletion/access revocation scrubs derived evidence atomically. No base-versus-head p95 benchmark was run; the synthetic EXPLAIN timings are not p95 claims. Measure deletion-path and comment-path p95 before any shadow promotion; no shadow promotion is part of this candidate.

No live provider/key/spend, correction, deployment, publication, merge, or destructive retirement was performed.
