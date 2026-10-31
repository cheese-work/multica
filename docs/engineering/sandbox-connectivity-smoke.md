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

The dev team leader authorizes the run under the repo's `agentintegration` rule. It spends one small model call per provider and reads the host's Claude and Codex credentials. Default tests never run it.

1. Confirm the authorization on the issue and that the host has `claude` and `codex` installed and logged in.
2. If a CLI is a script that needs a runtime outside `/usr`, list its install paths, colon-separated, in `MULTICA_SMOKE_READONLY`. Paths under `~/.claude` and `~/.codex` are refused by design.
3. Run (with the variable set, a missing CLI, credential or sandbox fails the run; nothing is skipped):

```
(cd server && MULTICA_RUN_REAL_AGENT_SMOKE=1 go test -tags=agentintegration \
  ./internal/daemon/sandbox -run TestSmokeRealCLIThroughIsolatedPath -count=1 -v) | tee /tmp/sandbox-smoke.log
```

4. Pass means the log ends with `SMOKE RESULT claude=PASS codex=PASS`; a log with one provider only is not accepted evidence. Per provider: the credential copy refuses chmod and append from inside the sandbox; the CLI exits 0 inside the sandbox (a run over 3 minutes is killed and counts as a failure); the log prints `proxy log:` lines for its traffic; a direct connection to `1.1.1.1:443` from the same sandbox is refused. The test does not check what the model answers.
5. Attach `/tmp/sandbox-smoke.log` to the A6 evidence. Credential values never appear in it; the test logs destination hosts and ports only.
