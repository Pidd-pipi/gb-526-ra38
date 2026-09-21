package constants

type PlanStatus string

const (
	PlanDraft            PlanStatus = "draft"
	PlanModeled          PlanStatus = "modeled"
	PlanPendingReview    PlanStatus = "pending_supervisor_review"
	PlanApprovedTraining PlanStatus = "approved_for_training"
	PlanArchived         PlanStatus = "archived"
)

// AssessmentSuperseded marks an assessment whose result was replaced by a
// planner reopen. The snapshot stays readable, but the result can no longer be
// submitted or approved. It is an assessment-only state, never a plan state.
const AssessmentSuperseded = "superseded"

var planTransitions = map[PlanStatus]map[PlanStatus]bool{
	PlanDraft:            {PlanModeled: true},
	PlanModeled:          {PlanDraft: true, PlanPendingReview: true},
	PlanPendingReview:    {PlanDraft: true, PlanApprovedTraining: true},
	PlanApprovedTraining: {PlanArchived: true},
	PlanArchived:         {},
}

func ValidPlanStatus(status PlanStatus) bool {
	_, ok := planTransitions[status]
	return ok
}

func CanTransitionPlan(from, to PlanStatus) bool {
	return planTransitions[from][to]
}

// CanReopenPlan reports whether a plan in the given status may be sent back to
// draft by a planner. Only a modeled plan or one awaiting supervisor review can
// be reopened; approved or archived plans are closed to the loop.
func CanReopenPlan(status PlanStatus) bool {
	return status == PlanModeled || status == PlanPendingReview
}

func PlanStatuses() []PlanStatus {
	return []PlanStatus{PlanDraft, PlanModeled, PlanPendingReview, PlanApprovedTraining, PlanArchived}
}
