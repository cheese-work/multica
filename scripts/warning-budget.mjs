import { execFileSync } from "node:child_process"
import { appendFileSync, writeFileSync } from "node:fs"
import { pathToFileURL } from "node:url"
import { stripVTControlCharacters } from "node:util"

function normalize(message) {
  return stripVTControlCharacters(message)
    .replace(/^\d{4}-\d\d-\d\dT\S+\s+/, "")
    .replace(/^\d{4}\/\d\d\/\d\d \d\d:\d\d:\d\d\s+/, "")
    .replace(/_\d{10,}_\d+/g, "_<id>")
    .replace(/\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/gi, "<uuid>")
    .replace(/\b\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)\b/g, "<time>")
    .replace(/\b(?:duration|duration_ms|request_id)=(?:"[^"]*"|\S+)/g, (field) => `${field.split("=")[0]}=<runtime>`)
    .replace(/(\/tmp\/Test[^/\s"]*?)\d{6,}(?=\/)/g, "$1<id>")
    .replace(/(\/tmp\/[A-Za-z0-9_-]+\.)[A-Za-z0-9]{6,}(?=\/)/g, "$1<id>")
    .replace(/((?:https?:\/\/|tcp )[^/:\s"\\]+):\d{2,5}\b/g, "$1:<port>")
    .replace(/^.*?##\[(warning|notice)\]/i, "")
    .replace(/\(node:\d+\)\s*/g, "")
    .replace(/#\d+\s+\d+\.\d+\s+/g, "")
    .replace(/^\d+:[A-Z] \d{1,2} [A-Za-z]{3} \d{4} \d\d:\d\d:\d\d\.\d+ /, "Redis ")
    .replace(/\s+/g, " ").trim()
}

function sourcePath(path) {
  return path.replaceAll("\\", "/").replace(/^.*\/multica\/multica\//, "").replace(/^\.\//, "")
}

export function warningCounts(jobs) {
  const counts = {}
  for (const job of jobs) {
    const logs = {}
    const annotations = {}
    const eslintMessages = new Map()
    let eslintFile = ""
    let commandHeader = false
    for (const line of stripVTControlCharacters(job.log).split("\n")) {
      if (/##\[group\]Run /.test(line)) {
        commandHeader = true
        eslintFile = ""
        continue
      }
      if (commandHeader) {
        if (/##\[endgroup\]/.test(line)) commandHeader = false
        continue
      }
      if (/^(?:\d{4}-\d\d-\d\dT\S+\s+)?\d{4}\/\d\d\/\d\d \d\d:\d\d:\d\d(?:\.\d+)?\s+WARN\s/.test(line)) continue
      const message = normalize(line)
      if (/^(?:\/|[A-Za-z]:\/|(?:\.\.?\/)?(?:apps|packages|server|scripts)\/).+\.[cm]?[jt]sx?$/.test(message)) {
        eslintFile = sourcePath(message)
        continue
      }
      if (/##\[(command|group)\]|^Run |^\s*(?:echo |printf )/.test(message)) continue
      if (/^(?:# Subtest:|(?:not )?ok \d+\b)/.test(message)) continue
      if (!/##\[(warning|notice)\]|::(?:warning|notice)::|\b(?:DeprecationWarning|ExperimentalWarning|[Ww]arning[:!]|WARN(?:ING)?\b)|\bnpm warn\b|⚠|╭ Warning |Found \d+ warnings|\d+:\d+\s+warning\b/.test(line)) continue
      let warning = message.replace(/^::(?:warning|notice)::/i, "")
      const eslint = warning.match(/^\d+:\d+\s+warning\s+(.+)$/)
      if (eslint) {
        const rule = eslintFile ? eslint[1].match(/\s([@\w-]+(?:\/[\w-]+)*)$/)?.[1] : undefined
        const detail = rule ? eslint[1].slice(0, -rule.length).trim() : eslint[1]
        warning = `${eslintFile ? `${eslintFile}: ` : ""}${rule ?? `warning ${detail}`}`
        if (eslintFile) eslintMessages.set(`${eslintFile}: ${detail}`, warning)
      }
      logs[warning] = (logs[warning] ?? 0) + 1
    }
    for (const annotation of job.annotations) {
      if (!["warning", "notice"].includes(annotation.annotation_level)) continue
      const message = normalize(annotation.message)
      const file = /\.[cm]?[jt]sx?$/.test(annotation.path ?? "") ? sourcePath(annotation.path) : ""
      const warning = file ? eslintMessages.get(`${file}: ${message}`) ?? `${file}: ${message}` : message
      annotations[warning] = (annotations[warning] ?? 0) + 1
    }
    for (const warning of new Set([...Object.keys(logs), ...Object.keys(annotations)])) {
      counts[`${job.name}: ${warning}`] = Math.max(logs[warning] ?? 0, annotations[warning] ?? 0)
    }
  }
  return counts
}

export function warningRegressions(baseline, current) {
  return Object.entries(current)
    .filter(([warning, count]) => count > (baseline[warning] ?? 0))
    .map(([warning, count]) => ({ warning, baseline: baseline[warning] ?? 0, current: count }))
}

function api(endpoint, json = true) {
  const response = execFileSync("gh", ["api", endpoint, ...(json ? [] : ["--allow-escape-sequences"])], { encoding: "utf8", maxBuffer: 128 * 1024 * 1024 })
  return json ? JSON.parse(response) : response
}

function pages(endpoint, key) {
  const results = []
  for (let page = 1; ; page++) {
    const response = api(`${endpoint}${endpoint.includes("?") ? "&" : "?"}per_page=100&page=${page}`)
    const items = key ? response[key] : response
    results.push(...items)
    if (items.length < 100) return results
  }
}

export function runBudget() {
  const repository = process.env.GITHUB_REPOSITORY
  const runId = process.env.GITHUB_RUN_ID
  if (!/^cheese-work\/multica$/.test(repository ?? "") || !/^\d+$/.test(runId ?? "")) throw new Error("Expected a Multica CI run")
  const prefix = `repos/${repository}`
  const current = api(`${prefix}/actions/runs/${runId}`)
  function jobs(run) {
    return pages(`${prefix}/actions/runs/${run.id}/jobs?filter=latest`, "jobs")
      .filter((job) => job.name !== "warning-budget" && job.conclusion !== "skipped")
  }
  function collect(job) {
    if (job.status !== "completed") throw new Error(`Job ${job.name} has not completed`)
    const checkId = job.check_run_url.split("/").at(-1)
    if (!/^\d+$/.test(checkId)) throw new Error(`Invalid check run for ${job.name}`)
    return { name: job.name, log: api(`${prefix}/actions/jobs/${job.id}/logs`, false), annotations: pages(`${prefix}/check-runs/${checkId}/annotations`) }
  }
  const currentJobs = jobs(current).map(collect)
  const zeroBaselineJobs = new Set(["migration-prefix-check", "image-budget", "go-lint"])
  const pending = new Set(currentJobs.map(({ name }) => name).filter((name) => !zeroBaselineJobs.has(name)))
  const jobBaselines = Object.fromEntries(currentJobs.map(({ name }) => [name, null]))
  const baselineJobs = []
  let baseline
  for (let page = 1; pending.size && page <= 10; page++) {
    const runs = api(`${prefix}/actions/workflows/${current.workflow_id}/runs?branch=main&status=success&created=${encodeURIComponent(`<${current.created_at}`)}&per_page=100&page=${page}`).workflow_runs
    const trusted = runs.filter((run) => String(run.id) !== runId && run.created_at < current.created_at && run.head_branch === "main" && ["push", "schedule", "workflow_dispatch"].includes(run.event))
      .sort((first, second) => second.created_at.localeCompare(first.created_at))
    for (const run of trusted) {
      baseline ??= run
      for (const job of jobs(run)) {
        const name = pending.has(job.name) ? job.name : job.name.replace(/^(cursor-background-lifecycle|installer) \(ubuntu-latest\)$/, "$1 (ubuntu-24.04)")
        if (!pending.has(name) || job.status !== "completed" || job.conclusion !== "success") continue
        baselineJobs.push({ ...collect(job), name })
        jobBaselines[name] = { run: run.html_url, sha: run.head_sha, job: job.name }
        pending.delete(name)
      }
      if (!pending.size) break
    }
    if (runs.length < 100) break
  }
  if (pending.size) throw new Error(`No successful main CI job baseline is available within the latest 1000 runs for: ${[...pending].join(", ")}`)
  const baselineCounts = warningCounts(baselineJobs)
  const currentCounts = warningCounts(currentJobs)
  const regressions = warningRegressions(baselineCounts, currentCounts)
  const report = { baseline_run: baseline?.html_url ?? null, current_run: current.html_url, baseline_sha: baseline?.head_sha ?? null, current_sha: current.head_sha, job_baselines: jobBaselines, zero_baseline_jobs: currentJobs.map(({ name }) => name).filter((name) => zeroBaselineJobs.has(name)), baseline: baselineCounts, current: currentCounts, annotations: currentJobs.map(({ name, annotations }) => ({ name, annotations })), regressions }
  writeFileSync("warning-budget-report.json", `${JSON.stringify(report, null, 2)}\n`)
  console.log(JSON.stringify(report, null, 2))
  if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, `Warning budget against per-job main baselines: ${regressions.length} new warning signatures. See job_baselines and zero_baseline_jobs in the report.\n`)
  if (regressions.length) process.exitCode = 1
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try { runBudget() } catch (error) { console.error(error.message); process.exitCode = 1 }
}
