---
name: steel-browser
description: How an agent gets a browser from the shared Steel pool — lease first, never hold a Steel credential, never touch a login.
---

# Steel browser

A browser comes from a lease. Without a lease you have no browser.

## Rules

1. **Lease first.** Run `multica browser lease` for the profile and class you need. Use only what it returns. Run `multica browser release` when you finish.
2. **You hold no Steel credential.** The lease carries a short-lived token. Never copy it into a comment, a file or a log.
3. **Never call Steel `/context`.** It can read cookies and page storage.
4. **Never type a credential.** Do not sign in to an account for anyone. A profile that needs a login is a human step: Cheese logs in by hand, and you stop and ask.
5. **Never reuse a session you did not lease.** A session ID from an old run is not yours.
6. **A refusal is an answer.** If the lease is denied or queued, say so on the issue. Do not look for another route to a browser.

## Classes

- `S0` anonymous: public pages, no login profile.
- `S2` browser with a login profile: needs a decision on the issue before the lease mints.

The `multica browser` commands ship in a later slice of this plan. Until they exist, you have no way to lease: stop and report.
