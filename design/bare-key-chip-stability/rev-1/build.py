# Generates rev-1/A.html and rev-1/B.html for design/bare-key-chip-stability.
# Usage: python3 build.py <rev-dir>
import sys
out = sys.argv[1]
DIRS = {
  "A": dict(cap="18rem", px="288", title="A — Coupled cap, 18 rem",
            lead="The bare-key box and the resolved chip both take the chip's existing cap, 18 rem (288 px) or the container width if narrower. This is what source 8187ea0 builds."),
  "B": dict(cap="14rem", px="224", title="B — Coupled cap, 14 rem",
            lead="Same coupling, smaller cap: 14 rem (224 px) or the container width if narrower. Short titles leave less empty space. Long titles truncate earlier."),
}
CSS = """
:root{--page-canvas:oklch(0.988087 0 0);--surface:oklch(1 0 0);--surface-border:oklch(0.92 0.004 286.32);--foreground:oklch(0.141 0.005 285.823);
--muted:oklch(0.967 0.001 286.375);--muted-foreground:oklch(0.505 0.016 285.938);--border:oklch(0.945 0.003 286.32);--accent:oklch(0.967 0.001 286.375);
--radius:.5rem;--text-caption:12px;--text-body:14px;--text-title:18px;--cap:%(cap)s}
*{box-sizing:border-box}
body{margin:0;background:var(--page-canvas);color:var(--foreground);font:var(--text-body)/20px system-ui,-apple-system,"Segoe UI",sans-serif;padding:24px}
.wrap{max-width:860px}h1{font-size:var(--text-title);line-height:28px;margin:0 0 2px}.lead{color:var(--muted-foreground);margin:0 0 8px}
h2.state{font-size:var(--text-caption);line-height:16px;font-weight:600;text-transform:uppercase;letter-spacing:.04em;color:var(--muted-foreground);margin:24px 0 6px}
.note{font-size:var(--text-caption);line-height:16px;color:var(--muted-foreground);margin:6px 0 0}
.bubble{background:var(--surface);border:1px solid var(--surface-border);border-radius:calc(var(--radius)*1.5);padding:10px 12px;max-width:560px;font-size:var(--text-body);line-height:22.75px}
.bubble p{margin:0}
.issue-mention{display:inline-flex;align-items:center;gap:6px;min-width:0;max-width:min(18rem,100%%);margin:0 2px;padding:2px 8px;border:1px solid var(--border);border-radius:calc(var(--radius)*.75);font-size:var(--text-caption);line-height:16px;vertical-align:middle;color:var(--foreground);text-decoration:none}
.stable{width:min(var(--cap),100%%);max-width:min(var(--cap),100%%)}
.icon{flex:none;width:14px;height:14px}.slot{flex:none;width:14px;height:14px}
.key{flex:none;font-weight:500;color:var(--muted-foreground)}.ttl{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
a.issue-mention:hover{background:var(--accent)}
.url{max-width:min(18rem,100%%)}
.tag{display:inline-block;font-size:var(--text-caption);line-height:16px;padding:0 6px;border-radius:999px;background:var(--muted);color:var(--muted-foreground);margin-left:6px}
@media(max-width:480px){body{padding:12px}}
"""
ICON = '<svg class="icon" viewBox="0 0 14 14" aria-hidden="true"><circle cx="7" cy="7" r="6" fill="none" stroke="currentColor" stroke-width="1.5" style="color:var(--muted-foreground)"/></svg>'
def pending(): return '<span class="issue-mention stable" aria-busy="true" data-state="pending"><span class="slot"></span><span class="key">MUL-7</span></span>'
def chip(t, cls="stable"): return f'<a class="issue-mention {cls}" href="#" data-state="resolved">{ICON}<span class="key">MUL-7</span><span class="ttl">{t}</span></a>'
SHORT="Short title"; LONG="A very long issue title that will certainly exceed the cap of the chip and must truncate"
def para(inner): return f'<div class="bubble"><p>see {inner} now and after.</p></div>'
for k, d in DIRS.items():
    body = f"""<div class="wrap"><h1>{d['title']}</h1><p class="lead">{d['lead']}</p>
<p class="note">Local to the bare-key presentation (an issue key typed in chat or a description). Box and chip are {d['px']} px wide whenever the container allows, so the text after them does not move. Real tokens from packages/ui/styles/tokens.css. Isolated design preview, not the product.</p>
<h2 class="state">1 · Pending lookup (non-link busy box)</h2>{para(pending())}
<p class="note">Not a link, no status icon until verified. aria-busy. Key text stays readable.</p>
<h2 class="state">2 · Resolved, short title</h2>{para(chip(SHORT))}
<p class="note">Same width and baseline as state 1. Space after the short title is empty by design.</p>
<h2 class="state">3 · Resolved, long title</h2>{para(chip(LONG))}
<p class="note">Title truncates with an ellipsis, as the chip does today.</p>
<h2 class="state">4 · Settled miss (404 or exhausted retry)</h2><div class="bubble"><p>see MUL-7 now and after.</p></div>
<p class="note">Plain text, no box, no link. Same as today.</p>
<h2 class="state">Reference · explicit chips stay content-sized<span class="tag">unchanged</span></h2>
<div class="bubble"><p>authored URL or UUID mention: {chip(SHORT, "url")}</p></div>
<p class="note">Only the bare-key presentation changes.</p></div>"""
    html = f'<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{d["title"]}</title><style>{CSS % dict(cap=d["cap"])}</style></head><body>{body}</body></html>'
    open(f"{out}/{k}.html", "w").write(html)
