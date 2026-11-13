# Decision requests

A decision request asks one named person a question and records the answer.
One call posts one issue comment and sends one DM message to that person, and
stores a request you can read back. Use it when work needs a human choice;
what counts as needing one is workspace policy, not platform behavior.

```bash
multica decision request --issue <issue> --for <user-id> --class <class> --ask-file ./ask.json
multica agent decision ask --class <class> --ask-file ./ask.json   # in a run: no issue, no --for
multica decision ask --class <class> --ask-file ./ask.json          # same command
multica decision list --issue <issue> --output json
multica decision get <decision-id> --output json
multica decision withdraw <decision-id> --reason "no longer needed"
multica decision feed --output json   # sweep PAT with the decisions:feed scope only
multica decision feed ack <answer-id> --version <n>
multica issue human-in-charge <issue> --output json   # who to address; `human` is null when nobody is eligible
```

- `decision get` on an answered request carries `answer`: `answered_by_user_id`,
  `answer_source`, `chosen_option_index` (null for a text-only answer),
  `content_snapshot` (the person's words), `created_at` and `disclaimed_at`.
  `status: answered` with no `answer` means a batch card covered it: read
  `answered_by_batch_request_id`. A text-only answer on an
  option card is still an answer: read `content_snapshot`, do not treat the
  card as open.

- `--ask-file` is UTF-8 JSON: `agent_did`, `decision`, `options`,
  `recommendation`, `recommended_option` (the exact label of the recommended
  option; required with two or more options, the server puts it first),
  `default_if_no_reply`, `source_issue_id`, `source_comment_id`. An ask that approves a file adds `subject:
  "artifact_approval"` and `artifacts[]` (`attachment_id`, `label`, `sha256`);
  the server checks each hash.
- `agent decision ask` is the one-step form for a running task: the task names
  the issue, and the server picks the issue's requester (or a member creator)
  and the DM lane. With no such person it refuses; use `decision request --for`.
  The MCP tool `decision_ask` takes the same ask.
- `--class` takes one of the classes `decision request --help` lists. Which
  class a situation belongs to is workspace policy.
- `issue human-in-charge` needs a running task on the issue. It returns the
  first creator, requester, project lead or workspace owner who is an active
  member and may invoke you, with `source`; otherwise `human: null` and a `reason`.
- `--for` is the named person's user or workspace member id (a real UUID). It
  may be omitted only when an open intent already names the person.
- `--parent <comment-id>` replies under a comment; it is not allowed with an
  open intent.
- `--footer-file` appends text to the issue comment only (not the DM), for
  example handoff lines.
- `withdraw` works only on an unanswered request, and `--reason` is shown to
  the person.

## Choosing the DM lane

A request goes to a DM lane with that person. List the lanes first:

```bash
multica chat sessions --issue <issue> --for <user-id> --output json
```

Pass one lane's id as `--session <lane-id>` to `decision request`, or pass
`--new-dm` to start a fresh Decisions DM. `chat sessions` works only inside
an issue task whose decision window is open; `--for` is required when the
window names more than one person. See [chat.md](chat.md).

## After asking

The request is the ask, not the wait. Record who owns the next step in the
issue (status or a wakeup on the answer) and end the run; do not poll.

## Recording the answer

Only a `reversible` answer is recorded this way. For every other class
(`spend`, `deploy`, `publish`, `irreversible`, `product_direction`) the person
answers on the decision card. The server refuses `--decision-record` for them,
whether the source is a DM message or a member comment, and whether the record
is asked or unprompted. The error carries the card URL when a card exists. Do
not retry with another class: a record cannot change the request's class.

When the person answers a `reversible` ask in a DM, record it on the issue:

```bash
multica issue comment add <issue> --decision-record \
  --message <dm-message-id> [--context-message <offer-message-id>] \
  [--excerpt "<text>"]... [--content-file ./note.md]
```

- `--message` is the attested DM message. The server quotes it; you cannot
  type the quote. `--content-file` is an optional note, kept apart from the
  quote and collapsed in the UI.
- `--context-message` is the earlier assistant message that offered the
  options. The record shows the matching option label, such as `D1`.
- `--excerpt` quotes only that part of the message. It is repeatable. The
  server refuses text that is not a verbatim part of the message, after
  whitespace is collapsed. Without `--excerpt`, the whole message is quoted.
- When one message answers several asks, record each ask on its own issue and
  pass the excerpt for that record. Do not quote the whole message on every
  issue.
- The full message stays behind a collapsed "Full message" disclosure on an
  excerpted record.

## Record states

A record is pending until the agent is woken, then effective, or disclaimed
by the person. `reversible` records wake at once. No other class has a record
(see above), so there is no 10-minute wait to plan for. The person can disclaim while it is pending and after it is
effective; an effective record sends the agent a retraction. A disclaim
reopens a live asked request, and cancels the request of an unprompted record
or an archived DM. Times shown to people
are ICT. User-facing description: `docs/decision-records.md` in the repo.

## Agent decision reliance

```bash
multica decision rely --issue <issue> --gate <gate> --candidate-sha256 <sha256> --purpose "Work relying on this decision"
```

This command requires a running task token for the same issue. It registers
reliance on an effective agent settlement under the decision lock. A successful
`decided: true` response requires the reliance transaction to commit. Lookup
alone is display only and never grants authorization. A workspace creates
effective agent settlements only while its agent decisions setting is on
(default off).

## Agent decision brief and setting

```bash
multica decision brief
multica decision brief --format markdown
multica decision agent-setting --enabled=true
```

`brief` reads the feed for the daily brief section "Agents decided — veto?":
each record with its link, veto line and 24 hour review target (a target, not
an expiry; a veto still works later), notice delivery failures, and pilot
metrics. `--format markdown` prints that section ready to paste into the brief,
outside Needs you, with times in ICT; it prints nothing when the setting is off and there is nothing to show. Each veto line already names the reading
owner as undo owner. `agent-setting` turns the workspace setting on or off; only a
workspace owner can. One excluded-category decision turns it off by itself and
`brief` shows the stop reason. Existing records stay vetoable.

## Decision-bound writes (agent changes a governed field)

An agent may change a workflow's steps or a governed instruction field only
when a workspace owner approved that exact change. The workspace must allow
the agent and the field (the delegated governed writes setting); without it the write is
refused.

1. Author the change. Run the write command with `--print-hash`: it prints
   `before_sha256` and `after_sha256` and writes nothing.
2. Get a PASS from a reviewer of another lab on `after_sha256`.
3. Post the decision request. `decision` is
   `Apply this change to <field>?` then the two lines `before_sha256: <hex>` and
   `after_sha256: <hex>`; `<field>` is `workflow.steps`, `workflow.instructions`,
   `workspace.context`, `squad.instructions` or `agent.instructions`. `options`
   must include a card labelled exactly `Approve write`; the owner answers with
   that label alone (a note makes it conditional and the server refuses it).
4. After the owner approves, repeat the step 1 command with `--decision-id
   <decision-id>` instead of `--print-hash`. The decision is single use and
   expires after 7 days.
5. Read the value back and compare its hash with `after_sha256`.

```bash
multica workflow set-instructions <id> --status in_progress --file ./step.md --print-hash
multica workflow update <id> --steps-file ./steps.json --decision-id <decision-id>
multica agent update <id> --instructions-file ./i.md --expected-before-digest <hex> --decision-id <decision-id>
multica squad update <id> --instructions-file ./i.md --expected-before-digest <hex> --decision-id <decision-id>
multica workspace update <id> --context-file ./c.md --expected-before-digest <hex> --decision-id <decision-id>
```

- Instruction fields hash the raw UTF-8 bytes; pass the live value's hash as
  `--expected-before-digest`. The CLI computes `after_sha256` from the file.
- Workflow commands hash the live workflow read just before the write (steps as
  canonical JSON wrapped with the workflow id), so no `--expected-before-digest`.
  `after_sha256` is the hash of the steps as the server stores them: the CLI
  applies the server's own trim, handler-default and board-order rules (and adds
  missing `todo`, `done` and `cancelled` steps), so a newline-terminated
  instruction file or a partial `--steps-file` hashes like the stored result.
- A bound write carries one field: no other update flag, and for workflows no
  run-rule flag. `--decision-id` and `--vote-issue` are alternatives.
- 409 means the live value moved or the decision was used: read it again and
  ask for a new decision. The CLI never retries.

## Proposing an action (task token)

An agent proposes a governed action; a person approves it with a passkey on a
card. The proposal changes nothing itself.

```bash
multica decision propose issue.run_budget.raise --version 1 --issue <issue> \
  --for <user-id> --param window_id=<id> --param new_cap:=40 [--reason "..."]
multica decision propose <action-type> --version 1 --issue <issue> --params-file ./params.json
```

- `--version` and `--issue` are required. `--for` names the person; leave it
  out only when an open intent already names one.
- `--param k=v` sends a string. `--param k:=<json>` sends raw JSON (a number,
  boolean, array or object). Repeat the flag per key; a duplicate key is
  refused. `--params-file` is one JSON object and cannot be mixed with
  `--param`.
- It works only with a task token. The output prints the card link
  `<web>/<workspace>/issues/<issue-id>?approve=<action-id>`; give that link to
  the person. That link opens the approval only for a run-budget action; for
  any other action the approve page links to the card in the chat.
- Never send a change that needs a passkey as a plain option card
  (`multica decision request` without `--action-file`). Approve only
  records the answer. The server cannot read intent, so it does not refuse:
  when the decision or an option reads like a workflow, agent, squad,
  workspace or budget change (or names an allowlisted action type), the
  response carries an advisory hint. Heed it.
- A refusal prints the server's status and error code, for example
  `503 proposals_unavailable`. Report it; do not retry with a PAT.
- `issue.run_budget.raise` takes `issue_id`, `window_id`, `new_cap`;
  `issue.run_budget.top_up` takes `issue_id`, `window_id`. Prefer
  `multica issue budget raise|top-up`, which fills these for you.

## Proposing a config change

`multica config propose` writes the action file for a workspace config change:
it reads the live value, computes `before_sha256` and `after_sha256`, and the
file goes to `multica decision request --action-file`. It sends nothing.

```bash
multica config propose workspace.context --file ./context.md --out ./action.json
multica config propose workflow.step.instructions <workflow-id> --status-key review --file ./step.md --dry-run
multica config propose workspace.settings --set compact_prompt="..." --out ./action.json
```

- Fields: `agent.instructions <agent-id>`, `squad.instructions <squad-id>`,
  `workspace.context`, `workflow.steps <workflow-id>`,
  `workflow.step.instructions <workflow-id> --status-key <key>`,
  `workspace.settings --set key=value|key:=<json>`.
- `--dry-run` prints both digests and the action and writes nothing.
  `--out` is required otherwise.
- Any other field or settings key is refused as `denylisted` before a read.
- `workflow.steps` takes the steps array as the workflow GET returns it; the
  server refuses a differently normalized array (`after_digest_mismatch`).

## Config-change action files

Six action types change workspace or agent configuration. An owner approves
each with a passkey on a card that shows the diff. Send one as
`multica decision propose <type> --version 1 --issue <issue> --for <user-id>
--params-file ./params.json` (the `params` object below), or with
`multica decision request --issue <issue> --for <user-id> --class <class>
--ask-file ./ask.json --action-file ./action.json` (the whole file). Each action sends no
secret, takes no `secret_slots`, and changes nothing until the owner approves.

| Type | Changes | Digest recipe |
| --- | --- | --- |
| `workspace.context.set` | workspace context text | sha256 of the raw UTF-8 text |
| `agent.instructions.set` | one agent's instructions | sha256 of the raw UTF-8 text |
| `workspace.settings.patch` | allow-listed workspace settings keys | settings digest |
| `workspace.routing_agent.set` | the routing agent setting | settings digest |
| `workspace.delegated_writes.set` | the delegated governed writes setting | settings digest |
| `agent.skills.set` | one agent's skill list | skill-set digest |

`base_sha256` is the digest of the live value you read. `after_sha256` is the
digest of the value you want. The server recomputes both; a stale base is
refused as `precondition_drift`, a wrong after digest as
`after_digest_mismatch`. Both are lowercase hex SHA-256.

- **Text digest.** `printf %s "$(cat ./context.md)"` would strip the final
  newline, so hash the file itself: `sha256sum ./context.md`. A value that is
  not set hashes as the empty string.
- **Settings digest.** sha256 of the compact JSON, keys sorted, of
  `{"keys": {<key>: <value or null>}, "workspace_id": "<id>"}`. A key that is
  not set is `null`. For example, with `jq`:
  `jq -cnS --arg ws "$WS" --argjson v "$VALUE" '{keys:{routing_agent:$v},workspace_id:$ws}' | tr -d '\n' | sha256sum`.
  The value is the whole stored setting, in the shape shown below (all keys
  present, lists sorted and without duplicates, agent ids lower case).
- **Skill-set digest.** Over the agent's assignment: each skill id with its
  enabled flag, and each skill's bundle hash. Every skill in the after list
  counts as enabled. Read the live list with `multica agent skills list
  <agent-id> --output json` and each skill with `multica skill get <id>
  --output json`, then run the script below with the skills (a JSON array of
  those `skill get` objects, covering the live and the proposed skills), the
  live assignment (`{"<skill-id>": <enabled>}`) and the proposed id list. It
  prints `base_sha256` then `after_sha256`.
- Prefer `multica config propose` (above) for `workspace.context`,
  `agent.instructions` and `workspace.settings`: it reads the live value and
  writes this action file with both digests.

```python
import hashlib, json, sys

def part(h, s):
    b = s.encode()
    h.update(b"%d:" % len(b) + b + b"\n")

def manifest(skill):
    h = hashlib.sha256()
    for s in ("v1", "workspace", skill["id"], skill["name"], skill["description"], skill["content"]):
        part(h, s)
    for f in sorted(skill.get("files") or [], key=lambda f: f["path"].encode()):
        part(h, f["path"])
        part(h, "sha256:" + hashlib.sha256(f["content"].encode()).hexdigest())
        part(h, f["content"])
    return "sha256:" + h.hexdigest()

def digest(enabled, skills):
    h = hashlib.sha256(b"skill-set/v1\n")
    for sid in sorted(enabled):
        h.update(f"{sid}:{str(enabled[sid]).lower()}:{manifest(skills[sid])}\n".encode())
    return h.hexdigest()

skills = {s["id"]: s for s in json.load(open(sys.argv[1]))}
live, after = json.load(open(sys.argv[2])), json.load(open(sys.argv[3]))
print(digest(live, skills), digest({i: True for i in after}, skills))
```

### workspace.context.set

A `governed:human` block in the context may not change.

```json
{
  "type": "workspace.context.set",
  "version": 1,
  "params": {
    "workspace_id": "2f4c8a1e-6b3d-4e5f-9a70-1c2d3e4f5a6b",
    "context": "Be brief. Link every PR.\nRun the full gate before handoff.\n",
    "base_sha256": "f42e4df49cccd826231e9190f25f6f72ea5e86a7aea950668bd63e03e696742f",
    "after_sha256": "67f7a8af78c220a03c448dc2fa5f07c37ff1a17dff759515bfbc5cde1ad9314f"
  }
}
```

### agent.instructions.set

```json
{
  "type": "agent.instructions.set",
  "version": 1,
  "params": {
    "agent_id": "9d1e7c3a-5b24-4f68-8a0c-3e5f7a9b1d2c",
    "instructions": "Review PRs.\nSign the verdict with the exact head SHA.\n",
    "base_sha256": "9cc0cb00eec6ef16a5716369155246ffcfe43668eff4f463eca4fe6e4884f7bd",
    "after_sha256": "9be91507313a3e1d0a98284b7d4f1a0282956f15e5047a144436699b654c4c8a"
  }
}
```

### workspace.settings.patch

`settings` names the keys to change; a stored key it does not name keeps its
value. The digest covers the named keys only. A key outside the allow table is
refused as `denylisted`.

```json
{
  "type": "workspace.settings.patch",
  "version": 1,
  "params": {
    "workspace_id": "2f4c8a1e-6b3d-4e5f-9a70-1c2d3e4f5a6b",
    "settings": {
      "compact_prompt": "Keep decisions and open questions.",
      "wakeups_paused": false
    },
    "base_sha256": "9932f2bdd26537a00ae58c41be55d8ed6d32aa46f7b1e95c690c83a9022e9ec",
    "after_sha256": "5c61f5f6ae0af114947168ce3751df591ba6059e589a12c2184adf63241caa88"
  }
}
```

### workspace.routing_agent.set

Replaces the whole routing agent setting. Every agent id must be a live agent
of the workspace. You may not newly name yourself. Send `overrides` as `{}`
when there is none, and list `statuses` in full (the server fills the default
list when it is empty, so the digest of an empty list would not match).

```json
{
  "type": "workspace.routing_agent.set",
  "version": 1,
  "params": {
    "workspace_id": "2f4c8a1e-6b3d-4e5f-9a70-1c2d3e4f5a6b",
    "routing_agent": {
      "agent_id": "9d1e7c3a-5b24-4f68-8a0c-3e5f7a9b1d2c",
      "fallback_agent_id": "c3b8a6d4-1e7f-4a29-b5c0-7d9e1f3a5b8c",
      "statuses": ["todo", "blocked"],
      "overrides": {}
    },
    "base_sha256": "d79779acfbe20a6c16ba54c37f23d964c7ade0a05aed7c4dc070f51140d2d53a",
    "after_sha256": "09ef2f63c872212917cd1b7a87e1e322db0497d924b4378f3212015aac3c6b6f"
  }
}
```

### workspace.delegated_writes.set

Replaces the whole delegated governed writes setting. A proposal may not give
its own proposer write authority it does not already hold, and may not turn
`requires_human_decision` from true to false. Narrowing and granting another
agent are allowed.

```json
{
  "type": "workspace.delegated_writes.set",
  "version": 1,
  "params": {
    "workspace_id": "2f4c8a1e-6b3d-4e5f-9a70-1c2d3e4f5a6b",
    "delegated_governed_writes": {
      "allowed_agents": ["9d1e7c3a-5b24-4f68-8a0c-3e5f7a9b1d2c"],
      "allowed_fields": ["agent.instructions", "workspace.context"],
      "requires_human_decision": true
    },
    "base_sha256": "4b7867c4982199d304c5973c34bf1f15b71671d93e694e1ef46dbbce6c56af3f",
    "after_sha256": "042b74af678d40ec7229340f9329062bba054cfaac701232a13226b1b547111b"
  }
}
```

### agent.skills.set

Replaces the agent's whole skill list (at most 50 skills; `[]` removes all).
Every skill must be in the workspace. An edit to a listed skill or an enabled
flag change after you propose makes the approval fail with
`precondition_drift`: read again and propose again. An agent may not add or
re-enable a skill for itself.

```json
{
  "type": "agent.skills.set",
  "version": 1,
  "params": {
    "agent_id": "9d1e7c3a-5b24-4f68-8a0c-3e5f7a9b1d2c",
    "skill_ids": [
      "5a6b7c8d-1e2f-4a3b-9c4d-5e6f7a8b9c0d",
      "7e8f9a0b-2c3d-4e4f-8a5b-6c7d8e9f0a1b"
    ],
    "base_sha256": "d6a47789d494253b9e3f96999ba50883304297fde3a3f61c5a473729c48c38a2",
    "after_sha256": "a7694e72cc6cad647e1742339da5113eae1b8b208132b369b326b287884a9744"
  }
}
```
