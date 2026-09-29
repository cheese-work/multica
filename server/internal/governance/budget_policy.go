package governance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrBudgetPolicyUnavailable = errors.New("governance deployment budget policy is unavailable")
	ErrBudgetPolicyInvalid     = errors.New("governance deployment budget policy is invalid")
	ErrBudgetPolicyCap         = errors.New("governance budget setting exceeds deployment policy")
	ErrBudgetPolicyVersion     = errors.New("governance workspace pricing policy version does not match deployment policy")
)

type DeploymentBudgetPolicy struct {
	Version                        string `json:"version"`
	Provider                       string `json:"provider"`
	Model                          string `json:"model"`
	InputMicroUSDPerMillionTokens  int64  `json:"input_micro_usd_per_million_tokens"`
	OutputMicroUSDPerMillionTokens int64  `json:"output_micro_usd_per_million_tokens"`
	MaxAttemptCostMicroUSD         int64  `json:"max_attempt_cost_micro_usd"`
	MaxWindowSpendMicroUSD         int64  `json:"max_window_spend_micro_usd"`
	MaxWorkspaceSpendMicroUSD      int64  `json:"max_workspace_spend_micro_usd"`
	MaxWindowSeconds               int64  `json:"max_window_seconds"`
	MaxEvaluationsPerCase          int64  `json:"max_evaluations_per_case"`
	// MaxConcurrentEvaluations bounds how many "governance-evaluation"
	// concurrency slots (governance_concurrency_guard.held_slots) this
	// workspace may hold at once, across every case. This is deliberately
	// its own deployment-owned value rather than reusing MaxEvaluationsPerCase
	// (CHE-707 review B2): MaxEvaluationsPerCase is a PER-CASE total-attempts
	// cap ("Bounds total Jev evaluations per work item", control.go), while
	// this is a workspace-wide IN-FLIGHT-AT-ONCE cap on the same shared
	// "governance-evaluation" resource guards.Reserve serializes through. A
	// workspace running many cases concurrently, each well under its own
	// per-case attempt limit, must not have one case's evaluations shed with
	// ErrConcurrencyLimit just because a low max_evaluations setting also
	// throttled the pool.
	MaxConcurrentEvaluations int64 `json:"max_concurrent_evaluations"`
}

type EffectiveBudgetLimits struct {
	RootCapMicroUSD    int64
	WindowCapMicroUSD  int64
	AttemptCapMicroUSD int64
	Window             time.Duration
}

type EvaluationBudgetInput struct {
	WorkspaceID  pgtype.UUID
	CaseID       pgtype.UUID
	AttemptID    pgtype.UUID
	ObligationID pgtype.UUID
	ControlEpoch int64
	Limits       OperatingLimits
}

func ParseDeploymentBudgetPolicy(raw string) (*DeploymentBudgetPolicy, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	var policy DeploymentBudgetPolicy
	if err := decoder.Decode(&policy); err != nil {
		return nil, ErrBudgetPolicyInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrBudgetPolicyInvalid
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &policy, nil
}

func (policy DeploymentBudgetPolicy) Validate() error {
	if strings.TrimSpace(policy.Version) == "" || policy.Version != strings.TrimSpace(policy.Version) || len(policy.Version) > 64 ||
		policy.Provider != "jev" || strings.TrimSpace(policy.Model) == "" || policy.Model != strings.TrimSpace(policy.Model) ||
		policy.InputMicroUSDPerMillionTokens <= 0 || policy.OutputMicroUSDPerMillionTokens < 0 ||
		policy.MaxAttemptCostMicroUSD <= 0 || policy.MaxWindowSpendMicroUSD <= 0 ||
		policy.MaxWorkspaceSpendMicroUSD <= 0 || policy.MaxWindowSeconds <= 0 || policy.MaxEvaluationsPerCase <= 0 ||
		policy.MaxConcurrentEvaluations <= 0 ||
		policy.MaxAttemptCostMicroUSD > policy.MaxWindowSpendMicroUSD ||
		policy.MaxWindowSpendMicroUSD > policy.MaxWorkspaceSpendMicroUSD {
		return ErrBudgetPolicyInvalid
	}
	if policy.MaxWindowSeconds > int64((time.Duration(1<<63-1))/time.Second) {
		return ErrBudgetPolicyInvalid
	}
	return nil
}

func (policy DeploymentBudgetPolicy) EffectiveLimits(settings OperatingLimits) (EffectiveBudgetLimits, error) {
	if err := policy.Validate(); err != nil {
		return EffectiveBudgetLimits{}, err
	}
	if !positiveLimit(settings.AdmissionWindowSeconds) || !positiveLimit(settings.WorkspaceSpendCapMicroUSD) || !positiveLimit(settings.MaxEvaluations) {
		return EffectiveBudgetLimits{}, ErrBudgetInput
	}
	if settings.PricingPolicy == nil || settings.PricingPolicy.Version != policy.Version {
		return EffectiveBudgetLimits{}, ErrBudgetPolicyVersion
	}
	if *settings.AdmissionWindowSeconds > policy.MaxWindowSeconds || *settings.WorkspaceSpendCapMicroUSD > policy.MaxWindowSpendMicroUSD ||
		*settings.MaxEvaluations > policy.MaxEvaluationsPerCase || policy.MaxAttemptCostMicroUSD > *settings.WorkspaceSpendCapMicroUSD {
		return EffectiveBudgetLimits{}, ErrBudgetPolicyCap
	}
	return EffectiveBudgetLimits{
		RootCapMicroUSD:    policy.MaxWorkspaceSpendMicroUSD,
		WindowCapMicroUSD:  *settings.WorkspaceSpendCapMicroUSD,
		AttemptCapMicroUSD: policy.MaxAttemptCostMicroUSD,
		Window:             time.Duration(*settings.AdmissionWindowSeconds) * time.Second,
	}, nil
}

func (service *BudgetService) ReserveEvaluation(ctx context.Context, policy *DeploymentBudgetPolicy, input EvaluationBudgetInput) (BudgetReservationResult, error) {
	if policy == nil {
		return BudgetReservationResult{}, ErrBudgetPolicyUnavailable
	}
	if service == nil {
		return BudgetReservationResult{}, ErrBudgetInput
	}
	limits, err := policy.EffectiveLimits(input.Limits)
	if err != nil {
		return BudgetReservationResult{}, err
	}
	windowStart := service.clock.Now().UTC().Truncate(limits.Window)
	reservationID := input.AttemptID
	return service.Reserve(ctx, BudgetReserveCommand{
		WorkspaceID:            input.WorkspaceID,
		ReservationID:          reservationID,
		BudgetRootID:           input.WorkspaceID,
		CaseID:                 input.CaseID,
		AttemptID:              input.AttemptID,
		ObligationID:           input.ObligationID,
		Resource:               "governance-evaluation",
		ControlEpoch:           input.ControlEpoch,
		WindowStart:            windowStart,
		WindowEnd:              windowStart.Add(limits.Window),
		RootCapMicroUSD:        limits.RootCapMicroUSD,
		WindowCapMicroUSD:      limits.WindowCapMicroUSD,
		MaxAttemptCostMicroUSD: limits.AttemptCapMicroUSD,
		MaxEvaluationsPerCase:  *input.Limits.MaxEvaluations,
		PolicyVersion:          policy.Version,
		RetryPolicyBounded:     true,
		SlotLimit:              policy.MaxConcurrentEvaluations,
		EventKey:               "reserve:" + policy.Version + ":" + reservationID.String(),
	})
}

func (service *BudgetService) SettleEvaluation(ctx context.Context, policy *DeploymentBudgetPolicy, reservation BudgetReservationResult, receiptID string, inputTokens, outputTokens int64, usageKnown bool) error {
	if service == nil || policy == nil {
		return ErrBudgetPolicyUnavailable
	}
	current := reservation.Reservation
	settledKnown := usageKnown
	var amount int64
	var usageErr error
	if settledKnown {
		amount, usageErr = policy.CostMicroUSD(inputTokens, outputTokens)
		if usageErr != nil {
			settledKnown = false
		} else if amount > current.TotalCapMicroUsd {
			settledKnown = false
			usageErr = ErrBudgetLimit
		}
	}
	if settledKnown && amount > 0 {
		debited, err := service.Debit(ctx, BudgetDebitCommand{
			WorkspaceID: current.WorkspaceID, ReservationID: current.ReservationID,
			ExpectedRevision: current.Revision, EventKey: "debit:" + policy.Version + ":" + current.ReservationID.String(),
			AmountMicroUSD: amount,
		})
		if err != nil {
			return err
		}
		current = debited.Reservation
	}
	_, settleErr := service.Settle(ctx, BudgetSettleCommand{
		WorkspaceID: current.WorkspaceID, ReservationID: current.ReservationID,
		ExpectedRevision: current.Revision, EventKey: "settle:" + policy.Version + ":" + current.ReservationID.String(),
		ReceiptID: receiptID, UsageKnown: settledKnown, TerminationKnown: true, UsageMicroUSD: amount,
	})
	if settleErr != nil {
		return settleErr
	}
	return usageErr
}

func (policy DeploymentBudgetPolicy) CostMicroUSD(inputTokens, outputTokens int64) (int64, error) {
	if err := policy.Validate(); err != nil || inputTokens < 0 || outputTokens < 0 {
		return 0, ErrBudgetInput
	}
	inputCost, ok := tokenCostMicroUSD(inputTokens, policy.InputMicroUSDPerMillionTokens)
	if !ok {
		return 0, ErrBudgetOverflow
	}
	outputCost, ok := tokenCostMicroUSD(outputTokens, policy.OutputMicroUSDPerMillionTokens)
	if !ok || inputCost > int64(^uint64(0)>>1)-outputCost {
		return 0, ErrBudgetOverflow
	}
	return inputCost + outputCost, nil
}

func tokenCostMicroUSD(tokens, rate int64) (int64, bool) {
	product := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(rate))
	product.Add(product, big.NewInt(999_999))
	product.Div(product, big.NewInt(1_000_000))
	if !product.IsInt64() {
		return 0, false
	}
	return product.Int64(), true
}
