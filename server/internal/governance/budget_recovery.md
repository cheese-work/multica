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
3. **Wire key** — each wire attempt persists a worst-case `Debit` under a unique
   `EventKey` *before* the broker sends. The same key never debits twice. The
   first debit rides the attempt reserved at admission; each further distinct
   key consumes one `retry_allowance_remaining`; an exhausted allowance returns
   `ErrBudgetLimit` and no wire work may follow.
4. **Expiry** — `Debit` samples the clock after the workspace lock. At or after
   `window_end` it returns `ErrBudgetWindow`; no new wire work is allowed.
5. **Settle** — `terminationKnown && usageKnown` refunds only the proven
   remainder; `terminationKnown && !usageKnown` charges the full cap; a settle
   without observed termination retains slot and liability. Duplicate receipts
   no-op; a conflicting receipt or usage returns `ErrBudgetConflict`.
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
