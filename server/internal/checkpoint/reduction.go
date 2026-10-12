package checkpoint

// ThreadInput is one thread's full input as the pre-checkpoint workflow would
// have read it: every comment's rune length, whether the thread is resolved,
// and whether it is unchanged since the checkpoint. This is the fixture shape
// InputReduction measures against — it is deliberately independent of any live
// comment type so tests can construct it without a DB.
type ThreadInput struct {
	ThreadRootID   string
	Resolved       bool
	Changed        bool  // per Diff: true if this thread must still be expanded in full
	CommentRunes   []int // rune length of each comment in the thread, root first
	MandatoryRunes int   // runes belonging to mandatory instructions / unresolved human requests inside this thread — never eligible for reduction, reported separately
}

// ReductionResult reports the row-7 acceptance numbers for one fixture: how
// much resolved-history input a checkpoint saves versus the pre-checkpoint
// baseline, with mandatory-instruction input broken out because it is never
// eligible for the reduction (CHE-482 v2 hard stop condition).
type ReductionResult struct {
	BaselineRunes   int // full input the old workflow would read: every thread in full
	CheckpointRunes int // input actually read under the checkpoint: unchanged-resolved threads reduced to their checkpoint summary line, changed/unresolved threads read in full
	MandatoryRunes  int // mandatory-instruction runes, counted in both totals above, reported separately per the acceptance target
	SavedRunes      int
	ReductionRatio  float64 // SavedRunes / BaselineRunes
}

// checkpointSummaryRunesPerThread approximates the rendered cost of one
// ResolvedThreads line in Render's output — used only to compute the
// checkpoint-path total, not to change Render's actual behavior.
const checkpointSummaryRunesPerThread = 40

// MeasureReduction computes the row-7 numbers for one fixture: a slice of
// threads representing a long-thread issue, where some threads are resolved
// and unchanged since the checkpoint (eligible for reduction) and others are
// unresolved or changed (must be read in full, same as the pre-checkpoint
// baseline).
func MeasureReduction(threads []ThreadInput) ReductionResult {
	var res ReductionResult
	for _, t := range threads {
		full := sumRunes(t.CommentRunes)
		res.BaselineRunes += full
		res.MandatoryRunes += t.MandatoryRunes

		eligible := t.Resolved && !t.Changed
		if eligible {
			// Reduced to the checkpoint's summary line, but mandatory-instruction
			// content inside this thread is never truncated — it is carried in
			// full into the checkpoint's Obligations rendering, so its runes
			// still count toward CheckpointRunes even though the rest of the
			// thread collapses.
			res.CheckpointRunes += checkpointSummaryRunesPerThread + t.MandatoryRunes
		} else {
			res.CheckpointRunes += full
		}
	}
	res.SavedRunes = res.BaselineRunes - res.CheckpointRunes
	if res.BaselineRunes > 0 {
		res.ReductionRatio = float64(res.SavedRunes) / float64(res.BaselineRunes)
	}
	return res
}

func sumRunes(runes []int) int {
	total := 0
	for _, r := range runes {
		total += r
	}
	return total
}
