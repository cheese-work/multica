#!/usr/bin/env python3
"""Generates A.html, B.html, C.html for design/dm-open-decisions/rev-1. Static output, no network."""
import html
from pathlib import Path

OUT = Path(__file__).parent

ICON = {
    "wait": '<path d="M12 8v5"/><path d="M12 16h.01"/><circle cx="12" cy="12" r="9"/>',
    "ok": '<path d="M20 6 9 17l-5-5"/>',
    "x": '<circle cx="12" cy="12" r="9"/><path d="m15 9-6 6M9 9l6 6"/>',
    "down": '<path d="m6 9 6 6 6-6"/>',
    "up": '<path d="m18 15-6-6-6 6"/>',
    "wifi": '<path d="m2 2 20 20M8.5 16.5a5 5 0 0 1 7 0M2 8.8a15 15 0 0 1 4.2-2.6M10.7 5.1A15 15 0 0 1 22 8.8M5 12.9a10 10 0 0 1 5.2-2.8M13.5 10.2A10 10 0 0 1 19 12.9M12 20h.01"/>',
    "warn": '<circle cx="12" cy="12" r="9"/><path d="M12 8v4M12 16h.01"/>',
    "more": '<circle cx="5" cy="12" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/>',
    "retry": '<path d="M3 12a9 9 0 1 0 3-6.7L3 8"/><path d="M3 3v5h5"/>',
    "inbox": '<path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.5 5.1 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.5-6.9A2 2 0 0 0 16.7 4H7.3a2 2 0 0 0-1.8 1.1z"/>',
}


def ic(name, cls="sm"):
    return f'<svg class="i {cls}" viewBox="0 0 24 24" aria-hidden="true">{ICON[name]}</svg>'


E = html.escape

ROWS = [
    dict(key="CHE-1890", title="feat(decisions): one decision request posts one issue comment", cls="Spend", age="2h", state="waiting"),
    dict(key="CHE-1861", title="Approve the C00 deploy of the DM unread fix", cls="Deploy", age="5h", state="waiting"),
    dict(key="CHE-1835", title="Retention window for decision answers", cls="Product direction", age="1d", state="answered"),
    dict(key="CHE-1812", title="Rotate the staging signing key", cls="Irreversible", age="2d", state="waiting"),
    dict(key="CHE-1799", title="Publish the 0.4 changelog to the public site", cls="Publish", age="3d", state="waiting"),
]
LONG = dict(key="CHE-1902", title="refactor(decisions): split the cancel worker from the reask worker so a slow reask no longer delays cancellation notices for other sessions", cls="Product direction", age="4h", state="waiting")
GONE = dict(key="CHE-1861", title="Approve the C00 deploy of the DM unread fix", cls="Deploy", age="5h", state="gone", note="Cancelled · issue closed")

STATE_TXT = {"waiting": ("Waiting on you", "wait"), "answered": ("Answered", "ok")}


def row(r, extra=""):
    if r["state"] == "gone":
        label, icon, st = r["note"], "x", "gone"
    else:
        label, icon = STATE_TXT[r["state"]]
        st = r["state"]
    cls_hot = " hot" if r["cls"] in ("Irreversible", "Spend", "Deploy", "Publish") else ""
    kind = "gone" if st == "gone" else ("wait-me" if st == "waiting" else "")
    return (
        f'<li class="dr {kind} {extra}"><a href="#{r["key"]}">'
        f'<span class="st {st}">{ic(icon, "xs")}{E(label)}</span>'
        f'<span class="key">{r["key"]}</span><span class="ttl">{E(r["title"])}</span>'
        f'<span class="cls{cls_hot}">{E(r["cls"])}</span><span class="age tnum">{r["age"]}</span></a></li>'
    )


def skel_rows(n=2):
    one = ('<li class="dr skelrow"><a href="#" tabindex="-1"><span class="skel" style="width:110px;height:12px"></span>'
           '<span class="skel" style="width:64px;height:12px"></span><span class="skel" style="width:70%;height:12px"></span>'
           '<span class="skel" style="width:64px;height:16px"></span><span class="skel" style="width:24px;height:12px"></span></a></li>')
    return one * n


def waiting_count(rows):
    return sum(1 for r in rows if r["state"] == "waiting")


def head(extra="", name="Reviewer agent", sub="Direct message · x99-claude-opus"):
    return (
        f'<header class="p-head"><div class="av" aria-hidden="true">X</div>'
        f'<div style="min-width:0;flex:1 1 auto"><div class="nm">{E(name)}</div><div class="sub">{E(sub)}</div></div>'
        f'<div class="sp"></div>{extra}'
        f'<button class="icbtn touch" aria-label="Chat actions">{ic("more")}</button></header>'
    )


MSGS = (
    '<div class="gut"><div class="col msgs">'
    '<div class="msg">I need your call on the CHE-1890 spend before I continue. Details are on the issue.</div>'
    '<div class="msg me">Looking now.</div></div></div>'
)

COMPOSER = '<div class="gut composer"><div class="col"><div class="box">Message x99-claude-opus…</div></div></div>'


def pane(top="", header_extra="", dock="", overlay="", msgs=MSGS):
    return f'<div class="pane">{head(header_extra)}{top}{msgs}<div class="gut"><div class="col">{dock}</div></div>{COMPOSER}{overlay}</div>'


def frame(title, note, body):
    return f'<section class="frame"><div class="cap"><b>{E(title)}</b><span>{E(note)}</span></div>{body}</section>'


def page(name, idea, frames, extra_head=""):
    return f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>dm-open-decisions rev-1 {name}</title><link rel="stylesheet" href="shared.css"></head>
<body><main class="board">
<div class="board-head"><h1>dm-open-decisions rev-1 · Direction {name}</h1><p>{E(idea)} Static mockup from <span class="mono">tokens.css</span> values; row data is invented. Light theme only. Click targets are not wired.</p></div>
<div class="grid">{''.join(frames)}</div></main></body></html>"""


# ---------------------------------------------------------------- A: pinned strip
def strip_a(rows, folded=False, show_all=False, mode="ok", live=None, hl=None):
    n = waiting_count(rows)
    cnt = f'<span class="cnt tnum" aria-label="{len(rows)} open decisions">{len(rows)}</span>' if rows else ""
    title = "Open decisions"
    toggle_lbl = "Show" if folded else "Hide"
    toggle = (f'<button class="tgl" aria-expanded="{"false" if folded else "true"}" aria-controls="od-list">'
              f'{ic("down" if folded else "up", "xs")}{toggle_lbl}</button>')
    if mode == "loading":
        return (f'<section class="strip gut-in" aria-labelledby="od-h" aria-busy="true"><div class="sh"><h2 id="od-h">{title}</h2><span class="sp"></span></div>'
                f'<ul class="list" id="od-list">{skel_rows(2)}</ul></section>')
    if mode == "error":
        return (f'<section class="strip" aria-labelledby="od-h"><div class="sh"><h2 id="od-h">{title}</h2><span class="sp"></span></div>'
                f'<div class="ban err" role="alert">{ic("warn", "xs")}<span>Could not load open decisions.</span><span class="sp"></span>'
                f'<button class="btn outline sm">{ic("retry", "xs")}Retry</button></div></section>')
    visible = rows if (show_all or len(rows) <= 3) else rows[:3]
    li = "".join(row(r, "hl" if hl == r["key"] else "") for r in visible)
    more = ""
    if len(rows) > 3:
        more = (f'<div class="more"><button class="tgl" aria-expanded="{"true" if show_all else "false"}">'
                f'{ic("up" if show_all else "down", "xs")}{"Show fewer" if show_all else f"Show {len(rows)-3} more"}</button></div>')
    ban = ""
    if mode == "offline":
        ban = f'<div class="ban" role="status">{ic("wifi", "xs")}<span>Not live. Reconnecting; rows may be out of date.</span></div>'
    liveline = f'<span class="live" role="status">{live}</span>' if live else ""
    hint = ""
    if folded:
        hint = f'<span class="muted t-caption">{n} waiting on you</span>' if n else ""
    body = "" if folded else f'<ul class="list" id="od-list">{li}</ul>{more}{ban}'
    return (f'<section class="strip" aria-labelledby="od-h"><div class="sh"><h2 id="od-h">{title}</h2>{cnt}{hint}'
            f'<span class="sp"></span>{liveline}{toggle}</div>{body}</section>')


def wrap_top(strip):
    return f'<div class="gut"><div class="col">{strip}</div></div>'


def build_a():
    r3 = ROWS[:3]
    frames = [
        frame("1 · Default — 2 waiting, 1 answered", "desktop 1440; strip sits between the header and the messages", pane(wrap_top(strip_a(r3)))),
        frame("2 · Five requests — shows 3, “Show 2 more”", "rule: collapsible when it has more than about 3 rows", pane(wrap_top(strip_a(ROWS)))),
        frame("3 · Five requests — expanded", "the message list keeps its own scroll; the strip grows to 5 rows", pane(wrap_top(strip_a(ROWS, show_all=True)))),
        frame("4 · Folded by the person", "one line: count and “N waiting on you”. Stays folded for this session view", pane(wrap_top(strip_a(r3, folded=True)))),
        frame("5 · A request is cancelled — row lingers 5 s, then leaves", "struck title, reason in text, polite live-region line; the other rows do not move until it leaves", pane(wrap_top(strip_a([ROWS[0], GONE, ROWS[2]], live="CHE-1861 cancelled: issue closed", hl="CHE-1861")))),
        frame("6 · Answered row, then the agent acts", "answered rows stay until the request closes; state text changes from “Waiting on you” in place", pane(wrap_top(strip_a([dict(ROWS[0], state="answered"), ROWS[1]], live="CHE-1890 answered")))),
        frame("7 · Empty — strip is not rendered", "no heading, no empty card; the DM looks as it does today", pane("")),
        frame("8 · Loading", "two skeleton rows; the strip keeps its place so the messages do not jump when rows arrive", pane(wrap_top(strip_a([], mode="loading")))),
        frame("9 · Load failed", "inline error with Retry; messages stay usable", pane(wrap_top(strip_a([], mode="error")))),
        frame("10 · Disconnected", "rows stay, marked as possibly out of date, until the socket is back and the list is refetched", pane(wrap_top(strip_a(r3, mode="offline")))),
        frame("11 · Long title", "title truncates with an ellipsis; the issue key and state stay visible", pane(wrap_top(strip_a([LONG, ROWS[1]])))),
    ]
    return page("A — Pinned strip", "A card pinned under the DM header. Always visible, so a person sees what is waiting before they read the thread.", frames)


# ---------------------------------------------------------------- B: header chip + popover
def chip(rows, mode="ok", pressed=False):
    n = waiting_count(rows)
    p = "true" if pressed else "false"
    if mode == "loading":
        return '<span class="skel" style="width:132px;height:28px;border-radius:999px" aria-busy="true"></span>'
    if mode == "error":
        return f'<button class="hchip err" aria-haspopup="dialog" aria-expanded="{p}">{ic("warn", "xs")}<span class="lbl-long">Decisions unavailable</span><span class="lbl-short">Retry</span></button>'
    if mode == "offline":
        return f'<button class="hchip quiet" aria-haspopup="dialog" aria-expanded="{p}">{ic("wifi", "xs")}<span class="lbl-long">{len(rows)} open · not live</span><span class="lbl-short">{len(rows)} · not live</span></button>'
    if n == 0 and rows:
        return f'<button class="hchip quiet" aria-haspopup="dialog" aria-expanded="{p}">{ic("ok", "xs")}<span class="lbl-long">{len(rows)} answered</span><span class="lbl-short">{len(rows)} answered</span></button>'
    return (f'<button class="hchip" aria-haspopup="dialog" aria-expanded="{p}">{ic("wait", "xs")}'
            f'<span class="lbl-long">{n} waiting on you</span><span class="lbl-short">{n} waiting</span></button>')


def popover(rows, mode="ok", live=None, hl=None):
    li = "".join(row(r, "hl" if hl == r["key"] else "") for r in rows)
    ban = ""
    if mode == "offline":
        ban = f'<div class="ban" role="status">{ic("wifi", "xs")}<span>Not live. Reconnecting; rows may be out of date.</span></div>'
    if mode == "error":
        li_html = (f'<div class="ban" role="alert" style="background:color-mix(in oklch, var(--destructive) 5%, white);color:oklch(0.45 0.2 27)">{ic("warn", "xs")}'
                   f'<span>Could not load open decisions.</span><span style="flex:1"></span><button class="btn outline sm">{ic("retry", "xs")}Retry</button></div>')
    elif mode == "loading":
        li_html = f'<ul class="list">{skel_rows(2)}</ul>'
    else:
        li_html = f'<ul class="list">{li}</ul>'
    lv = f'<span class="muted t-caption" role="status" style="margin-left:auto">{live}</span>' if live else ""
    return (f'<div class="pop" role="dialog" aria-label="Open decisions"><div class="sh">Open decisions{lv}</div>{li_html}{ban}</div>')


def build_b():
    r3 = ROWS[:3]

    def p(rows, mode="ok", open_=False, live=None, hl=None, chip_mode=None):
        cm = chip_mode or mode
        ov = popover(rows, mode, live, hl) if open_ else ""
        return pane("", chip(rows, cm, open_), overlay=ov).replace('class="pane"', 'class="pane" style="min-height:420px"' if open_ else 'class="pane"', 1)

    frames = [
        frame("1 · Closed — chip shows the count", "a warning-tone chip in the header; nothing else on the page changes", p(r3)),
        frame("2 · Open — 2 waiting, 1 answered", "popover under the chip; Esc or outside click closes it, focus returns to the chip", p(r3, open_=True)),
        frame("3 · Five requests — open", "popover scrolls after about 5 rows; no “show more” step", p(ROWS, open_=True)),
        frame("4 · A request is cancelled — popover open", "row lingers 5 s with a struck title and the reason; the chip count drops at once", p([ROWS[0], GONE, ROWS[2]], open_=True, live="CHE-1861 cancelled", hl="CHE-1861")),
        frame("5 · A request is cancelled — popover closed", "only the count changes; a polite live-region line says what changed. This is the weak spot of B", p([ROWS[0], ROWS[2]])),
        frame("6 · Only answered rows remain", "chip goes calm: grey, check icon, “1 answered”", p([ROWS[2]])),
        frame("7 · Empty — chip is not rendered", "header is as it is today", p([], chip_mode="ok") .replace(chip([], "ok"), "")),
        frame("8 · Loading", "skeleton chip keeps the header from shifting", p([], mode="loading", chip_mode="loading")),
        frame("9 · Load failed — popover open", "chip reads “Decisions unavailable”; Retry inside the popover", p([], mode="error", open_=True)),
        frame("10 · Disconnected — popover open", "chip says “not live”; rows are kept", p(r3, mode="offline", open_=True)),
        frame("11 · Long title in the popover", "title truncates; key and state stay", p([LONG, ROWS[1]], open_=True)),
    ]
    return page("B — Header chip + popover", "No space cost: a count chip in the DM header opens a list. Quietest layout, but the list is one click away.", frames)


# ---------------------------------------------------------------- C: dock above the composer
def dock(rows, expanded=False, mode="ok", live=None, hl=None):
    n = waiting_count(rows)
    calm = " calm" if n == 0 else ""
    if mode == "loading":
        return '<div class="dock calm" aria-busy="true"><div class="one"><span class="skel" style="width:60%;height:14px"></span></div></div>'
    if mode == "error":
        return (f'<div class="dock calm"><div class="one"><span class="main"><span class="st gone">{ic("warn", "xs")}Could not load open decisions.</span></span>'
                f'<button class="btn outline sm">{ic("retry", "xs")}Retry</button></div></div>')
    if expanded:
        shown = rows[:4]
        more = (f'<div class="more"><button class="tgl" aria-expanded="false">{ic("down", "xs")}Show {len(rows)-4} more</button></div>' if len(rows) > 4 else "")
        live_html = f'<span class="muted t-caption" role="status">{live}</span>' if live else ""
        cnt = f'<span class="cnt tnum" style="height:20px;min-width:20px;padding:0 6px;border-radius:999px;background:color-mix(in oklch, var(--warning) 22%, white);color:var(--warning-text);font-size:12px;font-weight:600;display:inline-grid;place-items:center">{len(rows)}</span>'
        head_html = (f'<div class="one"><span class="main"><b class="t-label">Open decisions</b>{cnt}</span>{live_html}'
                     f'<button class="tgl" aria-expanded="true">{ic("down", "xs")}Hide</button></div>')
        lst = f'<ul class="list">{"".join(row(r, "hl" if hl == r["key"] else "") for r in shown)}</ul>{more}'
    else:
        lead = next((r for r in rows if r["state"] == "gone"), None) or next((r for r in rows if r["state"] == "waiting"), rows[0])
        label, icon = (lead["note"], "x") if lead["state"] == "gone" else STATE_TXT[lead["state"]]
        st = lead["state"]
        others = len(rows) - 1
        more_txt = f"+{others} more" if others else "Details"
        live_html = f'<span class="muted t-caption" role="status">{live}</span>' if live else ""
        lead_html = (f'<span class="main"><a href="#{lead["key"]}"><span class="st {st}">{ic(icon, "xs")}{E(label)}</span>'
                     f'<span class="key">{lead["key"]}</span><span class="ttl">{E(lead["title"])}</span>'
                     f'<span class="cls">{E(lead["cls"])}</span><span class="age tnum">{lead["age"]}</span></a></span>')
        head_html = f'<div class="one">{lead_html}{live_html}<button class="tgl" aria-expanded="false">{ic("up", "xs")}{more_txt}</button></div>'
        lst = ""
    ban = ""
    if mode == "offline":
        ban = f'<div class="ban" role="status">{ic("wifi", "xs")}<span>Not live. Reconnecting; rows may be out of date.</span></div>'
    return f'<section class="dock{calm}" aria-label="Open decisions">{head_html}{lst}{ban}</section>'


def build_c():
    r3 = ROWS[:3]
    frames = [
        frame("1 · Default — leads with the oldest ask waiting on you", "one line above the composer, where the answer is typed; “+2 more” opens the list", pane(dock=dock(r3))),
        frame("2 · Expanded", "list opens upward from the dock; 4 rows then a scroll", pane(dock=dock(r3, expanded=True))),
        frame("3 · Five requests — expanded", "4 rows visible, the rest scroll inside the list", pane(dock=dock(ROWS, expanded=True))),
        frame("4 · A request is cancelled — lead row", "dock shows the cancelled row for 5 s with the reason, then moves on to the next waiting ask", pane(dock=dock([GONE, ROWS[0], ROWS[2]]))),
        frame("5 · Only answered rows remain", "dock goes calm: no warning tint", pane(dock=dock([ROWS[2]]))),
        frame("6 · Empty — dock is not rendered", "composer sits where it does today", pane()),
        frame("7 · Loading", "one skeleton line; reserved height", pane(dock=dock([], mode="loading"))),
        frame("8 · Load failed", "one line with Retry", pane(dock=dock([], mode="error"))),
        frame("9 · Disconnected — expanded", "rows kept, marked not live", pane(dock=dock(r3, expanded=True, mode="offline"))),
        frame("10 · Long title", "title truncates; key and state stay", pane(dock=dock([LONG, ROWS[1]]))),
    ]
    return page("C — Dock above the composer", "One line above the composer shows the oldest ask waiting on you. The rest fold into “+N more”. The ask and the answer box sit together.", frames)


for name, fn in (("A", build_a), ("B", build_b), ("C", build_c)):
    (OUT / f"{name}.html").write_text(fn())
print("ok")
