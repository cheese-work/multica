package checkpoint

import (
	"encoding/json"
	"time"
)

// Row is the plain-data shape a Checkpoint marshals to/from for storage in
// issue_checkpoint (migration 475). It exists so this package stays free of
// any db/pgtype import — the handler package converts between Row and its
// sqlc-generated params/row types, keeping the storage schema and the
// content-contract type independent of each other.
type Row struct {
	IssueRevision       int64
	CandidateID         string
	Coverage            []byte // JSON-encoded map[string]ThreadCoverage
	AcceptedDecisions   []byte // JSON-encoded []string
	Obligations         []byte // JSON-encoded []Obligation
	Blockers            []byte // JSON-encoded []string
	NextPermittedAction string
	Evidence            []byte // JSON-encoded []EvidenceLink
	ResolvedThreads     []byte // JSON-encoded []ResolvedNote
	BuiltAt             time.Time
}

// ToRow marshals a Checkpoint's content-contract fields into their JSONB
// column encodings. IssueID/AgentID are not included — those are the row's
// owner-scoping key, supplied separately by the caller at the query
// boundary, not part of the content payload.
func (cp Checkpoint) ToRow() (Row, error) {
	coverage, err := json.Marshal(cp.Coverage)
	if err != nil {
		return Row{}, err
	}
	decisions, err := json.Marshal(nonNilStrings(cp.AcceptedDecisions))
	if err != nil {
		return Row{}, err
	}
	obligations, err := json.Marshal(nonNilObligations(cp.Obligations))
	if err != nil {
		return Row{}, err
	}
	blockers, err := json.Marshal(nonNilStrings(cp.Blockers))
	if err != nil {
		return Row{}, err
	}
	evidence, err := json.Marshal(nonNilEvidence(cp.Evidence))
	if err != nil {
		return Row{}, err
	}
	resolved, err := json.Marshal(nonNilResolvedNotes(cp.ResolvedThreads))
	if err != nil {
		return Row{}, err
	}
	return Row{
		IssueRevision:       cp.IssueRev,
		CandidateID:         cp.CandidateID,
		Coverage:            coverage,
		AcceptedDecisions:   decisions,
		Obligations:         obligations,
		Blockers:            blockers,
		NextPermittedAction: cp.NextPermittedAction,
		Evidence:            evidence,
		ResolvedThreads:     resolved,
		BuiltAt:             cp.BuiltAt,
	}, nil
}

// FromRow rebuilds a Checkpoint from a stored Row plus the owner identity
// (issueID, agentID) the caller resolved the row by. Malformed JSON in any
// field fails the whole read rather than silently dropping obligations or
// blockers — a partially-decoded checkpoint could otherwise tell the caller
// "nothing outstanding" when the stored bytes actually said otherwise.
func FromRow(issueID, agentID string, row Row) (Checkpoint, error) {
	cp := Checkpoint{
		IssueID:             issueID,
		AgentID:             agentID,
		IssueRev:            row.IssueRevision,
		CandidateID:         row.CandidateID,
		NextPermittedAction: row.NextPermittedAction,
		BuiltAt:             row.BuiltAt,
	}
	if len(row.Coverage) > 0 {
		if err := json.Unmarshal(row.Coverage, &cp.Coverage); err != nil {
			return Checkpoint{}, err
		}
	}
	if len(row.AcceptedDecisions) > 0 {
		if err := json.Unmarshal(row.AcceptedDecisions, &cp.AcceptedDecisions); err != nil {
			return Checkpoint{}, err
		}
	}
	if len(row.Obligations) > 0 {
		if err := json.Unmarshal(row.Obligations, &cp.Obligations); err != nil {
			return Checkpoint{}, err
		}
	}
	if len(row.Blockers) > 0 {
		if err := json.Unmarshal(row.Blockers, &cp.Blockers); err != nil {
			return Checkpoint{}, err
		}
	}
	if len(row.Evidence) > 0 {
		if err := json.Unmarshal(row.Evidence, &cp.Evidence); err != nil {
			return Checkpoint{}, err
		}
	}
	if len(row.ResolvedThreads) > 0 {
		if err := json.Unmarshal(row.ResolvedThreads, &cp.ResolvedThreads); err != nil {
			return Checkpoint{}, err
		}
	}
	return cp, nil
}

// BuildFromScan produces the next checkpoint to persist after a claim: it
// carries forward every content-contract field the caller already decided
// (obligations, blockers, decisions, evidence, next action, resolved-thread
// notes) and replaces only the coverage cursor with what the live scan just
// observed. It performs no summarization and no diffing itself — Diff /
// EvaluateAgainstScan already did that against the PRIOR checkpoint before
// the caller decided what belongs in carryOver; this just packages the
// result for storage.
func BuildFromScan(issueID, agentID string, issueRev int64, candidateID string, live []ThreadCoverage, carryOver Checkpoint) Checkpoint {
	coverage := make(map[string]ThreadCoverage, len(live))
	for _, lt := range live {
		coverage[lt.ThreadRootID] = lt
	}
	return Checkpoint{
		IssueID:             issueID,
		AgentID:             agentID,
		IssueRev:            issueRev,
		CandidateID:         candidateID,
		Coverage:            coverage,
		AcceptedDecisions:   carryOver.AcceptedDecisions,
		Obligations:         carryOver.Obligations,
		Blockers:            carryOver.Blockers,
		NextPermittedAction: carryOver.NextPermittedAction,
		Evidence:            carryOver.Evidence,
		ResolvedThreads:     carryOver.ResolvedThreads,
		BuiltAt:             time.Now().UTC(),
	}
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilObligations(s []Obligation) []Obligation {
	if s == nil {
		return []Obligation{}
	}
	return s
}

func nonNilEvidence(s []EvidenceLink) []EvidenceLink {
	if s == nil {
		return []EvidenceLink{}
	}
	return s
}

func nonNilResolvedNotes(s []ResolvedNote) []ResolvedNote {
	if s == nil {
		return []ResolvedNote{}
	}
	return s
}
