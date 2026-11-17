# Tevo ownership policy

`tevo-prod1-guard.json` is the canonical policy for CHE-6879. The latest ownership
handoff `01a1e905-220b-7025-87d7-ebc6a12ad975` (17 Nov 2026 ICT) replaces the
initial 25-repository team roster. Its attached `allow.txt` (SHA256
`bb9d848d4e5e145c9494c0ead31ea2061075ee09c8b04426bf9f09cfdf48c416`)
lists 39 approved repositories, including ST008 and shared `tevo_iap`.
TT016 and SA041 are explicitly denied. The policy includes those 39 plus six
shared tooling exemptions below: 45 repositories and 33 product codes.
No membership, contributor share or prefix grants ownership. Repository names
are exact and case-insensitive. Unlisted Tevo repositories are denied.

The vitals crawler and `vitals.py scope` read this file from this repository's
`main` through the authenticated GitHub contents API. Product codes are generated
from approved repository names; `derived/prod1-codes.txt` is output, not an
independently maintained source. Sweep and rollout inventories intersect these
exact names before acting.

Deploy compatible server/CLI/daemons before proposing this policy through
`multica guard plugin set`. The existing passkey-approved settings action then
copies it to `workspace.settings.guard_policy`; it is not active merely because
this file exists. Running tasks keep their delivered policy snapshot; start new
runs after activation. Existing digest/file protection and credential-broker
routing stay in force. The PATH guard covers managed POSIX runs; absolute tool
paths and Windows shims are outside that existing guard boundary.

The policy retains all default plugins. Its scope boundary runs before plugin
selection and cannot be weakened by org, repo or path layers. Unknown push
options, unresolved GitHub identities, and unexpanded aliases fail closed.
Use canonical Git/GitHub commands when an alias is refused.
When the allowlist is enabled, non-GitHub and local-path push destinations are
also refused because their GitHub identity cannot be verified. Unlisted GitHub
owners remain unrestricted. Git layer targeting does not require a clone or
GitHub API lookup; only `gh` resolves repository layers.

Shared tooling exemptions use the first handoff's explicit delegation to decide
per tooling repository (`01a1e8eb-4150-753e-90eb-5c5d85725ddd`). Six are additional
to the latest attachment; the others already appear in its 39:

| Repository | Reason |
| --- | --- |
| `code-android-gradle-plugin` | Shared Android build plugin; additional tooling exemption. |
| `code-android-template` | Shared Android template, expressly retained by the handoff. |
| `code-android-template-fork` | Shared Android template fork, not a product. |
| `flutter-template` | Shared Flutter template, expressly retained by the handoff. |
| `playevo-mobile-sdk` | Shared Quest SDK, expressly retained by the handoff. |
| `playevo-example` | Shared Quest SDK example, expressly retained by the handoff. |
| `mMonetize` | Shared monetization SDK, not a product. |
| `mMonetize-sample` | Shared monetization SDK sample, not a product. |
| `tevo_iap` | Shared billing SDK, not a product. |
| `tevo-license-mobile-sdk` | Shared mobile license SDK, not a product. |
| `flutter-ads` | Shared Flutter ads library, not a product. |
| `che-5179-template-sync-smoke` | Shared template test fixture, not a product. |
