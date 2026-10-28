# CHE-2124 F1/F2 task cleanup

The new `multica-954` fixture environment was destroyed through `make destroy ARGS='multica-954 --yes'`.
The script verified its manifest and removed only the recorded fixture database and profile.
A task-only `psql` launcher used `docker exec -i multica-postgres-1 psql` for the official destroy procedure.
The launcher was then removed. Shared PostgreSQL was not stopped or removed.

```text
Cleanup receipt
- Mount: /mnt/ssd2
- df -B1 task-start available: 131333042176 bytes
- df -B1 before cleanup available: 148349538304 bytes
- df -B1 after cleanup available: 149234872320 bytes
- Net available-space increase during cleanup: 885334016 bytes
- Net available-space increase from task start: 17901830144 bytes
- Removed (exact paths, relative to the task working directory unless absolute):
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
  .capture-output/fixes-web-results
  .capture-output/fixes-desktop-results
  .capture-output/fixes-cleanup-tools
  /home/congvc/.multica/dev/envs/multica-954
  /home/congvc/.multica/profiles/dev-multica-954
  /home/congvc/multica_workspaces_dev-multica-954
  /home/congvc/.config/Multica Canary multica-954
  PostgreSQL database multica_multica_954
- Reference check: task web PIDs 443640, 445315 and 445634 matched the task checkout before termination; no task Node/Electron/server references or nested mounts remained before file removal; the sole worktree was preserved.
- Recoverable because: dependencies/builds regenerate from the lockfile; isolated profiles/fixtures regenerate from the E2E setup; screenshots and raw evidence remain in docs/ux/CHE-2124.
- Preserved on purpose: source checkout, captures, raw evidence, shared caches, shared PostgreSQL, other agents' worktrees/containers, runtime daemon.
- Health after: shared PostgreSQL pg_isready accepts connections; runtime daemon is running at PID 1915359; API port 19034 and web port 13954 have no listener.
- Residual / owner decision: none for this task's capture services, fixture database or build artifacts. Small scratch logs and the contact sheet remain in .capture-output for audit.
```

These are shared-volume free-space readings, not a sum of nominal file sizes or a claim that all freed space came from this task.
Other agents' concurrent writes and cleanup affect both differences.
The local preview is no longer available.
