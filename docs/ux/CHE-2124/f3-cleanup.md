# CHE-2124 F3 task cleanup

The task's new `multica-954` environment was destroyed through the official managed procedure.
Its manifest named this checkout, API port 19034, web port 13954 and database `multica_multica_954`.
The task-only `psql` launcher used the existing shared PostgreSQL container; it did not provision or remove a container.
[Destroy output](raw/f3/f3-destroy.log) records removal of the fixture database, CLI profile and environment slot.

```text
Cleanup receipt
- Mount: X99 /mnt/ssd2
- df -B1 task-start available: 127721230336 bytes
- df -B1 immediately before file cleanup available: 115653603328 bytes
- df -B1 after file cleanup available: 117232332800 bytes
- Net available-space increase during file cleanup: 1578729472 bytes
- Removed (exact paths, relative to this task workdir unless absolute):
  multica/node_modules
  multica/packages/ui/node_modules
  multica/packages/core/node_modules
  multica/packages/plugin-sdk/node_modules
  multica/packages/views/node_modules
  multica/packages/eslint-config/node_modules
  multica/apps/docs/node_modules
  multica/apps/ui-lab/node_modules
  multica/apps/mobile/node_modules
  multica/apps/desktop/node_modules
  multica/apps/web/node_modules
  multica/apps/web/.next
  multica/apps/web/.source
  multica/apps/desktop/out
  multica/apps/desktop/resources/bin
  multica/server/bin/linux-amd64
  multica/server/data/uploads
  multica/.turbo
  multica/.env
  .capture-output/f3-red-results
  .capture-output/f3-green-results
  .capture-output/f3-desktop-results
  .capture-output/f3-permission-results
  .capture-output/f3-cleanup-tools
  /home/congvc/.multica/dev/envs/multica-954
  /home/congvc/.multica/profiles/dev-multica-954
  /home/congvc/multica_workspaces_dev-multica-954
  /home/congvc/.config/Multica Canary multica-954
  PostgreSQL database multica_multica_954
- Reference check: Next.js listener PID 3550934 had this checkout's apps/web cwd before termination. Protected runtime daemon PID 1915359 was not targeted. The official destroy stopped the task API. No task Node/Electron/server references, port listeners or nested mounts remained before file cleanup; the source worktree was preserved.
- Recoverable because: lockfile dependencies and builds regenerate; isolated fixture accounts/profiles regenerate from E2E setup. Captures and raw evidence remain in docs/ux/CHE-2124.
- Preserved on purpose: source checkout, evidence, shared caches/PostgreSQL, unrelated worktrees/containers and runtime daemon. Unattributed apps/docs/.source output was not removed.
- Health after: PostgreSQL accepts connections; runtime-scoped daemon status is running at PID 1915359; ports 19034 and 13954 have no listener.
- Residual / owner decision: small scratch logs/contact sheet retained for audit. No task capture service or fixture database remains. Sol preserves unattributed docs output rather than broadening cleanup.
```

The [before](raw/f3/disk-before-cleanup.txt) and [after](raw/f3/disk-after-cleanup.txt) readings are shared-volume free space.
Concurrent writes by other agents affect the difference; it is not the sum of nominal artifact sizes.
The preview is no longer available.
