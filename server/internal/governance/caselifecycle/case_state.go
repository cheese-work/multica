package caselifecycle

type CaseState string

const (
	CaseCaptured          CaseState = "captured"
	CaseEvidenceReady     CaseState = "evidence_ready"
	CaseJevEvaluating     CaseState = "jev_evaluating"
	CaseAgentEscalation   CaseState = "agent_escalation"
	CaseAgentAttempt      CaseState = "agent_attempt"
	CaseNextAttempt       CaseState = "next_attempt"
	CaseCorrectionPending CaseState = "correction_pending"
	CaseExecuting         CaseState = "executing"
	CaseRefreshing        CaseState = "refreshing"
	CaseHumanReview       CaseState = "human_review"
	CaseAbstained         CaseState = "abstained"
	CaseResolved          CaseState = "resolved"
	CaseDismissed         CaseState = "dismissed"
	CaseInvalidated       CaseState = "invalidated"
	CaseFailed            CaseState = "failed"
	CaseParked            CaseState = "parked"
)

type CaseReason string

const (
	ReasonEvidenceCaptured        CaseReason = "evidence_captured"
	ReasonEvaluationStarted       CaseReason = "evaluation_started"
	ReasonQualifiedProposal       CaseReason = "qualified_proposal"
	ReasonUncertainClassification CaseReason = "uncertain_classification"
	ReasonNoViolation             CaseReason = "no_violation"
	ReasonEligibleCandidate       CaseReason = "eligible_candidate"
	ReasonNoAdmissibleRoute       CaseReason = "no_admissible_route"
	ReasonValidProposal           CaseReason = "valid_proposal"
	ReasonUnqualifiedAttempt      CaseReason = "unqualified_attempt"
	ReasonRemainingEligibleSlot   CaseReason = "remaining_eligible_slot"
	ReasonAttemptsExhausted       CaseReason = "attempts_exhausted"
	ReasonGuardsPassed            CaseReason = "guards_passed"
	ReasonCoveredMutation         CaseReason = "covered_mutation"
	ReasonHumanApproval           CaseReason = "human_approval"
	ReasonHumanDismissal          CaseReason = "human_dismissal"
	ReasonAgeOnlyExpiry           CaseReason = "age_only_expiry"
	ReasonFreshEvidence           CaseReason = "fresh_evidence"
	ReasonRefreshBudgetExhausted  CaseReason = "refresh_budget_exhausted"
	// ReasonFreshEvidenceForHuman returns a refreshed human_review case to the
	// human; fresh evidence alone never lets it skip the human disposition.
	ReasonFreshEvidenceForHuman CaseReason = "fresh_evidence_for_human"
	ReasonDisabled              CaseReason = "disabled"
	ReasonConfigMissing         CaseReason = "configuration_missing"
	ReasonMaterialChanged       CaseReason = "material_changed"
	ReasonInfrastructureFailure CaseReason = "infrastructure_failure"
)

func (state CaseState) IsTerminal() bool {
	switch state {
	case CaseAbstained, CaseResolved, CaseDismissed, CaseInvalidated, CaseFailed:
		return true
	default:
		return false
	}
}

func CanTransition(from, to CaseState) bool {
	if !validCaseState(from) || !validCaseState(to) || from == to || from.IsTerminal() {
		return false
	}
	if to == CaseInvalidated {
		return true
	}
	if from == CaseParked {
		return false
	}
	if to == CaseFailed || to == CaseParked {
		return true
	}
	switch from {
	case CaseCaptured:
		return to == CaseEvidenceReady
	case CaseEvidenceReady:
		return to == CaseJevEvaluating
	case CaseJevEvaluating:
		return to == CaseCorrectionPending || to == CaseAgentEscalation || to == CaseAbstained
	case CaseAgentEscalation:
		return to == CaseAgentAttempt || to == CaseHumanReview
	case CaseAgentAttempt:
		return to == CaseCorrectionPending || to == CaseNextAttempt
	case CaseNextAttempt:
		return to == CaseAgentAttempt || to == CaseHumanReview
	case CaseCorrectionPending:
		return to == CaseExecuting || to == CaseRefreshing
	case CaseExecuting:
		return to == CaseResolved
	case CaseRefreshing:
		return to == CaseEvidenceReady || to == CaseHumanReview
	case CaseHumanReview:
		return to == CaseCorrectionPending || to == CaseDismissed || to == CaseRefreshing
	default:
		return false
	}
}

func CanTransitionReason(from, to CaseState, reason CaseReason) bool {
	if !CanTransition(from, to) {
		return false
	}
	switch {
	case to == CaseParked:
		return reason == ReasonDisabled || reason == ReasonConfigMissing
	case to == CaseInvalidated:
		return reason == ReasonMaterialChanged
	case to == CaseFailed:
		return reason == ReasonInfrastructureFailure
	}
	switch from {
	case CaseCaptured:
		return reason == ReasonEvidenceCaptured
	case CaseEvidenceReady:
		return reason == ReasonEvaluationStarted
	case CaseJevEvaluating:
		switch to {
		case CaseCorrectionPending:
			return reason == ReasonQualifiedProposal
		case CaseAgentEscalation:
			return reason == ReasonUncertainClassification
		case CaseAbstained:
			return reason == ReasonNoViolation
		}
	case CaseAgentEscalation:
		if to == CaseAgentAttempt {
			return reason == ReasonEligibleCandidate
		}
		return reason == ReasonNoAdmissibleRoute
	case CaseAgentAttempt:
		if to == CaseCorrectionPending {
			return reason == ReasonValidProposal
		}
		return reason == ReasonUnqualifiedAttempt
	case CaseNextAttempt:
		if to == CaseAgentAttempt {
			return reason == ReasonRemainingEligibleSlot
		}
		return reason == ReasonAttemptsExhausted
	case CaseCorrectionPending:
		if to == CaseExecuting {
			return reason == ReasonGuardsPassed
		}
		return reason == ReasonAgeOnlyExpiry
	case CaseExecuting:
		return reason == ReasonCoveredMutation
	case CaseRefreshing:
		if to == CaseEvidenceReady {
			return reason == ReasonFreshEvidence
		}
		return reason == ReasonRefreshBudgetExhausted || reason == ReasonFreshEvidenceForHuman
	case CaseHumanReview:
		switch to {
		case CaseCorrectionPending:
			return reason == ReasonHumanApproval
		case CaseRefreshing:
			return reason == ReasonAgeOnlyExpiry
		}
		return reason == ReasonHumanDismissal
	default:
		return false
	}
	return false
}

func validCaseReason(reason CaseReason) bool {
	switch reason {
	case ReasonEvidenceCaptured, ReasonEvaluationStarted, ReasonQualifiedProposal,
		ReasonUncertainClassification, ReasonNoViolation, ReasonEligibleCandidate,
		ReasonNoAdmissibleRoute, ReasonValidProposal, ReasonUnqualifiedAttempt,
		ReasonRemainingEligibleSlot, ReasonAttemptsExhausted, ReasonGuardsPassed,
		ReasonCoveredMutation, ReasonHumanApproval, ReasonHumanDismissal,
		ReasonAgeOnlyExpiry, ReasonFreshEvidence, ReasonRefreshBudgetExhausted, ReasonFreshEvidenceForHuman,
		ReasonDisabled, ReasonConfigMissing, ReasonMaterialChanged, ReasonInfrastructureFailure:
		return true
	default:
		return false
	}
}

func validCaseState(state CaseState) bool {
	switch state {
	case CaseCaptured, CaseEvidenceReady, CaseJevEvaluating, CaseAgentEscalation,
		CaseAgentAttempt, CaseNextAttempt, CaseCorrectionPending, CaseExecuting,
		CaseRefreshing, CaseHumanReview, CaseAbstained, CaseResolved, CaseDismissed,
		CaseInvalidated, CaseFailed, CaseParked:
		return true
	default:
		return false
	}
}
