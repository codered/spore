package companion

import "github.com/codered/spore/internal/store"

// BudgetFloor is the fewest unprompted messages a day the governor allows.
// At the floor spore sends little beyond what the user approved.
const BudgetFloor = 1.0

// Feedback kinds companion_feedback accepts.
const (
	FeedbackMore = "more"
	FeedbackLess = "less"
	FeedbackStop = "stop"
)

// FeedbackKinds is the closed set companion_feedback accepts.
var FeedbackKinds = []string{FeedbackMore, FeedbackLess, FeedbackStop}

// ApplyOutcome is the governor's response to one resolved outreach: an
// answer raises the budget a little, silence lowers it more, and "stop"
// halves it. Pending and unanswered change nothing: unanswered is not yet a
// verdict.
func ApplyOutcome(budget float64, outcome string, start int) float64 {
	switch outcome {
	case store.OutcomeReplied, store.OutcomeReacted:
		budget += 0.25
	case store.OutcomeIgnored:
		budget -= 0.5
	case store.OutcomeStop:
		budget /= 2
	}
	return clampBudget(budget, start)
}

// ApplyFeedback is the governor's response to the user saying more, less or
// stop in the companion conversation. It moves faster than ApplyOutcome:
// the user said it outright.
func ApplyFeedback(budget float64, kind string, start int) float64 {
	switch kind {
	case FeedbackMore:
		budget++
	case FeedbackLess:
		budget--
	case FeedbackStop:
		budget /= 2
	}
	return clampBudget(budget, start)
}

func clampBudget(b float64, start int) float64 {
	return max(BudgetFloor, min(b, float64(start)))
}
