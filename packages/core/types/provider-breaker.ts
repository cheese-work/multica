/** `closed` admits work; `open` refuses it; `half_open` admits one canary. */
export type ProviderBreakerState = "closed" | "open" | "half_open";

export interface ProviderBreaker {
  provider: string;
  /** An unknown server value parses to `open`: refusing is the safe reading. */
  state: ProviderBreakerState;
  open_reason: string;
  failure_count: number;
  opened_at: string | null;
  next_probe_at: string | null;
  last_success_at: string | null;
  trip_evidence: string[];
}
