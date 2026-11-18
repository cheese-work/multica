import type { AgentTask } from "../types";

// Both lists mirror runGateInFlight in server/internal/service/run_gate_join.go
// (server/internal/service/web_client_lists_contract_test.go keeps them equal).

/** A run in any of these states is still in flight. */
export const ACTIVE_TASK = new Set<AgentTask["status"]>(["queued", "dispatched", "waiting_local_directory", "deferred", "running"]);

/** In flight but the agent session has not started, so there is no transcript. */
export const PRE_RUN_STATUSES: AgentTask["status"][] = ["queued", "dispatched", "waiting_local_directory", "deferred"];
