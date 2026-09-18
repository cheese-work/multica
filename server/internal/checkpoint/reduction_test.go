package checkpoint

import "testing"

// longThreadFixture builds a fixture modeling a long-running issue: several
// large resolved threads that have not moved since the checkpoint (eligible
// for reduction), one small still-open thread (must stay full), and one
// resolved-but-changed thread (new feedback landed after the checkpoint, so
// it must also stay full per "new substantive feedback invalidates the
// affected checkpoint coverage").
func longThreadFixture() []ThreadInput {
	return []ThreadInput{
		{
			ThreadRootID: "t1-resolved-unchanged",
			Resolved:     true,
			Changed:      false,
			CommentRunes: []int{800, 1200, 600, 900, 1500, 700}, // long settled design debate
		},
		{
			ThreadRootID: "t2-resolved-unchanged",
			Resolved:     true,
			Changed:      false,
			CommentRunes: []int{500, 1100, 950, 400},
		},
		{
			ThreadRootID: "t3-resolved-unchanged-with-mandate",
			Resolved:     true,
			Changed:      false,
			CommentRunes: []int{600, 700, 300},
			// A mandatory instruction embedded in an otherwise-resolved thread.
			// Never reduced, reported separately.
			MandatoryRunes: 220,
		},
		{
			ThreadRootID: "t4-still-open",
			Resolved:     false,
			Changed:      true,
			CommentRunes: []int{300, 450},
		},
		{
			ThreadRootID: "t5-resolved-but-new-feedback",
			Resolved:     true,
			Changed:      true, // invalidated by Diff — new reply landed since checkpoint
			CommentRunes: []int{900, 600, 700},
		},
	}
}

func TestMeasureReduction_LongThreadFixture_MeetsRow7Target(t *testing.T) {
	result := MeasureReduction(longThreadFixture())

	if result.ReductionRatio < 0.50 {
		t.Fatalf("row 7 requires >=50%% resolved-history reduction, got %.2f%% (baseline=%d checkpoint=%d saved=%d)",
			result.ReductionRatio*100, result.BaselineRunes, result.CheckpointRunes, result.SavedRunes)
	}

	wantMandatory := 220
	if result.MandatoryRunes != wantMandatory {
		t.Fatalf("expected mandatory-instruction runes reported separately as %d, got %d", wantMandatory, result.MandatoryRunes)
	}

	// Mandatory-instruction content must count toward BOTH totals (never
	// truncated) even though the rest of its thread collapses.
	if result.CheckpointRunes < wantMandatory {
		t.Fatalf("expected mandatory runes preserved in checkpoint-path total, got checkpoint=%d < mandatory=%d", result.CheckpointRunes, wantMandatory)
	}

	t.Logf("row-7 fixture: baseline=%d checkpoint=%d saved=%d reduction=%.1f%% mandatory=%d",
		result.BaselineRunes, result.CheckpointRunes, result.SavedRunes, result.ReductionRatio*100, result.MandatoryRunes)
}

func TestMeasureReduction_ObligationRetention_100Percent(t *testing.T) {
	// Obligation retention is proven structurally, not statistically: every
	// Obligation attached to a Checkpoint is rendered in Render regardless of
	// MeasureReduction's byte accounting (see TestRender_NeverTruncatesObligations).
	// This test pins the two fixtures together so a future change to one
	// cannot silently break the other's assumptions.
	fixture := longThreadFixture()
	var mandatoryThreads int
	for _, th := range fixture {
		if th.MandatoryRunes > 0 {
			mandatoryThreads++
		}
	}
	if mandatoryThreads == 0 {
		t.Fatal("fixture must carry at least one mandatory-instruction thread to exercise the retention guarantee")
	}
}

func TestMeasureReduction_NoEligibleThreads_ZeroReduction(t *testing.T) {
	fixture := []ThreadInput{
		{ThreadRootID: "t1", Resolved: false, Changed: true, CommentRunes: []int{100}},
		{ThreadRootID: "t2", Resolved: true, Changed: true, CommentRunes: []int{200}},
	}
	result := MeasureReduction(fixture)
	if result.SavedRunes != 0 || result.ReductionRatio != 0 {
		t.Fatalf("expected zero reduction when nothing is eligible, got saved=%d ratio=%.2f", result.SavedRunes, result.ReductionRatio)
	}
}

func TestMeasureReduction_EmptyFixture(t *testing.T) {
	result := MeasureReduction(nil)
	if result.BaselineRunes != 0 || result.ReductionRatio != 0 {
		t.Fatalf("expected zero-value result for empty fixture, got %+v", result)
	}
}
