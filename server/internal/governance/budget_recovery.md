# Budget recovery and broker contract (H2)

Proven by `TestBudgetRecovery*` (CHE-883). C07 source admission consumes this
contract; it does not wait for CHE-729 to close.

## Broker contract

1. **Admit** — `AdmissionService.Admit` commits case, attempt, obligation,
   stable slot, budget reservation, journal and one `admit` outbox row in a
   single transaction. Nothing is dispatched inside it.
2. **Native admission key** — the broker keys admission on
   `governance_budget_outbox.obligation_id`. Redelivery of a `pending` or
   `claimed` row reuses that key, so the broker admits at most once. A retry of
   `Admit` with the same input returns `Duplicate` and the same outbox event id.
3. **Wire pre-debit** — `Debit` has one role: a worst-case pre-debit for new
   wire work, persisted under a unique `EventKey` *before* the broker sends.
   The same key never debits twice. The first pre-debit rides the attempt
   reserved at admission; each further distinct key consumes one
   `retry_allowance_remaining`, and an exhausted allowance returns
   `ErrBudgetLimit` with no wire work to follow.
4. **Expiry** — `Debit` samples the clock after the workspace lock. At or after
   `window_end` it returns `ErrBudgetWindow`: no new wire work. `Settle` has no
   expiry fence, so a provider call that straddles `window_end` still settles.
5. **Settle** — the only path that refunds, and it books verified usage itself.
   `terminationKnown && usageKnown` sets spend to `usage`: a worst-case
   pre-debit above usage is refunded by `pre-debit - usage` exactly once (the
   reservation leaves `reserved` in the same transaction, so a replay or a
   different event key for the same receipt is a no-op); usage above the
   pre-debit is charged the difference. `terminationKnown && !usageKnown`
   charges the full cap. A settle without observed termination retains slot and
   liability, and interim usage below the pre-debit is `ErrBudgetInvariant`.
   Duplicate receipts no-op; a conflicting receipt or usage returns
   `ErrBudgetConflict`. `BudgetService.SettleEvaluation` therefore never calls
   `Debit`; a broker that pre-debits and then settles is neither double-counted
   nor charged a retry.
6. **Uncertain native outcome** — an unacknowledged or uncertain admission keeps
   its slot and spend until an observed terminal receipt settles it.

## Crash schedule

| Row | Boundary | Injection |
| --- | --- | --- |
| R1a | before reserve commit | backend termination and context abort at the journal insert |
| R1b | after reserve commit | acknowledgement discarded; fresh process retries |
| R2a | before debit commit | backend termination at the journal insert |
| R2b | after debit commit, before wire | dispatcher stops; replay debits once |
| R3a | after native admission, before ack | dispatcher stops; outbox replayed |
| R4a | before settle commit | backend termination at the journal insert |
| R4b | after settle commit | acknowledgement discarded; fresh process replays |

The injector is a test-owned `AFTER INSERT` trigger on `governance_budget_journal`
that waits on an advisory lock, parking the transaction after its local writes
and before commit. The trigger is created and dropped per test.
