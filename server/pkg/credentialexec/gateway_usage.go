package credentialexec

import "math"

type GatewayUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

type UsageSnapshot struct {
	Models   map[string]GatewayUsage
	Complete bool
}

func (boundary *Boundary) UsageSnapshot() UsageSnapshot {
	if boundary == nil {
		return UsageSnapshot{}
	}
	boundary.mutex.Lock()
	defer boundary.mutex.Unlock()
	snapshot := UsageSnapshot{Complete: len(boundary.usage) != 0 && !boundary.requestActive && boundary.stopErrorLocked() == nil}
	if len(boundary.usage) != 0 {
		snapshot.Models = make(map[string]GatewayUsage, len(boundary.usage))
		for model, usage := range boundary.usage {
			snapshot.Models[model] = usage
		}
	}
	return snapshot
}

func (boundary *Boundary) recordGatewayUsage(protocol *gatewayProtocol) error {
	if protocol == nil || protocol.path == "/v1/models" || protocol.path == "/v1/messages/count_tokens" {
		return nil
	}
	if protocol.model == "" || len(protocol.model) > 1024 || protocol.responseID == "" {
		return ErrOutcomeUnknown
	}
	boundary.mutex.Lock()
	defer boundary.mutex.Unlock()
	if boundary.stopErrorLocked() != nil {
		return ErrOutcomeUnknown
	}
	prior, exists := boundary.usage[protocol.model]
	if !exists && len(boundary.usage) >= 128 {
		return ErrOutcomeUnknown
	}
	counts := []int64{prior.InputTokens, prior.OutputTokens, prior.CacheReadTokens, prior.CacheWriteTokens}
	for _, field := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		count, exists := protocol.usage[field]
		if !exists {
			return ErrOutcomeUnknown
		}
		counts = append(counts, count)
	}
	total := int64(0)
	for _, count := range counts {
		if count < 0 || count > math.MaxInt64-total {
			return ErrOutcomeUnknown
		}
		total += count
	}
	if boundary.usage == nil {
		boundary.usage = make(map[string]GatewayUsage)
	}
	boundary.usage[protocol.model] = GatewayUsage{
		InputTokens:      prior.InputTokens + protocol.usage["input_tokens"],
		OutputTokens:     prior.OutputTokens + protocol.usage["output_tokens"],
		CacheReadTokens:  prior.CacheReadTokens + protocol.usage["cache_read_input_tokens"],
		CacheWriteTokens: prior.CacheWriteTokens + protocol.usage["cache_creation_input_tokens"],
	}
	return nil
}

func (boundary *Boundary) setRequestActive(active bool) {
	boundary.mutex.Lock()
	defer boundary.mutex.Unlock()
	boundary.requestActive = active
}
