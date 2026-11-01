# Sandbox connectivity gate and real-CLI smoke (CHE-2216 A7c)

The A6 entry gate needs two pieces of evidence for the isolated network path. Plan: CHE-2216, section 4.

## 1. Default gate (fakes, runs in CI on X99)

```
(cd server && go test ./internal/daemon/sandbox ./internal/daemon -run TestGate -count=1 -v)
```

Each test runs a fake agent (the test binary) inside the real sandbox, bridge and egress proxy:

| Test | Proves |
| --- | --- |
| `TestGateProviderReachableThroughIsolatedPath` | request and streamed (SSE) reply to a fake provider; the server holds events 1 and 2 until the client acks event 0, so a buffered tunnel fails |
| `TestGateMulticaHTTPAndWebSocketReachableThroughIsolatedPath` | authenticated HTTP and a `wss` echo to a fake Multica server |
| `TestGateEveryMergedRuntimeMCPServerReachableThroughIsolatedPath` (package `daemon`) | the config from the daemon's own `mergeRuntimeAndAgentMcpConfig` (runtime `~/.claude.json` plus agent layer): each server is started or called with its `env` and `headers`; the test logs the servers tested and fails if one is missing or untested |
| `TestGateRemoteMCPServerNeedsItsHeaders`, `TestGateStdioMCPServerNeedsItsEnv` (package `daemon`) | the fakes refuse a call without the headers or env, so the test above fails when the probe drops them |
| `TestGateProxyLogShowsEveryDestination` | the proxy destination log lists every service the run used |
| `TestGateNoDirectRouteToAnyService` | a direct dial, and a client that ignores the proxy variables, find no route and leave no proxy log line |
| `TestGateProxyDeathMidRunLeavesNoRoute` | after the proxy goes away, proxied and direct connections both fail |
| `TestGateBridgeBinaryMissingStopsRunBeforeAgent` | no bridge, no run, no host-network fallback |
| `TestGatePolicyRefusalReachesTheClient` | a policy refusal arrives as the proxy's `403` |
| `TestGateCredentialCopyCannotBeChangedInsideSandbox` | a provider credential copy bound read-only in the run HOME refuses chmod, append, rename, delete and replace |

## 2. Real-CLI smoke (one per provider; needs authorization)

The dev team leader authorizes the run under the repo's `agentintegration` rule. It spends one small model call per provider. Default tests never run it.

The smoke authenticates both CLIs with the model gateway named by `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` in the environment of `go test` (on X99, the c00 sub2api route). Claude gets both variables. Codex gets the same token as `OPENAI_API_KEY` and the same base URL through `-c model_providers.smoke=...`. The token is in the environment only: not in argv, not in the repo, not in the log (failure output is scrubbed). The proxy allows exactly the gateway `host:port`; every other destination is refused, so `allowed=false` lines in the log are expected. The credential file in the run HOME is a placeholder that keeps the read-only bind and its mutation probe in the smoke.

1. Confirm the authorization on the issue and that the host has native `claude` and `codex` binaries and the two variables set (names only; never print the values).
2. Put the real binaries (not wrapper scripts, which exec outside the sandbox) on `PATH`, for example links in a scratch directory. If a CLI needs a runtime outside `/usr`, list its install paths, colon-separated, in `MULTICA_SMOKE_READONLY`. `MULTICA_SMOKE_CODEX_MODEL` overrides the Codex model (default `gpt-6-luna`; `gpt-6-astra` and `gpt-5.6-terra` need a composite route on the X99 gateway).
3. Run (with the variable set, a missing CLI, token or sandbox fails the run; nothing is skipped):

```
set -o pipefail
(cd server && MULTICA_RUN_REAL_AGENT_SMOKE=1 go test -tags=agentintegration \
  ./internal/daemon/sandbox -run TestSmokeRealCLIThroughIsolatedPath -count=1 -v) | tee /tmp/sandbox-smoke.log
echo "go test exit: ${PIPESTATUS[0]}"
```

4. Pass needs all of: `go test` exit 0 (the `go test exit:` line, which `tee` would otherwise mask), `--- PASS: TestSmokeRealCLIThroughIsolatedPath/claude` and `.../codex` in the log, and `SMOKE RESULT claude=PASS codex=PASS`. The marker alone is not evidence; a log with one provider only is not accepted. Per provider: the credential copy refuses chmod and append from inside the sandbox; the CLI exits 0 inside the sandbox (a run over 3 minutes is killed and counts as a failure); the log prints `proxy log:` lines and the gateway `host:port` shows `allowed=true`; a direct connection to `1.1.1.1:443` from the same sandbox is refused. The test does not check what the model answers.
5. Attach `/tmp/sandbox-smoke.log` to the A6 evidence. The token never appears in it; the test logs destination hosts and ports only.
