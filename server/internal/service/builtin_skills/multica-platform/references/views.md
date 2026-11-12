# Saved views

When a request names a saved view ("the Bugs view", a view link), read it
with `view issues` instead of rebuilding its filters from `issue list` flags:
the server applies every condition the view saves — "is not", the advanced
condition tree, relative due dates, project status — which `issue list`
cannot express.

```bash
multica view list --output json
multica view issues "Bugs" --output json --fields identifier,title,status
```

`<view>` is an ID, ID prefix, or exact name. Pages hold at most 100 issues;
while `has_more` is true, pass `next_cursor` to `--cursor`. Views and their
"me" conditions resolve against the runtime owner, so a member's private view
is only visible when that member owns the runtime.
