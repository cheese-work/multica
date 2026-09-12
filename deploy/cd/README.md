# D1 image qualification

This directory contains the build-side half of CHE-372's deployment design.
It builds a linux/amd64 backend and web image pair from a trusted `main` push,
records their immutable registry digests in a release manifest, and validates
the tuple before a later controller can consider it for deployment.

It does not deploy, contact C00, receive C00 deployment credentials, receive
recovery private keys, or use restored production data. The D2 controller is
the only component allowed to mutate C00.

The main-push workflow produces a `build-evidence` manifest. Its configuration
digest is for the synthetic fixture only, so `admission.mjs` refuses it for a
deployment. A `release-candidate` manifest requires the fresh Hermes
configuration and migration snapshot and is the only manifest class that can
pass admission.

`PR #20` (`CHE-392`) is an explicit prerequisite for running this workflow on
the dedicated X99 GitHub Actions runner. This change does not extend or
replace that pull request.

## Tuple rule

The manifest binds one source SHA, one configuration digest, one migration
inventory digest, and the backend/web linux/amd64 image digests. Changing any
member invalidates the affected qualification evidence. A later D2/D4 stage
must supply a fresh C00 configuration, live image, and migration snapshot
before it runs an upgrade or rollback rehearsal.

## Local checks

```bash
bash deploy/cd/test-release-manifest.sh
bash deploy/cd/test-admission.sh
bash deploy/cd/test-isolated-qualification.sh
```

The isolated qualification test uses only synthetic, throwaway data. Its
upgrade/rollback command is intentionally disabled until the caller supplies
an admitted tuple and a non-production fixture location.
