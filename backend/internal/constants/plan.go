package constants

type PlanStatus string

const (
	PlanDraft            PlanStatus = "draft"
	PlanModeled          PlanStatus = "modeled"
	PlanPendingReview    PlanStatus = "pending_supervisor_review"
	PlanApprovedTraining PlanStatus = "approved_for_training"
	PlanArchived         PlanStatus = "archived"
)

// AssessmentSuperseded marks a previously current assessment whose plan was
// reopened in the same transaction. The snapshot stays readable but the
// result can never be submitted or approved again.
const AssessmentSuperseded PlanStatus = "superseded"

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

func PlanStatuses() []PlanStatus {
	return []PlanStatus{PlanDraft, PlanModeled, PlanPendingReview, PlanApprovedTraining, PlanArchived}
}

// CanReopenPlan is the planner-side reopen-and-replace guard: only plans that
// already carry an immutable model result (modeled) or that are waiting for
// supervisor review can be carried back to draft with a reason.
func CanReopenPlan(status PlanStatus) bool {
	return status == PlanModeled || status == PlanPendingReview
}
