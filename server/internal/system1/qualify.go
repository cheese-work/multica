package system1

import "time"

// The CHE-3239 go-live bar, unchanged (CHE-4383 B5): 7 days in the epoch, a
// denominator of at least 20 counted cases, agreement of at least 90%.
const (
	QualifyMinAge         = 7 * 24 * time.Hour
	QualifyMinCases       = 20
	QualifyMinAgreement   = 0.9
	InsufficientDataAfter = 28 * 24 * time.Hour
)

const (
	StatusCollecting       = "collecting"
	StatusInsufficientData = "insufficient_data"
	StatusQualified        = "qualified"
	StatusDemoted          = "demoted"
)

// Status of one epoch. denominator is the counted cases, which is also the
// agreement denominator: a labelled abstention is not one.
func Status(age time.Duration, denominator, agreed int64, demoted bool) string {
	switch {
	case demoted:
		return StatusDemoted
	case age >= QualifyMinAge && denominator >= QualifyMinCases && float64(agreed)/float64(denominator) >= QualifyMinAgreement:
		return StatusQualified
	case age >= InsufficientDataAfter && denominator < QualifyMinCases:
		return StatusInsufficientData
	default:
		return StatusCollecting
	}
}
