import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const ISSUE_KEY = /\b[a-z][a-z0-9]{0,9}-\d+\b/gi;
const CLOSE_DIRECTIVE = /\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)[:\s]+([a-z][a-z0-9]{0,9}-\d+)\b/gi;
const REFERENCE_DIRECTIVE = /\b(?:refs?|part\s+of)[:\s]+([a-z][a-z0-9]{0,9}-\d+)\b/gi;
const FENCE_START = /^ {0,3}(?:(`{3,})[^`\n]*|(~{3,})[^\n]*)$/m;

function withoutMarkdownCode(body) {
  let remaining = body.replace(/\r\n?/g, "\n");
  let visible = "";

  while (true) {
    const opening = FENCE_START.exec(remaining);
    if (!opening) {
      visible += remaining;
      break;
    }

    const delimiter = opening[1] ?? opening[2];
    visible += `${remaining.slice(0, opening.index)} `;
    const afterOpening = remaining.slice(opening.index + opening[0].length);
    const closingFence = new RegExp(
      `^ {0,3}${delimiter[0]}{${delimiter.length},}[ \\t]*$`,
      "m",
    ).exec(afterOpening);
    remaining = closingFence
      ? afterOpening.slice(closingFence.index + closingFence[0].length)
      : "";
  }

  let stripped = "";
  let index = 0;
  while (index < visible.length) {
    if (visible[index] !== "`") {
      stripped += visible[index];
      index += 1;
      continue;
    }

    const start = index;
    while (index < visible.length && visible[index] === "`") index += 1;
    const delimiterLength = index - start;
    let closeIndex = -1;
    let closeEnd = -1;
    let searchIndex = index;

    while (searchIndex < visible.length) {
      if (visible[searchIndex] !== "`") {
        searchIndex += 1;
        continue;
      }

      let runEnd = searchIndex;
      while (runEnd < visible.length && visible[runEnd] === "`") runEnd += 1;
      if (runEnd - searchIndex === delimiterLength) {
        closeIndex = searchIndex;
        closeEnd = runEnd;
        break;
      }
      searchIndex = runEnd;
    }

    if (closeIndex === -1) {
      stripped += visible.slice(start, index);
      continue;
    }

    stripped += " ";
    index = closeEnd;
  }

  return stripped;
}

function issueKeys(text) {
  const keys = new Set();
  for (const match of text.matchAll(ISSUE_KEY)) {
    keys.add(match[0].toUpperCase());
  }
  return [...keys];
}

export function checkCloseTarget({ title = "", body = "", headRef = "", author = "" } = {}) {
  const normalizedHeadRef = typeof headRef === "string" ? headRef : "";
  if (normalizedHeadRef.startsWith("upstream-sync/") || author === "dependabot[bot]") {
    return { closeTargets: [], strays: [], skipped: true };
  }

  const closeTargets = new Set();
  const visibleTitle = withoutMarkdownCode(typeof title === "string" ? title : "");
  const visibleBody = withoutMarkdownCode(typeof body === "string" ? body : "");

  const visibleText = `${visibleTitle}\n${visibleBody}`;
  const referenceTargets = [...new Set(
    [...visibleText.matchAll(REFERENCE_DIRECTIVE)].map((match) => match[1].toUpperCase()),
  )];
  for (const directive of visibleText.matchAll(CLOSE_DIRECTIVE)) {
    closeTargets.add(directive[1].toUpperCase());
  }

  const strays = [];
  const linkedKeys = new Set();
  const linkedEntries = [];
  const hasExplicitCloseTarget = closeTargets.size > 0;
  for (const { value, where } of [
    { value: typeof title === "string" ? title : "", where: "title" },
    { value: normalizedHeadRef, where: "branch" },
  ]) {
    for (const key of issueKeys(value)) {
      if (!linkedKeys.has(key)) linkedEntries.push({ key, where });
      linkedKeys.add(key);
      if (hasExplicitCloseTarget && !closeTargets.has(key)) {
        strays.push({ key, where });
      }
    }
  }

  if (!hasExplicitCloseTarget && linkedKeys.size > 1) {
    strays.push(...linkedEntries);
  }

  if (linkedKeys.size > 0) {
    for (const key of closeTargets) {
      if (!linkedKeys.has(key)) strays.push({ key, where: "close target" });
    }
  }

  return {
    closeTargets: [...closeTargets],
    strays,
    referenceTargets,
    missingIssueLink: ![...linkedKeys, ...closeTargets, ...referenceTargets].some((key) => /^CHE-\d+$/.test(key)),
  };
}

function run(args) {
  if (args.length !== 2 || args[0] !== "--event" || !args[1]) {
    console.error("Usage: node scripts/pr-close-target-check.mjs --event <path>");
    return 2;
  }

  let event;
  try {
    event = JSON.parse(readFileSync(args[1], "utf8"));
  } catch (error) {
    console.error(`Unable to read pull request event: ${error.message}`);
    return 2;
  }

  const pullRequest = event?.pull_request;
  if (
    !pullRequest
    || typeof pullRequest.title !== "string"
    || typeof pullRequest.head?.ref !== "string"
  ) {
    console.error("Pull request event must include title and head.ref fields");
    return 2;
  }

  const result = checkCloseTarget({
    title: pullRequest.title,
    body: typeof pullRequest.body === "string" ? pullRequest.body : "",
    headRef: pullRequest.head.ref,
    author: pullRequest.user?.login,
  });

  if (result.skipped) {
    console.log("Close-target check skipped for upstream-sync head or dependabot author");
    return 0;
  }

  if (result.missingIssueLink) {
    console.log("::error::PR has no issue link. Add Part of CHE-N on its own body line for a partial PR, or add the issue key to the title when the PR delivers the whole issue.");
    return 1;
  }

  if (result.strays.length > 0) {
    const declared = result.closeTargets.length > 0
      ? result.closeTargets.join(", ")
      : "none";
    for (const { key, where } of result.strays) {
      const message = where === "close target"
        ? `${key} is a PR close target not named by title or branch; linked issue(s): ${[...new Set([...issueKeys(pullRequest.title), ...issueKeys(pullRequest.head.ref)])].join(", ")}`
        : `${key} in PR ${where} is not declared as a close target; declared target(s): ${declared}`;
      console.log(
        `::error::${message}`,
      );
    }
    return 1;
  }

  const number = Number.isInteger(pullRequest.number) ? `#${pullRequest.number}` : "PR";
  const linkedKeys = [...new Set([
    ...issueKeys(pullRequest.title),
    ...issueKeys(pullRequest.head.ref),
  ])];
  const targets = result.closeTargets.length > 0
    ? result.closeTargets
    : linkedKeys;
  const targetSummary = result.closeTargets.length > 0
    ? targets.join(", ")
    : targets.length === 1
      ? `${targets[0]} from title/branch`
      : "none";
  const referenceSummary = result.referenceTargets.length > 0
    ? `; reference link(s): ${result.referenceTargets.join(", ")}`
    : "";
  console.log(`${number} close-target check passed; effective target(s): ${targetSummary}${referenceSummary}`);
  return 0;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exitCode = run(process.argv.slice(2));
}
