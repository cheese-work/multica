const parameters = new URLSearchParams(location.search);
const direction = document.body.dataset.direction;
let showDiff = parameters.get("diff") === "on";
let selectedBase = parameters.get("base") || "2";
let state = parameters.get("state") || "ready";
let platform = parameters.get("platform") || "web";
const expanded = parameters.get("picker") === "open";

const currentText = [
  "# Plan: revision review",
  "",
  "## Goal",
  "Review each plan without scrolling through full copies.",
  "",
  "## Delivery",
  "- Attach each revision as plan-rev-N.md.",
  "- Default the comparison to the previous revision.",
  "- Allow any earlier revision as the base.",
  "- Keep the source issue's read permission.",
  "",
  "## Verification",
  "- Capture web and desktop before review.",
];
const previousText = [
  "# Plan: revision review", "", "## Goal",
  "Review each plan without scrolling through full copies.", "", "## Delivery",
  "- Post the entire revision as a comment.", "", "## Verification",
  "- Capture web before review.",
];
const firstText = [
  "# Plan: revision review", "", "## Goal", "Put plan text in an issue comment.",
  "", "## Delivery", "- Post the entire revision as a comment.",
];

function escapeText(text) {
  return text.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&quot;");
}

function line(text, kind, before, after, split = false) {
  const marker = kind === "add" ? "+" : kind === "remove" ? "−" : " ";
  const numbers = split
    ? `<span class="number">${before || after || ""}</span>`
    : `<span class="number">${before || ""}</span><span class="number">${after || ""}</span>`;
  return `<div class="line ${kind}">${numbers}<span class="marker" aria-label="${kind === "add" ? "Added" : kind === "remove" ? "Removed" : "Unchanged"}">${marker}</span><span class="code">${escapeText(text) || " "}</span></div>`;
}

function fixtureChanges() {
  const rows = [];
  let before = 1;
  let after = 1;
  function append(text, kind = "context") {
    rows.push({ text, kind, before: kind === "add" ? "" : before++, after: kind === "remove" ? "" : after++ });
  }
  if (selectedBase === "1") {
    currentText.slice(0, 3).forEach((text) => append(text));
    append(firstText[3], "remove");
    append(currentText[3], "add");
    currentText.slice(4, 6).forEach((text) => append(text));
    append(firstText[6], "remove");
    currentText.slice(6).forEach((text) => append(text, "add"));
  } else {
    currentText.slice(0, 6).forEach((text) => append(text));
    append(previousText[6], "remove");
    currentText.slice(6, 10).forEach((text) => append(text, "add"));
    currentText.slice(10, 12).forEach((text) => append(text));
    append(previousText[9], "remove");
    append(currentText[12], "add");
  }
  return rows;
}

function unifiedDiff() {
  const output = fixtureChanges().map((row) => line(row.text, row.kind, row.before, row.after)).join("");
  return `<section class="sheet" aria-label="Revision changes"><div class="diff-heading">Revision ${selectedBase.startsWith("2") ? "2" : "1"} → Revision 3 · Markdown source</div><div class="diff-lines">${output}</div></section>`;
}

function splitDiff() {
  const changes = fixtureChanges();
  const baseLines = changes.map((row) => row.kind === "add" ? line("", "context", "", "", true) : line(row.text, row.kind, row.before, "", true)).join("");
  const currentLines = changes.map((row) => row.kind === "remove" ? line("", "context", "", "", true) : line(row.text, row.kind, "", row.after, true)).join("");
  return `<div class="split"><section class="sheet"><div class="diff-heading">Base · plan-rev-${selectedBase.startsWith("2") ? "2" : "1"}.md</div><div class="diff-lines">${baseLines}</div></section><section class="sheet"><div class="diff-heading">Current · plan-rev-3.md</div><div class="diff-lines">${currentLines}</div></section></div><div class="mobile-diff">${unifiedDiff()}</div>`;
}

function documentSheet() {
  return `<article class="sheet prose"><h1>Plan: revision review</h1><p class="caption">Revision 3 · plan-rev-3.md</p><h2>Goal</h2><p>Review each plan without scrolling through full copies.</p><h2>Delivery</h2><ul><li>Attach each revision as <code>plan-rev-N.md</code>.</li><li>Default the comparison to the previous revision.</li><li>Allow any earlier revision as the base.</li><li>Keep the source issue's read permission.</li></ul><div class="note">Each revision stays pinned to its immutable attachment ID.</div><h2>Verification</h2><p>Capture web and desktop before review.</p></article>`;
}

function baseControl() {
  if (!showDiff) return "";
  const disabled = ["first", "loading", "offline"].includes(state);
  const ambiguous = state === "ambiguous";
  const options = ambiguous
    ? `<option value="">Choose an attachment</option><option value="2a" title="4cc00000-0000-4000-8000-00000000e92a">Revision 2 · 4cc0…e92a</option><option value="2b" title="9e630000-0000-4000-8000-00000000c087">Revision 2 · 9e63…c087</option><option value="1">Revision 1 · plan-rev-1.md</option>`
    : `<option value="2">Revision 2 · previous</option><option value="1">Revision 1 · plan-rev-1.md</option>`;
  return `<div class="base-control ${expanded ? "expanded" : ""}"><label for="base">Compare with</label><select id="base" ${expanded ? 'size="3"' : ""} ${disabled ? 'disabled aria-describedby="reason"' : ""}>${options}</select></div>`;
}

function compareControls() {
  const disabled = ["first", "loading", "offline"].includes(state);
  const reasons = { first: "No earlier revision on this issue.", loading: "Loading revision history…", offline: "Offline. Reconnect to change the comparison.", ambiguous: "Two attachments are labelled Revision 2. Choose an exact attachment." };
  const changes = fixtureChanges();
  const added = changes.filter((row) => row.kind === "add").length;
  const removed = changes.filter((row) => row.kind === "remove").length;
  return `<label class="switch-label"><input id="diff" type="checkbox" role="switch" ${showDiff ? "checked" : ""} ${disabled ? 'disabled aria-describedby="reason"' : ""}> Diff</label>${baseControl()}${showDiff && !disabled && state !== "ambiguous" ? `<div class="summary" aria-label="Change summary"><span class="added-count">+${added} added lines</span><span class="removed-count">−${removed} removed lines</span></div>` : ""}${reasons[state] ? `<span class="reason" id="reason">${reasons[state]}</span>` : ""}`;
}

function content() {
  if (state === "loading") return `<section class="sheet state-panel" role="status"><h2>Loading revision history…</h2><p>Opening plan-rev-3.md.</p></section>`;
  if (state === "empty") return `<section class="sheet state-panel"><h2>This revision is empty</h2><p>plan-rev-3.md contains no text.</p></section>`;
  if (state === "error") return `<div class="notice" role="alert"><strong>Could not load Revision 2</strong><span>The file may be unavailable or access may have changed. The current revision is still readable.</span><br><button id="retry">Retry comparison</button></div>${documentSheet()}`;
  if (state === "offline") return `<div class="notice" role="status"><strong>Offline · showing the cached current revision</strong><span>Reconnect to compare or load a different revision.</span></div>${documentSheet()}`;
  if (state === "ambiguous" && !selectedBase) return documentSheet();
  if (!showDiff || state === "first") return documentSheet();
  return direction === "B" ? splitDiff() : unifiedDiff();
}

function render() {
  const focusedControl = document.activeElement?.id;
  const chrome = platform === "desktop"
    ? `<div class="desktop-tabs" aria-label="Desktop app mockup"><span class="caption">Multica desktop</span><span class="tab">plan-rev-3.md</span></div>`
    : `<div class="browser" aria-label="Web browser mockup"><span class="caption">Web</span><div class="address">/cheese-work/attachments/fixture-revision-3/preview</div></div>`;
  const toolbar = `<div class="${direction === "B" ? "rail" : "toolbar"}">${direction === "B" ? "<h2>Revisions</h2>" : ""}${compareControls()}</div>`;
  document.querySelector("#preview").innerHTML = `${chrome}<header class="titlebar"><div class="file"><span class="filename" title="plan-rev-3.md">plan-rev-3.md</span><div class="context">CHE-2124 · Revision 3 <span class="pill">Read-only</span></div></div><button type="button" disabled aria-describedby="download-reason">Download</button></header><div class="${direction === "B" ? "layout-rail" : "layout"}">${toolbar}<div class="stage">${content()}<p class="caption" id="download-reason" style="max-width:900px;margin:18px auto">Design preview: Download is disabled. No real attachment is loaded.</p></div></div><aside class="harness" aria-label="Design preview controls"><strong>Design ${direction} · not product UI</strong><label>State <select id="state"><option value="ready">Ready</option><option value="loading">Loading</option><option value="first">First revision</option><option value="empty">Empty file</option><option value="error">Base load failed</option><option value="offline">Offline</option><option value="ambiguous">Duplicate base</option></select></label><label>Surface <select id="platform"><option value="web">Web mockup</option><option value="desktop">Desktop mockup</option></select></label><span>Fixture content only. No product acceptance evidence.</span></aside>`;
  document.querySelector("#diff").addEventListener("change", (event) => { showDiff = event.target.checked; render(); });
  const base = document.querySelector("#base");
  if (base) {
    base.value = selectedBase;
    base.addEventListener("change", (event) => { selectedBase = event.target.value; render(); });
  }
  const stateControl = document.querySelector("#state");
  stateControl.value = state;
  stateControl.addEventListener("change", (event) => { state = event.target.value; if (state === "ambiguous") selectedBase = ""; render(); });
  const surfaceControl = document.querySelector("#platform");
  surfaceControl.value = platform;
  surfaceControl.addEventListener("change", (event) => { platform = event.target.value; render(); });
  document.querySelector("#retry")?.addEventListener("click", () => { state = "ready"; render(); });
  const restoredControl = focusedControl && document.getElementById(focusedControl);
  if (restoredControl && !restoredControl.disabled) restoredControl.focus({ preventScroll: true });
}

if (state === "ambiguous") selectedBase = "";
render();
