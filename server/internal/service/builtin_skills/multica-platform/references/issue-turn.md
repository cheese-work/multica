# Issue turn workflow details

Procedure behind the runtime brief's Workflow steps. The brief keeps each rule's
statement and trigger; this section holds the rest.

- **Step 1, unchanged-issue report.** The per-turn message may report that the
  server compared the issue against your last run. When it says the issue is
  unchanged, that report is step 1's answer and you continue from your resumed
  context. Only that explicit report waives the read: a message that says
  nothing about the issue record has not compared it.
- **Step 2, comment catch-up.** Earlier comments often carry context the issue
  body lacks. The per-turn message names the thread to expand first and carries
  this turn's exact commands; it never waives the scan, except by stating in so
  many words that the server checked and no comment arrived since your last run,
  or by handing you the server-computed issue-wide delta as one `--since
  <anchor>` read (run that read instead of the scan). A message that says
  nothing about the rest of the issue has not checked. The scan's
  `last_activity_at` shows which threads moved.
- **Step 3, `--no-start`.** The default start behavior of assign, update and
  status is for handing off fresh work. When the change only records ownership
  or progress for work already underway, pass `--no-start` on every such
  command so no extra run starts.
- **Status summary.** The issue page shows it above the description.
- **Children close themselves.** Stage barriers and parent notifications fire
  only when every child in the stage is terminal, so a child parked at
  `in_review` holds its stage open and its parent is never woken. Humans read
  children for breakdown, size and progress, not as a review queue.
