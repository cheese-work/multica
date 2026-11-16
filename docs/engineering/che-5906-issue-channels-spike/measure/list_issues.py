import json, subprocess, sys, os
out = sys.argv[1]; cli = os.environ["MULTICA_CLI_PATH"]
off = 0; n_all = 0
with open(out, "w") as f:
    while True:
        r = subprocess.run([cli, "issue", "list", "--limit", "100", "--offset", str(off), "--sort", "created_at",
                            "--direction", "desc", "--fields", "id,identifier,last_activity_at,created_at", "--output", "json"],
                           capture_output=True, text=True, check=True)
        d = json.loads(r.stdout); items = d["issues"] if isinstance(d, dict) else d
        for i in items: f.write(json.dumps(i) + "\n")
        n_all += len(items); off += len(items)
        if not (isinstance(d, dict) and d.get("has_more")) or not items: break
print("issues", n_all)
