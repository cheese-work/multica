# Labels, metadata and properties

Three ways to tag an issue. Labels are named colored tags. Metadata is free-form
key/value on one issue. Properties are typed fields defined once per workspace.

## Labels

A label has a `resource-type`: `issue` (the default) or `skill`.

```bash
multica label list [--resource-type issue|skill] --output json
multica label get <id>
multica label create --name "needs-design" --color "#3b82f6" [--description "..."] [--resource-type skill]
multica label update <id> --name ... --color ... --description ...
multica label delete <id>

multica issue label list <issue-id>
multica issue label add <issue-id> <label-id>
multica issue label remove <issue-id> <label-id>

multica skill label list <skill-id>
multica skill label add <skill-id> <label-id>
multica skill label remove <skill-id> <label-id>
```

`--color` is a hex value like `#3b82f6` and is required on create. `add` and
`remove` take a label id from `label list`, never a name. A skill takes only
`skill` labels and an issue only `issue` labels. `label delete` removes the
label from the workspace; read `label get` first.

## Issue metadata

Per-issue key/value pairs for machine state that does not deserve a property.

```bash
multica issue metadata list <issue-id>
multica issue metadata get <issue-id> --key <key>
multica issue metadata set <issue-id> --key <key> --value waiting
multica issue metadata delete <issue-id> --key <key>
```

`--value` is JSON-parsed: `true` is a bool, `3` a number, `waiting` a string.
Pass `--type string|number|bool` to force a type, or quote the value as `'"42"'`
to keep a number-looking value a string. `set` overwrites the key.

## Property definitions

`multica property` manages the definitions. Values on an issue are set with
`multica issue property set` (see `references/issues.md`).

```bash
multica property get <id-or-name>
multica property create --name Severity --type select \
  --option "Critical:#ef4444" --option "Minor:#6b7280" [--description ...] [--icon flag]
multica property update <id-or-name> --name ... --description ... --icon ... --option ...
multica property archive <id-or-name>
multica property unarchive <id-or-name>
```

Types: `text`, `number`, `select`, `multi_select`, `date`, `checkbox`, `url`,
`actor`, `multi_actor`, `multi_text`, `multi_url`. Only `select` and
`multi_select` take `--option` (`"Name"` or `"Name:#rrggbb"`, repeatable).

- `update --option` REPLACES the whole option list. Options are matched by
  name, so existing option ids and the values stored on issues survive. Omit a
  name and that option is dropped.
- `--icon ""` clears the icon.
- `archive` hides the property from pickers and keeps its values;
  `unarchive` restores it. Prefer it to any destructive change.

## Board order

```bash
multica issue reorder <id> --top | --bottom | --before <issue-id> | --after <issue-id>
```

Pick exactly one target. It moves the issue inside its current status column
only. To move it to another column, change its status; the issue lands at the
top of the destination column and needs no reorder.
