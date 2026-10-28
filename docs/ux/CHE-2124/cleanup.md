# CHE-2124 task cleanup

The task environment `multica-954` was destroyed through `make destroy ARGS=--yes`.
The script verified its manifest and removed only the recorded fixture database and profile.
The host has no `psql` executable in PATH. A temporary task-only launcher used the existing `psql` inside `multica-postgres-1`.
The shared PostgreSQL container was not stopped or removed.

```text
Cleanup receipt
- Mount: /mnt/ssd2   df -B1 before: 128305864704 available bytes   after: 130407477248 available bytes   net returned: 2101612544 bytes
- Removed (exact paths, relative to the task working directory unless absolute):
  .impl-before (git worktree remove --force)
  .impl-store
  multica/node_modules
  multica/packages/ui/node_modules
  multica/packages/core/node_modules
  multica/packages/plugin-sdk/node_modules
  multica/packages/views/node_modules
  multica/packages/eslint-config/node_modules
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
  multica/apps/web/tsconfig.tsbuildinfo
  .capture-output/impl-before-results
  .capture-output/impl-desktop-results
  .capture-output/impl-e2e-results
  .capture-output/impl-cleanup-tools
  .capture-output/impl-quality
  /home/congvc/.multica/dev/envs/multica-954
  /home/congvc/.multica/profiles/dev-multica-954
  /home/congvc/multica_workspaces_dev-multica-954
  /home/congvc/.config/Multica Canary multica-954
  PostgreSQL database multica_multica_954
- Reference check: no matching task node/Electron/server process remained; the detached baseline worktree was removed; API port 19034 has no listener; recorded database count is 0.
- Recoverable because: dependencies and builds regenerate from the lockfile; the isolated fixture and profiles regenerate from the E2E setup; captures and raw test evidence are committed.
- Preserved on purpose: source checkout, screenshots and raw evidence, prior design evidence, shared caches, shared PostgreSQL, unrelated containers and worktrees, runtime daemon.
- Health after: shared PostgreSQL pg_isready accepts connections; runtime daemon is running at PID 1915359 with 0 failed and 0 pending terminal reports.
- Residual / owner decision: none for this task's capture services, fixture database or build artifacts.
```

The readings measure available space, not nominal file sizes.
Other agents use the same volume, so concurrent writes affect the net figure.
The task's local preview is no longer available.
